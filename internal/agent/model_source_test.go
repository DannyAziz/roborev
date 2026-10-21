package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agentname"
	"go.kenn.io/roborev/internal/config"
)

// resolveModelSource mirrors the daemon's single-review path: workflow
// config, strict preferred-or-backup selection, then the paired model.
func resolveModelSource(t *testing.T, cliAgent string, repoCfg *config.RepoConfig, cfg *config.Config) (Agent, string, error) {
	t.Helper()
	res, err := ResolveWorkflowConfigFromConfig(cliAgent, repoCfg, cfg, "review", "")
	require.NoError(t, err)
	a, err := GetPreferredOrBackupWithConfigFromConfig(repoCfg, res.PreferredAgent, cfg, res.BackupAgent)
	if err != nil {
		return nil, "", err
	}
	return a, res.ModelForSelectedAgent(a.Name(), ""), nil
}

func TestModelSourceSelection(t *testing.T) {
	tests := []struct {
		name      string
		global    config.Config
		repo      config.RepoConfig
		cliAgent  string
		path      []string
		marker    bool
		wantAgent string
		wantModel string
		wantErr   string
		lastResor bool
	}{
		{name: "unset picks codex", path: []string{"codex", "claude"}, wantAgent: "codex"},
		{name: "unset keeps strict codex error", path: []string{"claude"}, wantErr: `no configured agent available (preferred: "codex"`},
		{name: "unset strict never falls back to host with marker", path: []string{"claude"}, marker: true, wantErr: `no configured agent available (preferred: "codex"`},
		{name: "unset default_model pairs with codex", global: config.Config{DefaultModel: "gpt-x"}, path: []string{"codex"}, wantAgent: "codex", wantModel: "gpt-x"},

		{name: "host with marker", global: config.Config{DefaultAgent: "host"}, path: []string{"codex"}, marker: true, wantAgent: "host"},
		{name: "host without marker errors naming the marker", global: config.Config{DefaultAgent: "host"}, path: []string{"codex"}, wantErr: HostMarkerEnv},
		{name: "host carries model hint", global: config.Config{DefaultAgent: "host", DefaultModel: "opus"}, marker: true, wantAgent: "host", wantModel: "opus"},

		{name: "cli without model", global: config.Config{DefaultAgent: "claude"}, path: []string{"codex", "claude"}, wantAgent: "claude-code"},
		{name: "cli with model", global: config.Config{DefaultAgent: "claude", DefaultModel: "sonnet"}, path: []string{"codex", "claude"}, wantAgent: "claude-code", wantModel: "sonnet"},
		{name: "cli unavailable keeps strict error", global: config.Config{DefaultAgent: "claude"}, path: []string{"codex"}, wantErr: `no configured agent available (preferred: "claude-code"`},
		{name: "cli unavailable keeps strict error with marker", global: config.Config{DefaultAgent: "claude"}, path: []string{"codex"}, marker: true, wantErr: `no configured agent available (preferred: "claude-code"`},

		{name: "auto uses fallback order", global: config.Config{DefaultAgent: "auto"}, path: []string{"codex", "claude"}, wantAgent: "codex"},
		{name: "auto reaches later agents", global: config.Config{DefaultAgent: "auto"}, path: []string{"gemini"}, wantAgent: "gemini"},
		{name: "auto does not pair generic model", global: config.Config{DefaultAgent: "auto", DefaultModel: "sonnet"}, path: []string{"claude"}, wantAgent: "claude-code"},
		{name: "auto does not pair generic model with host", global: config.Config{DefaultAgent: "auto", DefaultModel: "sonnet"}, marker: true, wantAgent: "host", lastResor: true},
		{name: "auto tries backup before order", global: config.Config{DefaultAgent: "auto", DefaultBackupAgent: "claude"}, path: []string{"codex", "claude"}, wantAgent: "claude-code"},
		{name: "auto with nothing and no marker", global: config.Config{DefaultAgent: "auto"}, wantErr: "no agents available"},
		{name: "auto with nothing uses host as last resort", global: config.Config{DefaultAgent: "auto"}, marker: true, wantAgent: "host", lastResor: true},
		{name: "auto with marker still prefers a CLI agent", global: config.Config{DefaultAgent: "auto"}, path: []string{"claude"}, marker: true, wantAgent: "claude-code"},
		{name: "auto as backup widens strict lookup", global: config.Config{DefaultAgent: "claude", DefaultBackupAgent: "auto"}, path: []string{"gemini"}, wantAgent: "gemini"},
		{name: "auto backup does not pair review_model", global: config.Config{DefaultAgent: "codex", ReviewBackupAgent: "auto", ReviewModel: "gpt-5"}, path: []string{"claude"}, wantAgent: "claude-code"},
		{name: "auto backup keeps default_model for the default agent", global: config.Config{DefaultAgent: "claude", DefaultModel: "sonnet", ReviewAgent: "codex", ReviewBackupAgent: "auto", ReviewModel: "gpt-5"}, path: []string{"claude"}, wantAgent: "claude-code", wantModel: "sonnet"},
		{name: "named primary with auto backup never becomes host", global: config.Config{DefaultAgent: "gemini", DefaultBackupAgent: "auto"}, marker: true, wantErr: `no configured agent available (preferred: "gemini"`},
		{name: "named primary with auto backup keeps strict error", global: config.Config{DefaultAgent: "gemini", DefaultBackupAgent: "auto"}, wantErr: `no configured agent available (preferred: "gemini"`},

		{name: "workflow auto does not pair review_model", global: config.Config{DefaultAgent: "claude", ReviewAgent: "auto", ReviewModel: "x"}, path: []string{"codex"}, wantAgent: "codex"},
		{name: "workflow auto keeps default_model for the default agent", global: config.Config{DefaultAgent: "claude", DefaultModel: "sonnet", ReviewAgent: "auto", ReviewModel: "x"}, path: []string{"claude"}, wantAgent: "claude-code", wantModel: "sonnet"},
		{name: "repo workflow auto does not pair global review_model", global: config.Config{ReviewModel: "x"}, repo: config.RepoConfig{ReviewAgent: "auto"}, path: []string{"gemini"}, wantAgent: "gemini"},
		{name: "cli auto does not pair review_model", cliAgent: "auto", global: config.Config{DefaultAgent: "codex", ReviewAgent: "claude", ReviewModel: "x"}, path: []string{"gemini"}, wantAgent: "gemini"},
		{name: "cli auto does not pair review_model with the workflow agent", cliAgent: "auto", global: config.Config{DefaultAgent: "codex", ReviewAgent: "claude", ReviewModel: "x"}, path: []string{"claude"}, wantAgent: "claude-code"},
		{name: "cli auto keeps default_model for the default agent", cliAgent: "auto", global: config.Config{DefaultAgent: "codex", DefaultModel: "gpt-x", ReviewModel: "x"}, path: []string{"codex"}, wantAgent: "codex", wantModel: "gpt-x"},
		{name: "cli agent over workflow auto does not take review_model", cliAgent: "codex", global: config.Config{DefaultAgent: "codex", ReviewAgent: "auto", ReviewModel: "x"}, path: []string{"codex"}, wantAgent: "codex"},
		{name: "cli auto uses host only as last resort", cliAgent: "auto", global: config.Config{DefaultAgent: "host"}, marker: true, wantAgent: "host", lastResor: true},
		{name: "cli auto without marker skips host", cliAgent: "auto", global: config.Config{DefaultAgent: "host"}, wantErr: "no agents available"},
		{name: "named workflow agent still pairs review_model", global: config.Config{DefaultAgent: "auto", ReviewAgent: "gemini", ReviewModel: "x"}, path: []string{"gemini"}, wantAgent: "gemini", wantModel: "x"},

		{name: "repo auto overrides global cli", global: config.Config{DefaultAgent: "claude"}, repo: config.RepoConfig{Agent: "auto"}, path: []string{"codex"}, wantAgent: "codex"},
		{name: "repo host overrides global auto with marker", global: config.Config{DefaultAgent: "auto"}, repo: config.RepoConfig{Agent: "host", Model: "r"}, path: []string{"codex"}, marker: true, wantAgent: "host", wantModel: "r"},
		{name: "repo host overrides global auto without marker errors", global: config.Config{DefaultAgent: "auto"}, repo: config.RepoConfig{Agent: "host", Model: "r"}, path: []string{"codex"}, wantErr: HostMarkerEnv},
		{name: "repo cli overrides global host", global: config.Config{DefaultAgent: "host", DefaultModel: "g"}, repo: config.RepoConfig{Agent: "claude", Model: "sonnet"}, path: []string{"claude"}, wantAgent: "claude-code", wantModel: "sonnet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setHostMarker(t, tt.marker)
			log := captureHostLastResortLog(t)
			stubPath(t, tt.path...)
			cfg, repo := tt.global, tt.repo

			a, model, err := resolveModelSource(t, tt.cliAgent, &repo, &cfg)
			if tt.wantErr != "" {
				assert.Empty(t, log.String(), "host is never a last resort on an error")
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.NotContains(t, err.Error(), "unknown agent")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantAgent, a.Name())
			assert.Equal(t, tt.wantModel, model)
			assert.Equal(t, tt.lastResor, log.Len() > 0, "host last-resort log: %q", log.String())

			if a.Name() == hostAgentName {
				host, ok := a.WithModel(model).(*HostAgent)
				require.True(t, ok)
				assert.Equal(t, tt.wantModel, host.Model)
			}
		})
	}
}

func TestAutoNeverReachesRegistryLookup(t *testing.T) {
	setHostMarker(t, false)
	stubPath(t, "claude")
	cfg := &config.Config{}

	_, err := Get(agentname.Auto)
	require.Error(t, err, "auto is a keyword, never a registered agent")
	assert.NotContains(t, Available(), agentname.Auto)
	assert.NotContains(t, AvailableNamesFromConfig(nil, cfg), agentname.Auto)

	check := func(label string, a Agent, err error) {
		t.Helper()
		require.NoError(t, err, label)
		assert.Equal(t, "claude-code", a.Name(), label)
	}
	a, err := GetAvailable("auto")
	check("GetAvailable", a, err)
	a, err = GetAvailable("auto", "auto")
	check("GetAvailable auto backup", a, err)
	a, err = GetAvailableWithConfigFromConfig(nil, " auto ", cfg)
	check("GetAvailableWithConfigFromConfig", a, err)
	a, err = GetAvailableWithConfigFromConfig(nil, "auto", nil)
	check("GetAvailableWithConfigFromConfig nil cfg", a, err)
	a, err = GetPreferredOrBackupWithConfigFromConfig(nil, "auto", cfg)
	check("GetPreferredOrBackupWithConfigFromConfig", a, err)
	a, err = GetPreferredOrBackupWithConfigFromConfig(nil, "codex", cfg, "auto")
	check("GetPreferredOrBackupWithConfigFromConfig auto backup", a, err)
	a, err = GetAvailableExactWithConfigFromConfig(nil, "auto", cfg)
	check("GetAvailableExactWithConfigFromConfig", a, err)
}

// classify_agent = "auto" is rejected (config validation and the schema
// resolver) rather than resolved: the classifier needs a named schema agent.
func TestAutoRejectedForClassifier(t *testing.T) {
	setHostMarker(t, false)
	stubPath(t, "claude")
	_, err := GetAvailableSchemaExactWithConfig("auto", &config.Config{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
	assert.NotContains(t, err.Error(), "not registered")
}

func TestAutoKeywordValidation(t *testing.T) {
	_, builtIn := agentname.BuiltIn("auto")
	assert.False(t, builtIn, "auto is a keyword, not a built-in agent")
	require.NoError(t, agentname.ValidateReference("auto"))
	assert.True(t, agentname.IsAuto(" auto "))
	assert.False(t, agentname.IsAuto("autox"))
	assert.False(t, agentname.IsAuto(""))
	assert.Equal(t, "claude-code", CanonicalName("claude"), "other names unchanged")
	assert.Error(t, agentname.ValidateReference("bogus"))
}

// An "auto" backup reaches host only as the last resort, and only under
// ROBOREV_HOST_AGENT=1: detection alone never makes host an automatic pick.
func TestResolveAutoBackup(t *testing.T) {
	cfg := &config.Config{}
	tests := []struct {
		name     string
		path     []string
		exclude  string
		want     string
		explicit string
	}{
		{name: "first available other than the primary", path: []string{"codex", "claude"}, exclude: "codex", want: "claude-code"},
		{name: "primary not in order keeps first", path: []string{"codex", "claude"}, exclude: "gemini", want: "codex"},
		{name: "alias exclude", path: []string{"claude", "gemini"}, exclude: "claude", want: "gemini"},
		{name: "only the primary", path: []string{"codex"}, exclude: "codex", want: "", explicit: "host"},
		{name: "nothing installed", exclude: "codex", want: "", explicit: "host"},
		{name: "host primary never backs itself up", exclude: "host", want: ""},
		{name: "marker never beats a CLI agent", path: []string{"codex", "claude"}, exclude: "codex", want: "claude-code"},
		{name: "host primary with a CLI", path: []string{"claude"}, exclude: "host", want: "claude-code"},
	}
	eachHostMode(t, func(t *testing.T, mode hostMode) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				stubPath(t, tt.path...)
				want := tt.want
				if mode.on && tt.explicit != "" {
					want = tt.explicit
				}
				assert.Equal(t, want, ResolveAutoBackup(nil, cfg, tt.exclude, nil))
				assert.Equal(t, mode.on, !strings.Contains(AutoBackupNotFound(), HostMarkerEnv))
			})
		}
	})
}

func TestHostModelHint(t *testing.T) {
	registered, err := Get(hostAgentName)
	require.NoError(t, err)
	withModel := registered.WithModel(" opus ")
	assert.Empty(t, registered.(*HostAgent).Model, "registered host agent is not mutated")
	assert.Equal(t, "opus", withModel.(*HostAgent).Model)

	for _, model := range []string{"", "opus"} {
		t.Run("model="+model, func(t *testing.T) {
			dir := t.TempDir()
			a := hostTestAgent(dir, 5*time.Second).WithModel(model)
			handoffs := fakeHost(t, dir, answerWith("LGTM"))
			out, err := a.Review(context.Background(), "/repo", "HEAD", "prompt", nil)
			require.NoError(t, err)
			assert.Equal(t, "LGTM", out)
			instr := (<-handoffs).instructions
			assert.Contains(t, instr, "answer.md")
			if model == "" {
				assert.NotContains(t, instr, "Requested model")
			} else {
				assert.Contains(t, instr, "Requested model (hint only): opus.")
			}
		})
	}
}

func structuredEligible(reviewType string) func(string) bool {
	return func(name string) bool {
		a, err := Get(name)
		return err == nil && ValidateStructuredReviewSelection(reviewType, a) == nil
	}
}

// An "auto" backup on a custom review passes enqueue validation and only
// resolves to an agent the caller's eligibility check accepts.
func TestAutoBackupForCustomReview(t *testing.T) {
	setHostMarker(t, false)
	resolution := WorkflowConfig{BackupAgent: "auto"}
	require.NoError(t, ValidateStructuredReviewBackup("custom", resolution, "codex"))

	cfg := &config.Config{}
	stubPath(t, "gemini")
	assert.Equal(t, "gemini", ResolveAutoBackup(nil, cfg, "codex", structuredEligible("")), "a built-in review takes any agent")
	assert.Empty(t, ResolveAutoBackup(nil, cfg, "codex", structuredEligible("custom")), "gemini cannot run a custom review")

	stubPath(t, "gemini", "claude")
	got := ResolveAutoBackup(nil, cfg, "codex", structuredEligible("custom"))
	assert.Equal(t, "claude-code", got)
}

// Strict lookup with an "auto" backup must honor eligible: a goal review
// with unavailable pi and Codex+Claude installed should pick Claude, not
// Codex.
func TestStrictAutoBackupHonorsEligible(t *testing.T) {
	setHostMarker(t, false)
	stubPath(t, "codex", "claude")
	cfg := &config.Config{}
	skipCodex := func(name string) bool { return name != "codex" }

	a, err := GetPreferredOrBackupWithConfigFromConfig(nil, "pi", cfg, "auto")
	require.NoError(t, err)
	assert.Equal(t, "codex", a.Name(), "nil eligible keeps the first available agent")

	a, err = GetPreferredOrBackupEligible(nil, "pi", cfg, skipCodex, "auto")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", a.Name())

	_, err = GetPreferredOrBackupEligible(nil, "pi", cfg, func(string) bool { return false }, "auto")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `preferred: "pi"`)
}
