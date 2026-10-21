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
// backend. Goal classification and fix jobs stay local: cursor-cloud reviews
// one commit and does not classify or edit.
func reviewHostApplies(workflow, reviewType, jobType string) bool {
	if workflow == "fix" || reviewType == config.ReviewTypeGoal {
		return false
	}
	switch jobType {
	case "", storage.JobTypeReview, storage.JobTypeRange, storage.JobTypeDirty:
		return true
	default:
		return false
	}
}

// enqueueHostJobType is the job type the host guard should see. The git ref
// wins over a declared job type: job_type "review" with a dirty or range ref
// is still a dirty or range review. An empty field is classified the same
// way. A single commit stays empty so storage still infers "review" after
// the commit id is known.
func enqueueHostJobType(explicit, customPrompt, gitRef string) string {
	switch classifyTarget("", gitRef) {
	case kindDirty:
		return storage.JobTypeDirty
	case kindRange:
		return storage.JobTypeRange
	}
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	switch classifyTarget(customPrompt, gitRef) {
	case kindDirty:
		return storage.JobTypeDirty
	case kindRange:
		return storage.JobTypeRange
	default:
		return ""
	}
}

func resolveEnqueueHost(workflow, reviewType, jobType, flag string, repo *config.RepoConfig, cfg *config.Config) (string, *RawJSONOutput) {
	repoHost, globalHost := "", ""
	if repo != nil {
		repoHost = repo.Host
	}
	if cfg != nil {
		globalHost = cfg.Host
	}
	// An explicit cursor-cloud host on a dirty or range review is rejected
	// before agent selection. It is not rewritten to the local backend.
	if jobType == storage.JobTypeDirty || jobType == storage.JobTypeRange {
		host, err := agent.ReviewHost(flag, repoHost, globalHost)
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
	host, err := agent.ReviewHost(flag, repoHost, globalHost)
	if err != nil {
		out, _ := rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return "", out
	}
	return host, nil
}
