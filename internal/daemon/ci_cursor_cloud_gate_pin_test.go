package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

func cloudGateHarness(t *testing.T) *ciPollerHarness {
	t.Helper()
	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	t.Setenv("CURSOR_API_KEY", "cursor-test-secret")
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	h.Cfg.AllowCursorCloud = true
	h.stubProcessPRGit()
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "range-prompt", nil
	}
	return h
}

func TestCIMatrixCursorCloudWithoutOptInDoesNotLaunch(t *testing.T) {
	h := cloudGateHarness(t)
	h.Cfg.CI.Agents = []string{"cursor-cloud"}
	h.Cfg.CI.ReviewTypes = []string{"default"}
	const head = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number: 21, HeadRefOid: head, HeadRefName: "feature", BaseRefName: "main",
		HeadRepo: "acme/api", BaseRepo: "acme/api",
	}, h.Cfg)
	require.NoError(t, err)
	assert.False(t, h.hasPanel(t, "acme/api", 21, head))
}

func TestCINamedPanelCursorCloudWithoutOptInDoesNotLaunch(t *testing.T) {
	h := cloudGateHarness(t)
	h.Cfg.CI.Panel = "ci"
	h.Cfg.Review = config.ReviewConfig{
		Subagents: map[string]config.SubagentSpec{
			"cloud": {Agent: agent.CursorCloudHost, ReviewType: "review"},
		},
		Panels: map[string]config.PanelSpec{
			"ci": {Members: []string{"cloud"}, SynthesisAgent: "test"},
		},
	}
	const head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number: 22, HeadRefOid: head, HeadRefName: "feature", BaseRefName: "main",
		HeadRepo: "acme/api", BaseRepo: "acme/api",
	}, h.Cfg)
	require.NoError(t, err)
	assert.False(t, h.hasPanel(t, "acme/api", 22, head))
}

func TestCINamedPanelForkCursorCloudDoesNotLaunch(t *testing.T) {
	h := cloudGateHarness(t)
	h.Cfg.CI.Panel = "ci"
	h.Cfg.Review = config.ReviewConfig{
		Subagents: map[string]config.SubagentSpec{
			"cloud": {Agent: agent.CursorCloudHost, ReviewType: "review"},
		},
		Panels: map[string]config.PanelSpec{
			"ci": {Members: []string{"cloud"}, SynthesisAgent: "test"},
		},
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const head = "ffffffffffffffffffffffffffffffffffffffff"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number: 26, HeadRefOid: head, HeadRefName: "feature", BaseRefName: "main",
		HeadRepo: "fork/api", BaseRepo: "acme/api",
	}, h.Cfg)
	require.NoError(t, err)
	assert.False(t, h.hasPanel(t, "acme/api", 26, head))
	assert.Contains(t, logs.String(), `head repo "fork/api" is not base repo "acme/api"`)
}

func TestCIForkMatrixCursorCloudDoesNotLaunch(t *testing.T) {
	h := cloudGateHarness(t)
	h.Cfg.CI.Agents = []string{agent.CursorCloudHost}
	h.Cfg.CI.ReviewTypes = []string{"default"}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const head = "cccccccccccccccccccccccccccccccccccccccc"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number: 23, HeadRefOid: head, HeadRefName: "feature", BaseRefName: "main",
		HeadRepo: "fork/api", BaseRepo: "acme/api",
	}, h.Cfg)
	require.NoError(t, err)
	assert.False(t, h.hasPanel(t, "acme/api", 23, head))
	assert.Contains(t, logs.String(), `head repo "fork/api" is not base repo "acme/api"`)
}

func TestCICursorCloudUnknownIdentityDoesNotLaunch(t *testing.T) {
	cases := []struct {
		name     string
		headRepo string
		baseRepo string
	}{
		{name: "both empty"},
		{name: "deleted fork", baseRepo: "acme/api"},
		{name: "base empty", headRepo: "fork/api"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := cloudGateHarness(t)
			h.Poller.loadRepoConfigFn = func(string) (ciRepoConfigSource, error) {
				return ciRepoConfigSource{Config: &config.RepoConfig{
					CI: config.RepoCIConfig{Host: agent.CursorCloudHost},
				}}, nil
			}
			h.Cfg.CI.Agents = []string{"codex"}
			h.Cfg.CI.ReviewTypes = []string{"default"}
			var logs bytes.Buffer
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			head := fmt.Sprintf("%040d", i+1)
			err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
				Number: 24 + i, HeadRefOid: head, BaseRefName: "main",
				HeadRepo: tc.headRepo, BaseRepo: tc.baseRepo,
			}, h.Cfg)
			require.NoError(t, err)
			assert.False(t, h.hasPanel(t, "acme/api", 24+i, head))
			assert.Contains(t, logs.String(), "head and base repositories are unknown")
		})
	}
}

func TestCIDeferredRetryOfForkDoesNotLaunchCursorCloud(t *testing.T) {
	h := cloudGateHarness(t)
	h.Poller.loadRepoConfigFn = func(string) (ciRepoConfigSource, error) {
		return ciRepoConfigSource{Config: &config.RepoConfig{
			CI: config.RepoCIConfig{Host: agent.CursorCloudHost},
		}}, nil
	}
	h.Cfg.CI.Agents = []string{"codex"}
	h.Cfg.CI.ReviewTypes = []string{"default"}

	const head = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const prNum = 25
	now := time.Now()
	created, err := h.DB.ReserveReviewAttempt("acme/api", prNum, head, now.Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, h.DB.DeferReviewAttempt(
		"acme/api", prNum, head, "transient", "provider unavailable",
		testUUIDPtr("old-run"), now.Add(-time.Minute), false,
	))
	h.Poller.prPostTargetFn = func(context.Context, string, int) (panelPostTarget, error) {
		return panelPostTarget{
			Open: true, HeadSHA: head, HeadRefName: "feature", BaseRefName: "main",
			HeadRepo: "fork/api", BaseRepo: "acme/api",
		}, nil
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	require.NoError(t, h.Poller.retryDueReviewAttempts(context.Background(), "acme/api", nil, h.Cfg))
	assert.False(t, h.hasPanel(t, "acme/api", prNum, head))
	assert.Contains(t, logs.String(), `head repo "fork/api" is not base repo "acme/api"`)
}

func TestCIPanelPostTargetCarriesRepoIdentities(t *testing.T) {
	const body = `{
		"number": 7,
		"state": "open",
		"title": "fork",
		"head": {"ref": "feature", "sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "repo": {"full_name": "fork/api"}},
		"base": {"ref": "main", "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "repo": {"full_name": "acme/api"}},
		"user": {"login": "alice"}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/repos/acme/api/pulls/7" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	h.Poller.githubAPIURL = srv.URL + "/"
	got, err := h.Poller.panelPostTarget(context.Background(), "acme/api", 7)
	require.NoError(t, err)
	assert.Equal(t, "fork/api", got.HeadRepo)
	assert.Equal(t, "acme/api", got.BaseRepo)
	assert.True(t, got.Open)
}
