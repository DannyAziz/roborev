package config

import (
	"fmt"
	"strings"
)

// PersonalReviewConfig is local publication authority, never repository config.
// An empty mode means dry-run. One daemon/data directory owns one reviewer.
type PersonalReviewConfig struct {
	Enabled        bool   `toml:"enabled"`
	Reviewer       string `toml:"reviewer"`
	Panel          string `toml:"panel"`
	Mode           string `toml:"mode"`
	Approve        bool   `toml:"approve"`
	RequestChanges bool   `toml:"request_changes"`
}

func (c PersonalReviewConfig) ResolvedMode() string {
	if c.Mode == "" {
		return "dry-run"
	}
	return c.Mode
}

func (c PersonalReviewConfig) Validate(global *Config) error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Reviewer) == "" || strings.TrimSpace(c.Reviewer) != c.Reviewer || strings.EqualFold(c.Reviewer, "@me") {
		return fmt.Errorf("ci.personal.reviewer must name the publishing GitHub user")
	}
	if c.ResolvedMode() != "dry-run" && c.ResolvedMode() != "publish" {
		return fmt.Errorf("ci.personal.mode must be dry-run or publish")
	}
	if global.CI.GitHubAppID != 0 || global.CI.GitHubAppPrivateKey != "" || global.CI.GitHubAppInstallationID != 0 || len(global.CI.GitHubAppInstallations) != 0 {
		return fmt.Errorf("ci.personal cannot use GitHub App installation authentication")
	}
	if len(global.CI.Repos) == 0 {
		return fmt.Errorf("ci.personal requires an explicit ci.repos allowlist")
	}
	for _, repo := range global.CI.Repos {
		parts := strings.Split(repo, "/")
		if strings.TrimSpace(repo) != repo || len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repo, "*? ") {
			return fmt.Errorf("ci.personal allowlist must contain explicit owner/repo names")
		}
	}
	panel, ok := global.Review.Panels[c.Panel]
	if !ok || len(panel.Members) == 0 {
		return fmt.Errorf("ci.personal.panel must name a locally configured review panel")
	}
	for _, name := range panel.Members {
		member, ok := global.Review.Subagents[name]
		if !ok || strings.TrimSpace(member.Agent) == "" {
			return fmt.Errorf("personal panel member %q requires a locally configured agent", name)
		}
		if member.NonVoting {
			return fmt.Errorf("personal panel members must be voting reviewers")
		}
	}
	return nil
}
