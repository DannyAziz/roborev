package agent

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
)

// Repository config alone cannot select cursor-cloud; the global
// allow_cursor_cloud must opt in. The --host flag, a CLI agent, and global
// settings are not gated.
func TestRepoCursorCloudNeedsAllowCursorCloud(t *testing.T) {
	unsetCursorCloudEnv(t)
	t.Setenv(cursorcloud.EnvAPIKey, cursorCloudTestSecret)

	notAllowed := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		_, ok := errors.AsType[*CursorCloudNotAllowedError](err)
		assert.True(t, ok, "got %T: %v", err, err)
		assert.Contains(t, err.Error(), "allow_cursor_cloud")
	}
	allow := config.DefaultConfig()
	allow.AllowCursorCloud = true

	t.Run("repo host", func(t *testing.T) {
		_, err := ReviewHost("", CursorCloudHost, "", false)
		notAllowed(t, err)
		host, err := ReviewHost("", CursorCloudHost, "", true)
		require.NoError(t, err)
		assert.Equal(t, CursorCloudHost, host)
	})

	for name, repo := range map[string]*config.RepoConfig{
		"repo agent":           {Agent: CursorCloudHost},
		"repo review_agent":    {ReviewAgent: CursorCloudHost},
		"repo review_agent_lv": {ReviewAgentThorough: CursorCloudHost},
		"repo backup_agent":    {BackupAgent: CursorCloudHost},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveWorkflowConfigFromConfig("", repo, config.DefaultConfig(), "review", "thorough")
			notAllowed(t, err)
			_, err = ResolveWorkflowConfigFromConfig("", repo, allow, "review", "thorough")
			require.NoError(t, err)
		})
	}

	t.Run("flag and global stay ungated", func(t *testing.T) {
		host, err := ReviewHost(CursorCloudHost, "", "", false)
		require.NoError(t, err)
		assert.Equal(t, CursorCloudHost, host)
		host, err = ReviewHost("", "", CursorCloudHost, false)
		require.NoError(t, err)
		assert.Equal(t, CursorCloudHost, host)

		res, err := ResolveWorkflowConfigFromConfig(CursorCloudHost, &config.RepoConfig{}, config.DefaultConfig(), "review", "")
		require.NoError(t, err)
		assert.Equal(t, CursorCloudHost, res.PreferredAgent)

		global := config.DefaultConfig()
		global.DefaultAgent = CursorCloudHost
		global.DefaultBackupAgent = CursorCloudHost
		res, err = ResolveWorkflowConfigFromConfig("", &config.RepoConfig{}, global, "review", "")
		require.NoError(t, err)
		assert.Equal(t, CursorCloudHost, res.PreferredAgent)
		assert.Equal(t, CursorCloudHost, res.BackupAgent)
	})
}
