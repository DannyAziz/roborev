package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewTypeFlagCompletion(t *testing.T) {
	cmd := reviewCmd()

	completion, ok := cmd.GetFlagCompletionFunc("type")
	require.True(t, ok)

	got, directive := completion(cmd, nil, "")

	assert.ElementsMatch(t, []cobra.Completion{"security", "design", "lookahead", "goal"}, got)
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

func TestAgentFlagCompletionIncludesNamedACPAgents(t *testing.T) {
	env := setupConfigEnv(t,
		"[acp.goose]\ncommand = 'goose'\n",
		"[acp.foo]\ncommand = 'foo-acp'\n",
	)
	t.Chdir(env.RepoDir)
	cmd := reviewCmd()

	completion, ok := cmd.GetFlagCompletionFunc("agent")
	require.True(t, ok)
	got, directive := completion(cmd, nil, "")

	assert.Contains(t, got, cobra.Completion("acp.goose"))
	assert.Contains(t, got, cobra.Completion("acp.foo"))
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

func TestAgentFlagCompletionDiscoversRepoACPAgentFromNestedDirectory(t *testing.T) {
	env := setupConfigEnv(t, "", "[acp.goose]\ncommand = 'goose'\n")
	nested := filepath.Join(env.RepoDir, "nested", "directory")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	t.Chdir(nested)
	cmd := reviewCmd()

	completion, ok := cmd.GetFlagCompletionFunc("agent")
	require.True(t, ok)
	got, directive := completion(cmd, nil, "")

	assert.Contains(t, got, cobra.Completion("acp.goose"))
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

func TestAgentFlagCompletionUsesCommandRepoFlag(t *testing.T) {
	env := setupConfigEnv(t, "", "[acp.goose]\ncommand = 'goose'\n")
	other := setupConfigEnv(t, "", "[acp.foo]\ncommand = 'foo-acp'\n")
	t.Chdir(env.RepoDir)
	cmd := reviewCmd()
	require.NoError(t, cmd.Flags().Set("repo", other.RepoDir))

	completion, ok := cmd.GetFlagCompletionFunc("agent")
	require.True(t, ok)
	got, directive := completion(cmd, nil, "")

	assert.Contains(t, got, cobra.Completion("acp.foo"))
	assert.NotContains(t, got, cobra.Completion("acp.goose"))
	assert.Equal(t, cobra.ShellCompDirectiveNoFileComp, directive)
}

func TestAgentFlagCompletionListsHostOnlyWithMarker(t *testing.T) {
	for _, tc := range []struct {
		name, globalTOML string
	}{
		{name: "config", globalTOML: ""},
		{name: "fallback", globalTOML: "default_agent = [broken\n"},
	} {
		for _, marker := range []string{"0", "1"} {
			t.Run(tc.name+"/marker="+marker, func(t *testing.T) {
				env := setupConfigEnv(t, tc.globalTOML, "")
				t.Chdir(env.RepoDir)
				t.Setenv("ROBOREV_HOST_AGENT", marker)
				cmd := reviewCmd()
				completion, ok := cmd.GetFlagCompletionFunc("agent")
				require.True(t, ok)
				got, _ := completion(cmd, nil, "")
				assert.Contains(t, got, cobra.Completion("codex"))
				assert.Equal(t, marker == "1", slices.Contains(got, cobra.Completion("host")), "%v", got)
			})
		}
	}
}
