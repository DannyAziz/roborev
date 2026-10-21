package daemon

import (
	"fmt"
	"net/http"
	"strings"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// reviewHostApplies reports whether this enqueue may leave the local agent
// backend. Goal classification stays local. A fix or compact workflow stays
// local only when the classified job is actually that job: a dirty or range
// ref with no prompt is a review, and the git ref wins.
func reviewHostApplies(workflow, reviewType, jobType string) bool {
	if reviewType == config.ReviewTypeGoal {
		return false
	}
	if workflow == "fix" && jobType != storage.JobTypeDirty && jobType != storage.JobTypeRange {
		return false
	}
	switch jobType {
	case "", storage.JobTypeReview, storage.JobTypeRange, storage.JobTypeDirty:
		return true
	default:
		return false
	}
}

// enqueueHostJobType is the job type the host guard should see. A custom
// prompt is what the descriptor freezes: fix and compact keep that type, and
// every other prompt is a task, even when the label contains "..". Without a
// prompt the git ref wins over a declared job type, so fix, compact, or task
// on a dirty or range ref is still that review. A single commit stays empty
// so storage still infers "review" after the commit id is known.
func enqueueHostJobType(explicit, customPrompt, gitRef string) string {
	explicit = strings.TrimSpace(explicit)
	if strings.TrimSpace(customPrompt) != "" {
		switch explicit {
		case storage.JobTypeFix, storage.JobTypeCompact:
			return explicit
		default:
			return storage.JobTypeTask
		}
	}
	switch classifyTarget("", gitRef) {
	case kindDirty:
		return storage.JobTypeDirty
	case kindRange:
		return storage.JobTypeRange
	}
	if explicit != "" {
		return explicit
	}
	return ""
}

func resolveEnqueueHost(workflow, reviewType, jobType, flag string, repo *config.RepoConfig, cfg *config.Config) (string, *RawJSONOutput) {
	repoHost, globalHost, allowRepoCloud := "", "", false
	if repo != nil {
		repoHost = repo.Host
	}
	if cfg != nil {
		globalHost, allowRepoCloud = cfg.Host, cfg.AllowCursorCloud
	}
	// Goal reviews stay local. A dirty or range review is refused before the
	// fix/compact exemption: without a prompt the descriptor stored that
	// review, and a declared fix, compact, or task does not rewrite it.
	if reviewType == config.ReviewTypeGoal {
		return agent.ReviewHostLocal, nil
	}
	// An explicit cursor-cloud host on a dirty or range review is rejected
	// before agent selection. It is not rewritten to the local backend.
	if jobType == storage.JobTypeDirty || jobType == storage.JobTypeRange {
		host, err := agent.ReviewHost(flag, repoHost, globalHost, allowRepoCloud)
		if host == agent.CursorCloudHost || agent.IsCursorCloudConfig(err) {
			out, _ := rawJSONOutput(http.StatusBadRequest, ErrorResponse{
				Error: fmt.Sprintf("cursor-cloud reviews one commit and cannot run a %s review", jobType),
			})
			return "", out
		}
	}
	if !reviewHostApplies(workflow, reviewType, jobType) {
		return agent.ReviewHostLocal, nil
	}
	host, err := agent.ReviewHost(flag, repoHost, globalHost, allowRepoCloud)
	if err != nil {
		out, _ := rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return "", out
	}
	return host, nil
}
