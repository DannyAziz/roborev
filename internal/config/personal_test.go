package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func personalTestConfig() *Config {
	cfg := DefaultConfig()
	cfg.CI.Repos = []string{"acme/api"}
	cfg.CI.Personal = PersonalReviewConfig{Enabled: true, Reviewer: "alice", Panel: "personal"}
	cfg.Review.Subagents = map[string]SubagentSpec{"reviewer": {Agent: "test", Instructions: "Review correctness."}}
	cfg.Review.Panels = map[string]PanelSpec{"personal": {Members: []string{"reviewer"}}}
	return cfg
}

func TestPersonalConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
		want   string
	}{
		{"valid", func(*Config) {}, ""},
		{"no reviewer", func(c *Config) { c.CI.Personal.Reviewer = "" }, "reviewer"},
		{"me alias", func(c *Config) { c.CI.Personal.Reviewer = "@me" }, "reviewer"},
		{"invalid mode", func(c *Config) { c.CI.Personal.Mode = "auto" }, "mode"},
		{"no allowlist", func(c *Config) { c.CI.Repos = nil }, "allowlist"},
		{"wildcard allowlist", func(c *Config) { c.CI.Repos = []string{"acme/*"} }, "allowlist"},
		{"URL allowlist", func(c *Config) { c.CI.Repos = []string{"https://github.com/acme/api"} }, "allowlist"},
		{"missing panel", func(c *Config) { c.CI.Personal.Panel = "missing" }, "panel"},
		{"missing agent", func(c *Config) { c.Review.Subagents["reviewer"] = SubagentSpec{} }, "agent"},
		{"nonvoting", func(c *Config) { c.Review.Subagents["reviewer"] = SubagentSpec{Agent: "test", NonVoting: true} }, "voting"},
		{"installation auth", func(c *Config) { c.CI.GitHubAppID = 1 }, "authentication"},
		{"partial installation auth", func(c *Config) { c.CI.GitHubAppPrivateKey = "key" }, "authentication"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := personalTestConfig()
			tc.change(cfg)
			err := cfg.Validate()
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestPersonalConfigRoundTrip(t *testing.T) {
	cfg := personalTestConfig()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, SaveGlobalTo(path, cfg))
	loaded, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	assert.Equal(t, cfg.CI.Personal, loaded.CI.Personal)
	assert.Equal(t, "dry-run", loaded.CI.Personal.ResolvedMode())
	assert.False(t, loaded.CI.Personal.Approve)
	assert.False(t, loaded.CI.Personal.RequestChanges)
}
