package goalreview

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/testutil"
)

// An unavailable pi with review_backup_agent=auto must hop to Claude, not
// pick Codex and then fail ValidateAgent. Mutant pin: if ResolveAgent still
// calls GetPreferredOrBackup without the goal-capable filter, this fails.
func TestResolveAgentAutoBackupSkipsIncapableAgents(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte(
		"review_agent = \"pi\"\nreview_backup_agent = \"auto\"\n",
	), 0o644))

	dir := t.TempDir()
	for _, name := range []string{"codex", "claude"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755))
	}
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	require.NoError(t, os.Symlink(gitPath, filepath.Join(dir, "git")))
	t.Setenv("PATH", dir)
	t.Setenv("ROBOREV_HOST_AGENT", "")
	require.NoError(t, os.Unsetenv("ROBOREV_HOST_AGENT"))

	a, _, err := ResolveAgent(repo.Path(), config.DefaultConfig(), AgentOptions{})
	require.NoError(t, err)
	assert.Equal(t, "claude-code", a.Name())
}
