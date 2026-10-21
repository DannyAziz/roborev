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

// enqueueHostJobType is the job type the host guard should see. A custom
// prompt is a task even when its label looks like a range. Otherwise the git
// ref wins over a declared job type: job_type "review" with a dirty or range
// ref is still a dirty or range review. An empty field is classified the same
// way. A single commit stays empty so storage still infers "review" after
// the commit id is known.
func enqueueHostJobType(explicit, customPrompt, gitRef string) string {
	// run and analyze send a custom prompt and put a label in git_ref.
	// That label is not a commit, even when it contains "..". A task cannot
	// run on cursor-cloud, so the host guard must see "task" and the
	// local-agent exemption can keep it off the cloud path.
	explicit = strings.TrimSpace(explicit)
	if explicit == storage.JobTypeTask || (explicit == "" && strings.TrimSpace(customPrompt) != "") {
		return storage.JobTypeTask
	}
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
	// Fix jobs and goal reviews stay local. A range ref is classified before
	// the declared job type, so this exemption has to run before the
	// dirty/range refusal. Otherwise host=cursor-cloud rejects a fix job
	// whose ref contains "..".
	if !reviewHostApplies(workflow, reviewType, jobType) {
		return agent.ReviewHostLocal, nil
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
	host, err := agent.ReviewHost(flag, repoHost, globalHost)
	if err != nil {
		out, _ := rawJSONOutput(http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return "", out
	}
	return host, nil
}
