package daemon

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// enqueueCursorCloudHostSource enqueues one ordinary review whose cursor-cloud
// host comes from exactly one source, with a workflow backup of "test". The
// repository has no remote, so the default cursor-cloud agent refuses the
// launch before create. configure, when given, adjusts the global config
// before the server starts.
func enqueueCursorCloudHostSource(t *testing.T, source string, configure ...func(*config.Config)) (*Server, *storage.ReviewJob) {
	t.Helper()
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	t.Setenv(agent.EnvDefaultHost, "")
	db, tmpDir := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 0
	cfg.ReviewBackupAgent = "test"
	req := EnqueueRequest{GitRef: "HEAD"}
	repo := testutil.InitTestGitRepo(t, filepath.Join(tmpDir, "repo"))
	repo.CommitFile("a.txt", "one\n", "one")
	switch source {
	case "--host":
		req.Host = agent.CursorCloudHost
	case "global host":
		cfg.Host = agent.CursorCloudHost
	case "repo host":
		repo.CommitFile(".roborev.toml", "host = \"cursor-cloud\"\n", "host")
	case "ROBOREV_DEFAULT_HOST":
		t.Setenv(agent.EnvDefaultHost, agent.CursorCloudHost)
	default:
		t.Fatalf("unknown host source %q", source)
	}
	for _, fn := range configure {
		fn(cfg)
	}
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })

	req.RepoPath = repo.Path()
	created := enqueueViaHTTP(t, server, req)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, created.ID, claimed.ID)
	require.Equal(t, agent.CursorCloudHost, claimed.Agent)
	require.Empty(t, claimed.BackupAgent)
	var explicit int
	require.NoError(t, db.QueryRow(`SELECT explicit_host FROM review_jobs WHERE id = ?`, created.ID).Scan(&explicit))
	require.Equal(t, source == "--host", explicit == 1)
	require.Equal(t, source == "--host", claimed.ExplicitHost)
	return server, claimed
}

// assertFailedOnCursorCloud requires the job to fail on cursor-cloud with no
// retry and no backup attempt left to claim.
func assertFailedOnCursorCloud(t *testing.T, server *Server, jobID int64) {
	t.Helper()
	updated, err := server.db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, updated.Status)
	assert.Equal(t, agent.CursorCloudHost, updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
	assert.NotContains(t, updated.Error, "failing over")
	claimed, claimErr := server.db.ClaimJob(testWorkerID)
	require.NoError(t, claimErr)
	assert.Nil(t, claimed)
}

func TestExplicitHostFlagPreLaunchDoesNotFailOver(t *testing.T) {
	server, job := enqueueCursorCloudHostSource(t, "--host")
	assert.Empty(t, server.workerPool.resolveBackupAgent(job))

	server.workerPool.processJob(testWorkerID, job)

	assertFailedOnCursorCloud(t, server, job.ID)
}

func TestExplicitHostFlagPostSendDoesNotFailOver(t *testing.T) {
	server, job := enqueueCursorCloudHostSource(t, "--host")

	server.workerPool.finishAgentError(
		context.Background(), testWorkerID, job, agent.CursorCloudHost,
		fmt.Errorf("create agent: %w", agent.ErrCursorCloudNonRetryable), time.Minute,
	)

	assertFailedOnCursorCloud(t, server, job.ID)
}

func TestExplicitHostFlagCreate500AfterSendDoesNotFailOver(t *testing.T) {
	server, job := enqueueCursorCloudHostSource(t, "--host")
	fake := &cursorcloudtest.Fake{MethodErrors: map[string]error{
		cursorcloudtest.MethodCreate: &cursorcloud.APIError{
			Op: "create agent", StatusCode: http.StatusInternalServerError, Message: "unavailable",
		},
		cursorcloudtest.MethodList: &cursorcloud.APIError{
			Op: "list agents", StatusCode: http.StatusTooManyRequests, Message: "slow",
		},
	}}
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	agent.Register(&agent.CursorCloudAgent{
		Client:  fake,
		RepoURL: "https://github.com/acme/api.git",
	})
	t.Cleanup(func() { agent.Register(prev) })

	server.workerPool.processJob(testWorkerID, job)

	creates := 0
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodCreate {
			creates++
		}
	}
	require.Equal(t, 1, creates)
	assertFailedOnCursorCloud(t, server, job.ID)
}

// TestConfigAndEnvCursorCloudHostPreLaunchFailsOver keeps the boundary of the
// rule: only --host is explicit. A config host or ROBOREV_DEFAULT_HOST whose
// launch is refused before create still fails over to the workflow backup.
func TestConfigAndEnvCursorCloudHostPreLaunchFailsOver(t *testing.T) {
	for _, source := range []string{"global host", "repo host", "ROBOREV_DEFAULT_HOST"} {
		t.Run(source, func(t *testing.T) {
			server, job := enqueueCursorCloudHostSource(t, source)
			assert.Equal(t, "test", server.workerPool.resolveBackupAgent(job))

			server.workerPool.processJob(testWorkerID, job)

			updated, err := server.db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusQueued, updated.Status)
			assert.Equal(t, "test", updated.Agent)
		})
	}
}

// TestExplicitHostFlagIsNotBudgetRouted pins that budget routing never
// replaces an explicit --host cursor-cloud with a cheaper local agent. Inside
// the reserve on an empty day codex scores 1/5 against cursor-cloud's 2/15,
// so a config host is still substituted; --host stays on cursor-cloud.
func TestExplicitHostFlagIsNotBudgetRouted(t *testing.T) {
	for _, source := range []string{"--host", "global host"} {
		t.Run(source, func(t *testing.T) {
			codex := &budgetRecordingAgent{name: "codex"}
			original, err := agent.Get(codex.name)
			require.NoError(t, err)
			agent.Register(codex)
			t.Cleanup(func() { agent.Register(original) })
			server, job := enqueueCursorCloudHostSource(t, source, func(cfg *config.Config) {
				cfg.Budget = config.BudgetConfig{
					Enabled: true, DailyLimitCents: 100, ReserveFloorCents: 100,
					AgentCosts: config.BudgetAgentCosts{agent.CursorCloudHost: 15, "codex": 5},
				}
			})

			server.workerPool.processJob(testWorkerID, job)

			if source == "--host" {
				assert.Zero(t, codex.calls)
				assertFailedOnCursorCloud(t, server, job.ID)
				updated, err := server.db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.False(t, updated.BudgetRoutingLocked)
				assert.Empty(t, updated.BudgetOriginalAgent)
				return
			}
			updated, err := server.db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusDone, updated.Status)
			assert.Equal(t, "codex", updated.Agent)
			assert.Equal(t, agent.CursorCloudHost, updated.BudgetOriginalAgent)
			assert.Equal(t, 1, codex.calls)
		})
	}
}
