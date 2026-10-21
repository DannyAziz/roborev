package agent

import (
	"fmt"
	"slices"

	"go.kenn.io/roborev/internal/agentname"
	"go.kenn.io/roborev/internal/config"
)

// withoutAuto maps the "auto" selection keyword to "no named preference" so
// it reaches the normal availability order instead of a registry lookup.
func withoutAuto(name string) string {
	if agentname.IsAuto(name) {
		return ""
	}
	return name
}

// splitAuto removes "auto" from backups and reports whether it was present.
func splitAuto(backups []string) ([]string, bool) {
	if !slices.ContainsFunc(backups, agentname.IsAuto) {
		return backups, false
	}
	return slices.DeleteFunc(slices.Clone(backups), agentname.IsAuto), true
}

// ResolveAutoBackup picks the concrete agent an "auto" backup stands for: the
// first available agent in the normal order, other than exclude (the agent
// the backup protects), that eligible accepts (nil accepts any), then host as
// a last resort when ROBOREV_HOST_AGENT=1 allows an automatic pick. It returns
// "" when nothing qualifies.
func ResolveAutoBackup(repoCfg *config.RepoConfig, cfg *config.Config, exclude string, eligible func(string) bool) string {
	return resolveAutoBackup(repoCfg, cfg, exclude, eligible, true)
}

func resolveAutoBackup(repoCfg *config.RepoConfig, cfg *config.Config, exclude string, eligible func(string) bool, lastResort bool) string {
	exclude = resolveAlias(withoutAuto(exclude))
	usable := func(name string) bool { return name != exclude && (eligible == nil || eligible(name)) }
	for _, name := range append(slices.Clone(fallbackAgentOrder), Available()...) {
		if name != "test" && name != hostAgentName && name != cursorCloudAgentName && usable(name) && isAvailableWithConfigFromConfig(name, repoCfg, cfg) {
			return name
		}
	}
	if lastResort && usable(hostAgentName) && HostAllowed() {
		return hostAgentName
	}
	return ""
}

// AutoBackupNotFound explains an "auto" backup that resolved to no agent.
func AutoBackupNotFound() string {
	if HostAllowed() {
		return fmt.Sprintf("\nbackup %q found no available agent", agentname.Auto)
	}
	return fmt.Sprintf("\nbackup %q found no available agent; host is an automatic pick only with %s=1", agentname.Auto, HostMarkerEnv)
}
