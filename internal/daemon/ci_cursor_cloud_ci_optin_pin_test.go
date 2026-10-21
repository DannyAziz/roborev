package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

func TestCIForkWithGlobalHostDoesNotLaunchCursorCloud(t *testing.T) {
	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	t.Setenv("CURSOR_API_KEY", "cursor-test-secret")
	t.Setenv("ROBOREV_DEFAULT_HOST", "cursor-cloud")
	h.Cfg.Host = agent.CursorCloudHost
	h.Cfg.CI.Agents = []string{"codex"}
	h.Cfg.CI.ReviewTypes = []string{"default"}
	h.stubProcessPRGit()
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "local-prompt", nil
	}

	const head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number:      11,
		HeadRefOid:  head,
		HeadRefName: "feature",
		BaseRefName: "main",
		HeadRepo:    "fork/api",
		BaseRepo:    "acme/api",
	}, h.Cfg)
	require.NoError(t, err)

	members := h.panelMembers(t, "acme/api", 11, head)
	require.NotEmpty(t, members)
	for _, member := range members {
		assert.NotEqual(t, agent.CursorCloudHost, member.Agent)
	}
}

func TestCISameRepoWithoutCIOptInDoesNotLaunchCursorCloud(t *testing.T) {
	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	t.Setenv("CURSOR_API_KEY", "cursor-test-secret")
	t.Setenv("ROBOREV_DEFAULT_HOST", "cursor-cloud")
	h.Cfg.Host = agent.CursorCloudHost
	h.Cfg.CI.Agents = []string{"codex"}
	h.Cfg.CI.ReviewTypes = []string{"default"}
	h.stubProcessPRGit()
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "local-prompt", nil
	}

	const head = "cccccccccccccccccccccccccccccccccccccccc"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number:      12,
		HeadRefOid:  head,
		HeadRefName: "feature",
		BaseRefName: "main",
		HeadRepo:    "acme/api",
		BaseRepo:    "acme/api",
	}, h.Cfg)
	require.NoError(t, err)

	members := h.panelMembers(t, "acme/api", 12, head)
	require.NotEmpty(t, members)
	for _, member := range members {
		assert.NotEqual(t, agent.CursorCloudHost, member.Agent)
	}
}

func TestCISameRepoCIOptInLaunchesCursorCloud(t *testing.T) {
	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	t.Setenv("CURSOR_API_KEY", "cursor-test-secret")
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	require.NoError(t, os.WriteFile(
		filepath.Join(h.RepoPath, ".roborev.toml"),
		[]byte("[ci]\nhost = \"cursor-cloud\"\n"),
		0o644,
	))
	h.Cfg.CI.Agents = []string{"codex", "gemini"}
	h.Cfg.CI.ReviewTypes = []string{"security", "default"}
	h.stubProcessPRGit()
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "range-prompt", nil
	}

	const head = "dddddddddddddddddddddddddddddddddddddddd"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number:      13,
		HeadRefOid:  head,
		HeadRefName: "feature",
		BaseRefName: "main",
		HeadRepo:    "acme/api",
		BaseRepo:    "acme/api",
	}, h.Cfg)
	require.NoError(t, err)

	members := h.panelMembers(t, "acme/api", 13, head)
	require.Len(t, members, 1)
	assert.Equal(t, agent.CursorCloudHost, members[0].Agent)
	assert.Equal(t, head, members[0].GitRef)
	assert.NotContains(t, members[0].GitRef, "..")
}

func TestCIOptInForkRefusesCursorCloud(t *testing.T) {
	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	t.Setenv("CURSOR_API_KEY", "cursor-test-secret")
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	require.NoError(t, os.WriteFile(
		filepath.Join(h.RepoPath, ".roborev.toml"),
		[]byte("[ci]\nhost = \"cursor-cloud\"\n"),
		0o644,
	))
	h.Cfg.CI.Agents = []string{"codex"}
	h.Cfg.CI.ReviewTypes = []string{"default"}
	h.stubProcessPRGit()
	h.Poller.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "range-prompt", nil
	}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const head = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	err := h.Poller.processPR(context.Background(), "acme/api", ghPR{
		Number:      14,
		HeadRefOid:  head,
		HeadRefName: "feature",
		BaseRefName: "main",
		HeadRepo:    "fork/api",
		BaseRepo:    "acme/api",
	}, h.Cfg)
	require.NoError(t, err)
	assert.False(t, h.hasPanel(t, "acme/api", 14, head))
	assert.Contains(t, logs.String(), "refusing cursor-cloud for acme/api#14")
	assert.Contains(t, logs.String(), `head repo "fork/api" is not base repo "acme/api"`)
}
