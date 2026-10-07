package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	ghpkg "go.kenn.io/roborev/internal/github"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func personalHarness(t *testing.T) *ciPollerHarness {
	t.Helper()
	h := newCIPollerHarness(t, "https://github.com/acme/api.git")
	h.Cfg.CI.Enabled = true
	h.Cfg.CI.Repos = []string{"acme/api"}
	h.Cfg.CI.Personal = config.PersonalReviewConfig{Enabled: true, Reviewer: "alice", Panel: "personal", Approve: true, RequestChanges: true}
	h.Cfg.Review.Subagents = map[string]config.SubagentSpec{"reviewer": {Agent: "test", Model: "local-model", Instructions: "Personal rubric."}}
	h.Cfg.Review.Panels = map[string]config.PanelSpec{"personal": {Members: []string{"reviewer"}, SynthesisAgent: "test"}}
	h.Poller.personalActorFn = func(context.Context, string) (string, error) { return "alice", nil }
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: "sha", AuthorLogin: "author", RequestedReviewers: []string{"alice"}}, nil
	}
	h.Poller.findPersonalReviewFn = func(context.Context, string, int, ghpkg.ReviewSubmission) (int64, error) { return 0, nil }
	h.Poller.submitPersonalReviewFn = func(context.Context, string, int, ghpkg.ReviewSubmission) (int64, error) { return 42, nil }
	return h
}

func seedPersonalPanel(t *testing.T, h *ciPollerHarness) *storage.CIPanel {
	t.Helper()
	panel, synth, _ := h.seedCIPanelRun(t, "acme/api", 1, "sha", "base..sha", []jobSpec{{Agent: "test", Status: "done", Output: "No issues found."}})
	snapshot, err := json.Marshal(ciACPExecutionConfig{Personal: personalPolicySnapshot(h.Cfg), PersonalMemberCount: 1})
	require.NoError(t, err)
	_, err = h.DB.Exec(`UPDATE review_jobs SET panel_member_config_json = ? WHERE id = ?`, string(snapshot), synth.ID)
	require.NoError(t, err)
	h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
	return panel
}

func TestPersonalRequestEligibility(t *testing.T) {
	policy := config.PersonalReviewConfig{Reviewer: "alice"}
	for _, tc := range []struct {
		name     string
		pr       ghPR
		eligible bool
	}{
		{"direct", ghPR{Author: ghPRAuthor{Login: "other"}, RequestedReviewers: []string{"ALICE"}}, true},
		{"unrequested", ghPR{}, false},
		{"different reviewer", ghPR{RequestedReviewers: []string{"bob"}}, false},
		{"draft", ghPR{Draft: true, RequestedReviewers: []string{"alice"}}, false},
		{"self-authored", ghPR{Author: ghPRAuthor{Login: "Alice"}, RequestedReviewers: []string{"alice"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.eligible, personalRequestEligible(policy, tc.pr)) })
	}
	assert.False(t, personalRequestEligible(config.PersonalReviewConfig{}, ghPR{RequestedReviewers: []string{""}}))
}

func TestPersonalRoutingUsesLocalPanelAndPreservesOpenInventory(t *testing.T) {
	h := personalHarness(t)
	h.stubProcessPRGit()
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "Frozen personal prompt.", nil
	}
	h.Poller.loadRepoConfigFn = func(string) (ciRepoConfigSource, error) {
		return ciRepoConfigSource{Config: &config.RepoConfig{Review: config.ReviewConfig{Subagents: map[string]config.SubagentSpec{"reviewer": {Agent: "codex", Model: "repo-model"}}, Panels: map[string]config.PanelSpec{"personal": {Members: []string{"reviewer"}}}}}}, nil
	}
	statuses := h.CaptureCommitStatuses()
	comments := h.CaptureComments()
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
		return []ghPR{
			{Number: 1, HeadRefOid: "sha", BaseRefName: "main", RequestedReviewers: []string{"alice"}},
			{Number: 2, HeadRefOid: "unrequested", BaseRefName: "main"},
		}, nil
	}
	require.NoError(t, h.Poller.pollRepo(t.Context(), "acme/api", h.Cfg))
	members := h.panelMembers(t, "acme/api", 1, "sha")
	require.Len(t, members, 1)
	assert := assert.New(t)
	assert.Equal("test", members[0].Agent)
	assert.Equal("local-model", members[0].Model)
	assert.Contains(members[0].PanelMemberConfigJSON, "Personal rubric.")
	assert.False(h.hasPanel(t, "acme/api", 2, "unrequested"))
	assert.Empty(*statuses)
	assert.Empty(*comments)
	// Removing the reviewer does not pretend the still-open PR was closed.
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
		return []ghPR{{Number: 1, HeadRefOid: "sha", BaseRefName: "main"}}, nil
	}
	require.NoError(t, h.Poller.pollRepo(t.Context(), "acme/api", h.Cfg))
	job, err := h.DB.GetJobByID(members[0].ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusQueued, job.Status)
}

func TestPersonalApprovalPolicy(t *testing.T) {
	clean := structuredreview.Document{SchemaVersion: 2, Summary: "Clean.", Verdict: "pass", Findings: []structuredreview.Finding{}}
	cleanRaw, err := json.Marshal(clean)
	require.NoError(t, err)
	cleanMember := storage.BatchReviewResult{Status: "done", StructuredOutput: cleanRaw}
	blocking := clean
	blocking.Verdict = "fail"
	blocking.Findings = []structuredreview.Finding{{Severity: "high", Problem: "Bug.", Fix: "Fix it."}}
	for _, tc := range []struct {
		name    string
		policy  config.PersonalReviewConfig
		doc     structuredreview.Document
		members []storage.BatchReviewResult
		event   string
	}{
		{"clean opt-in", config.PersonalReviewConfig{Approve: true}, clean, []storage.BatchReviewResult{cleanMember}, "APPROVE"},
		{"no approval opt-in", config.PersonalReviewConfig{}, clean, []storage.BatchReviewResult{cleanMember}, "COMMENT"},
		{"no members", config.PersonalReviewConfig{Approve: true}, clean, nil, "COMMENT"},
		{"partial failure", config.PersonalReviewConfig{Approve: true}, clean, []storage.BatchReviewResult{cleanMember, {Status: "failed"}}, "COMMENT"},
		{"malformed member", config.PersonalReviewConfig{Approve: true}, clean, []storage.BatchReviewResult{{Status: "done", StructuredOutput: []byte(`{}`)}}, "COMMENT"},
		{"blockers opt-in", config.PersonalReviewConfig{Approve: true, RequestChanges: true}, blocking, []storage.BatchReviewResult{cleanMember}, "REQUEST_CHANGES"},
		{"blockers without opt-in", config.PersonalReviewConfig{Approve: true}, blocking, []storage.BatchReviewResult{cleanMember}, "COMMENT"},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.event, personalReviewEvent(tc.policy, tc.doc, tc.members)) })
	}
}

func TestPersonalDryRunCannotBeUpgradedAfterEnqueue(t *testing.T) {
	h := personalHarness(t)
	panel := seedPersonalPanel(t, h)
	h.Cfg.CI.Personal.Mode = "publish"
	submitted := 0
	h.Poller.submitPersonalReviewFn = func(context.Context, string, int, ghpkg.ReviewSubmission) (int64, error) { submitted++; return 42, nil }
	statuses := h.CaptureCommitStatuses()
	comments := h.CaptureComments()
	h.Poller.postPanelRun(t.Context(), panel)
	publication, err := h.DB.GetPersonalPublication(panel.PanelRunUUID)
	require.NoError(t, err)
	require.NotNil(t, publication)
	assert := assert.New(t)
	assert.Equal("preview", publication.Phase)
	assert.Contains(publication.SubmissionJSON, "APPROVE")
	assert.Zero(submitted)
	assert.Empty(*statuses)
	assert.Empty(*comments)
	assert.True(h.panelPostedAt(t, panel.ID))
}

func TestPersonalPublicationRefusesIneligibleTargets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ciPollerHarness)
	}{
		{"stale even quiet-hours", func(h *ciPollerHarness) {
			h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
				return panelPostTarget{Open: true, HeadSHA: "new-sha", RequestedReviewers: []string{"alice"}}, nil
			}
		}},
		{"unknown SHA", func(h *ciPollerHarness) {
			h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
				return panelPostTarget{Open: true, RequestedReviewers: []string{"alice"}}, nil
			}
		}},
		{"removed request", func(h *ciPollerHarness) {
			h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
				return panelPostTarget{Open: true, HeadSHA: "sha"}, nil
			}
		}},
		{"identity mismatch", func(h *ciPollerHarness) {
			h.Poller.personalActorFn = func(context.Context, string) (string, error) { return "bob", nil }
		}},
		{"skip label added", func(h *ciPollerHarness) {
			h.Cfg.CI.SkipLabels = []string{"skip-auto"}
			h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
				return panelPostTarget{Open: true, HeadSHA: "sha", RequestedReviewers: []string{"alice"}, Labels: []string{"skip-auto"}}, nil
			}
		}},
		{"CI disabled", func(h *ciPollerHarness) { h.Cfg.CI.Enabled = false }},
		{"disabled", func(h *ciPollerHarness) { h.Cfg.CI.Personal.Enabled = false }},
		{"removed from allowlist", func(h *ciPollerHarness) { h.Cfg.CI.Repos = []string{"other/project"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := personalHarness(t)
			h.Cfg.CI.Personal.Mode = "publish"
			panel := seedPersonalPanel(t, h)
			_, err := h.DB.Exec(`UPDATE ci_pr_panels SET allow_stale_post=1 WHERE id=?`, panel.ID)
			require.NoError(t, err)
			tc.change(h)
			submitted := 0
			h.Poller.submitPersonalReviewFn = func(context.Context, string, int, ghpkg.ReviewSubmission) (int64, error) { submitted++; return 42, nil }
			h.Poller.postPanelRun(t.Context(), panel)
			publication, err := h.DB.GetPersonalPublication(panel.PanelRunUUID)
			require.NoError(t, err)
			assert.Zero(t, submitted)
			assert.Nil(t, publication)
		})
	}
}

func TestPersonalAcceptedReviewReconcilesBeforeRequestEligibility(t *testing.T) {
	h := personalHarness(t)
	h.Cfg.CI.Personal.Mode = "publish"
	panel := seedPersonalPanel(t, h)
	submitted := 0
	accepted := false
	h.Poller.submitPersonalReviewFn = func(context.Context, string, int, ghpkg.ReviewSubmission) (int64, error) {
		submitted++
		accepted = true
		return 0, errors.New("response lost")
	}
	h.Poller.findPersonalReviewFn = func(context.Context, string, int, ghpkg.ReviewSubmission) (int64, error) {
		if accepted {
			return 42, nil
		}
		return 0, nil
	}
	h.Poller.postPanelRun(t.Context(), panel)
	publication, err := h.DB.GetPersonalPublication(panel.PanelRunUUID)
	require.NoError(t, err)
	require.NotNil(t, publication)
	assert.Equal(t, "uncertain", publication.Phase)
	// GitHub removes the review request after accepting a formal review. Recovery
	// still finds the accepted operation and never submits a second approval.
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{Open: true, HeadSHA: "sha"}, nil
	}
	h.Poller.postPanelRun(t.Context(), panel)
	publication, err = h.DB.GetPersonalPublication(panel.PanelRunUUID)
	require.NoError(t, err)
	assert.Equal(t, "submitted", publication.Phase)
	assert.Equal(t, int64(42), publication.ReviewID)
	assert.Equal(t, 1, submitted)
	assert.True(t, h.panelPostedAt(t, panel.ID))
}

func TestPersonalUncertainSubmissionIsNeverResent(t *testing.T) {
	h := personalHarness(t)
	h.Cfg.CI.Personal.Mode = "publish"
	panel := seedPersonalPanel(t, h)
	submitted := 0
	h.Poller.submitPersonalReviewFn = func(context.Context, string, int, ghpkg.ReviewSubmission) (int64, error) {
		submitted++
		return 0, errors.New("unavailable")
	}
	h.Poller.postPanelRun(t.Context(), panel)
	h.Poller.postPanelRun(t.Context(), panel)
	assert.Equal(t, 1, submitted)
	assert.False(t, h.panelPostedAt(t, panel.ID))
}

func TestPersonalEnqueueAndRetryRequireMatchingActorAndRequest(t *testing.T) {
	h := personalHarness(t)
	h.stubProcessPRGit()
	h.Poller.personalActorFn = func(context.Context, string) (string, error) { return "bob", nil }
	created, err := h.Poller.enqueuePanelRun(t.Context(), "acme/api", ghPR{Number: 1, HeadRefOid: "sha", RequestedReviewers: []string{"alice"}}, h.Cfg)
	require.ErrorContains(t, err, "publishing user")
	assert.False(t, created)
	created, err = h.Poller.enqueuePanelRun(t.Context(), "acme/api", ghPR{Number: 1, HeadRefOid: "sha"}, h.Cfg)
	require.NoError(t, err)
	assert.False(t, created)
	created, err = h.Poller.enqueuePanelRun(t.Context(), "other/project", ghPR{Number: 1, HeadRefOid: "sha", RequestedReviewers: []string{"alice"}}, h.Cfg)
	require.NoError(t, err)
	assert.False(t, created)
}
