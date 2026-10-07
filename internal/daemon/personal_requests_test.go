package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestPersonalObservedReRequestReviewsSameCommitAgain(t *testing.T) {
	h := personalHarness(t)
	h.stubProcessPRGit()
	h.Cfg.CI.ThrottleInterval = "0"
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "Frozen rubric.", nil
	}
	requested := true
	h.Poller.listOpenPRsFn = func(context.Context, string) ([]ghPR, error) {
		pr := ghPR{Number: 1, HeadRefOid: "sha", BaseRefName: "main"}
		if requested {
			pr.RequestedReviewers = []string{"alice"}
		}
		return []ghPR{pr}, nil
	}
	require.NoError(t, h.Poller.pollRepo(t.Context(), "acme/api", h.Cfg))
	first, err := h.DB.GetCIPanelByPRSHA("acme/api", 1, "sha")
	require.NoError(t, err)
	members, err := h.DB.GetPanelMembers(first.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	h.markJobDoneWithReview(t, members[0].ID, "test", "No issues found.")
	synth, err := h.DB.GetSynthesisJob(first.PanelRunUUID)
	require.NoError(t, err)
	require.NotNil(t, synth)
	h.completeSynthesisWithReview(t, synth.ID, "No issues found.")
	h.Poller.postPanelRun(t.Context(), first)
	require.True(t, h.panelPostedAt(t, first.ID))
	// An unchanged outstanding request cannot cause repeated reviews.
	require.NoError(t, h.Poller.pollRepo(t.Context(), "acme/api", h.Cfg))
	unchanged, err := h.DB.GetCIPanelByPRSHA("acme/api", 1, "sha")
	require.NoError(t, err)
	assert.Equal(t, first.ID, unchanged.ID)
	requested = false
	require.NoError(t, h.Poller.pollRepo(t.Context(), "acme/api", h.Cfg))
	requested = true
	require.NoError(t, h.Poller.pollRepo(t.Context(), "acme/api", h.Cfg))
	renewed, err := h.DB.GetCIPanelByPRSHA("acme/api", 1, "sha")
	require.NoError(t, err)
	assert.NotEqual(t, first.ID, renewed.ID)
	jobs, err := h.DB.GetPanelMembers(renewed.PanelRunUUID)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, storage.JobStatusQueued, jobs[0].Status)
}

func TestPersonalReRequestAfterFailedSynthesis(t *testing.T) {
	h := personalHarness(t)
	h.stubProcessPRGit()
	h.Cfg.CI.ThrottleInterval = "0"
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "Frozen rubric.", nil
	}
	panel := seedPersonalPanel(t, h)
	synth, err := h.DB.GetSynthesisJob(panel.PanelRunUUID)
	require.NoError(t, err)
	require.NotNil(t, synth)
	_, err = h.DB.Exec(`UPDATE review_jobs SET status='failed', error='synthetic failure' WHERE id=?`, synth.ID)
	require.NoError(t, err)
	require.NoError(t, h.DB.ObservePersonalRequest("acme/api", 1, "alice", true))
	h.Poller.postPanelRun(t.Context(), panel)
	require.True(t, h.panelPostedAt(t, panel.ID))
	publication, err := h.DB.GetPersonalPublication(panel.PanelRunUUID)
	require.NoError(t, err)
	require.Nil(t, publication)
	// No publication record exists, but the explicit new request must still
	// re-arm this completed failure without deleting its history.
	require.NoError(t, h.DB.ObservePersonalRequest("acme/api", 1, "alice", false))
	require.NoError(t, h.DB.ObservePersonalRequest("acme/api", 1, "alice", true))
	created, err := h.Poller.enqueuePanelRun(t.Context(), "acme/api", ghPR{Number: 1, HeadRefOid: "sha", RequestedReviewers: []string{"alice"}}, h.Cfg)
	require.NoError(t, err)
	assert.True(t, created)
}
