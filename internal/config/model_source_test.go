package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelSourceValuesPassValidation(t *testing.T) {
	for _, value := range []string{"auto", "host", "claude", "codex"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			globalPath := filepath.Join(dir, "config.toml")
			require.NoError(t, os.WriteFile(globalPath, []byte("default_agent = \""+value+"\"\ndefault_backup_agent = \""+value+"\"\nreview_agent = \""+value+"\"\ndefault_model = \"m1\"\n"), 0o600))
			global, err := LoadGlobalFrom(globalPath)
			require.NoError(t, err)
			require.NoError(t, global.Validate())
			assert.Equal(t, value, ResolveAgent("", "", global))

			repo := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte("agent = \""+value+"\"\nbackup_agent = \""+value+"\"\n"), 0o600))
			require.NoError(t, ValidateRepoConfig(repo))

			viaSet := &Config{}
			require.NoError(t, SetConfigValue(viaSet, "default_agent", value))
			require.NoError(t, viaSet.Validate())
			assert.Equal(t, value, viaSet.DefaultAgent)
		})
	}
}

// classify_agent = "auto" is rejected, not resolved: the classifier needs a
// named schema-capable agent. Nested agent keys are always explicit.
func TestAutoRejectedOutsideTopLevelAgentKeys(t *testing.T) {
	tests := []struct {
		name, toml, want string
	}{
		{name: "classify_agent", toml: "classify_agent = \"auto\"\n", want: "classify_agent: \"auto\" is not supported for the classifier"},
		{name: "classify_backup_agent", toml: "classify_backup_agent = \"auto\"\n", want: "classify_backup_agent: \"auto\" is not supported for the classifier"},
		{name: "nested ci synthesis agent", toml: "[ci]\nsynthesis_agent = \"auto\"\n", want: "ci.synthesis_agent: \"auto\" is only supported in default_agent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tt.toml), 0o600))
			global, err := LoadGlobalFrom(path)
			if err == nil {
				err = global.Validate()
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}

	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".roborev.toml"), []byte("classify_agent = \"auto\"\n"), 0o600))
	err := ValidateRepoConfig(repo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported for the classifier")
}

func TestModelSourceRepoOverridesGlobal(t *testing.T) {
	tests := []struct {
		name, global, globalModel, repo, repoModel string
		wantAgent, wantModel                       string
	}{
		{name: "unset", wantAgent: "codex"},
		{name: "global only", global: "host", globalModel: "g", wantAgent: "host", wantModel: "g"},
		{name: "repo auto over global host", global: "host", globalModel: "g", repo: "auto", wantAgent: "auto", wantModel: "g"},
		{name: "repo host over global auto", global: "auto", repo: "host", repoModel: "r", wantAgent: "host", wantModel: "r"},
		{name: "repo cli over global host", global: "host", repo: "claude", repoModel: "sonnet", wantAgent: "claude", wantModel: "sonnet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global := &Config{DefaultAgent: tt.global, DefaultModel: tt.globalModel}
			repo := &RepoConfig{Agent: tt.repo, Model: tt.repoModel}
			assert.Equal(t, tt.wantAgent, ResolveAgentFromConfig("", repo, global))
			assert.Equal(t, tt.wantModel, ResolveModelFromConfig("", repo, global))
			assert.Equal(t, "gemini", ResolveAgentFromConfig("gemini", repo, global), "explicit flag still wins")
		})
	}
}

func TestBudgetRejectsAutoAgentCost(t *testing.T) {
	b := BudgetConfig{Enabled: true, DailyLimitCents: 100, AgentCosts: map[string]int{"auto": 5}}
	err := b.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "selection keyword")
}
