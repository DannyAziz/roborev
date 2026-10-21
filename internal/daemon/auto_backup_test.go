package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func stubAgentPath(t *testing.T, commands ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, c := range commands {
		require.NoError(t, os.WriteFile(filepath.Join(dir, c), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	}
	t.Setenv("PATH", dir)
}

// An "auto" backup is pinned at enqueue to a concrete agent other than the
// selected one, and never carries the configured backup model.
func TestAutoBackupPinnedAtEnqueueWithoutModel(t *testing.T) {
	noHostMarker(t)
	stubAgentPath(t, "codex", "claude")
	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "codex"
	cfg.DefaultBackupAgent = "auto"
	cfg.DefaultModel = "gpt-x"
	cfg.DefaultBackupModel = "sonnet"
	repoCfg := &config.RepoConfig{}
	res, err := agent.ResolveWorkflowConfigFromConfig("", repoCfg, cfg, "review", "")
	require.NoError(t, err)

	name, model := backupExecutionForSelectedAgent(res, "", "codex", repoCfg, cfg)
	assert.Equal(t, "claude-code", name)
	assert.Empty(t, model, "auto backup must not get the generic backup model")

	name, model = backupExecutionForSelectedAgent(res, "", "claude-code", repoCfg, cfg)
	assert.Equal(t, "codex", name)
	assert.Empty(t, model)

	stubAgentPath(t, "codex")
	name, model = backupExecutionForSelectedAgent(res, "", "codex", repoCfg, cfg)
	assert.Empty(t, name, "nothing other than the primary: no backup")
	assert.Empty(t, model)
}

// An "auto" backup fails over at most once: after the hop, the job does not
// re-resolve auto back to the agent that failed first and does not get a
// fresh set of retries. FailoverJob resets retry_count, so the second failure
// is checked at retry 0 (the normal case) as well as after a retry.
func TestAutoBackupFailsOverOnceWithoutResettingRetries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workflow    string
		storedAgent string
		storedModel string
	}{
		{name: "workflow auto backup", workflow: "auto"},
		{name: "stored auto backup", storedAgent: "auto", storedModel: "sonnet"},
	} {
		for _, retries := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/retries=%d", tc.name, retries), func(t *testing.T) {
				noHostMarker(t)
				wtc := newWorkerTestContext(t, 1)
				cfg := config.DefaultConfig()
				cfg.DefaultBackupAgent = tc.workflow
				cfg.DefaultBackupModel = "sonnet"
				wtc.reconfigurePool(cfg)
				sha := testutil.GetHeadSHA(t, wtc.TmpDir)
				commit, err := wtc.DB.GetOrCreateCommit(wtc.Repo.ID, sha, "Author", "Subject", time.Now())
				require.NoError(t, err)
				_, err = wtc.DB.EnqueueJob(storage.EnqueueOpts{
					RepoID: wtc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "codex",
					BackupAgent: tc.storedAgent, BackupModel: tc.storedModel,
				})
				require.NoError(t, err)
				stubAgentPath(t, "codex", "claude")

				job, err := wtc.DB.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, job)
				wtc.Pool.failoverOrFailNonRetryableAgentLocked(testWorkerID, job, job.Agent, "codex broke")

				hopped, err := wtc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, storage.JobStatusQueued, hopped.Status)
				assert.Equal(t, "claude-code", hopped.Agent, "auto resolves to an agent other than the failed one")
				assert.Empty(t, hopped.Model, "auto backup gets no configured model")
				assert.Equal(t, 0, hopped.RetryCount, "FailoverJob resets retries")

				job, err = wtc.DB.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, job)
				require.Equal(t, 0, job.RetryCount)
				if retries > 0 {
					_, err = wtc.DB.Exec(`UPDATE review_jobs SET retry_count = ? WHERE id = ?`, retries, job.ID)
					require.NoError(t, err)
					job.RetryCount = retries
				}
				wtc.Pool.failoverOrFailNonRetryableAgentLocked(testWorkerID, job, job.Agent, "claude broke")

				final, err := wtc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, storage.JobStatusFailed, final.Status, "no second hop back to codex")
				assert.Equal(t, "claude-code", final.Agent)
				assert.Equal(t, retries, final.RetryCount, "retries are not reset by a second failover")
				again, err := wtc.DB.ClaimJob(testWorkerID)
				require.NoError(t, err)
				assert.Nil(t, again, "the failed job is not re-run")
			})
		}
	}
}

// A rerun that selects "auto" stores the concrete agent auto resolved to.
func TestRerunSelectedAutoIsPinned(t *testing.T) {
	noHostMarker(t)
	stubAgentPath(t, "claude")
	cfg := config.DefaultConfig()
	job := &storage.ReviewJob{
		Agent: "codex", JobType: storage.JobTypeReview,
		ReviewType: config.ReviewTypeDefault, RepoPath: t.TempDir(),
	}
	opts, err := resolveRerunOpts(job, cfg, nil, "auto")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", opts.Agent)

	stubAgentPath(t)
	_, err = resolveRerunOpts(job, cfg, nil, "auto")
	require.Error(t, err, "auto with nothing available is an error, not a stored 'auto'")
}

// Rerun --agent auto must resolve the model as enqueue --agent auto does:
// keep PreferredAgent as the auto keyword so Claude does not inherit the
// configured Codex review_model.
func TestRerunSelectedAutoDoesNotInheritWorkflowModel(t *testing.T) {
	noHostMarker(t)
	stubAgentPath(t, "claude")
	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "codex"
	cfg.ReviewModel = "gpt-5"
	job := &storage.ReviewJob{
		Agent: "codex", JobType: storage.JobTypeReview,
		ReviewType: config.ReviewTypeDefault, RepoPath: t.TempDir(),
	}
	opts, err := resolveRerunOpts(job, cfg, nil, "auto")
	require.NoError(t, err)
	assert.Equal(t, "claude-code", opts.Agent)
	assert.Empty(t, opts.Model)
}

func stubAgentPathWithGit(t *testing.T, commands ...string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	stubAgentPath(t, commands...)
	require.NoError(t, os.Symlink(gitPath, filepath.Join(os.Getenv("PATH"), "git")))
}

// Fix-job enqueue pins an "auto" fix backup to a concrete agent.
func TestFixJobPinsAutoBackup(t *testing.T) {
	noHostMarker(t)
	fixture := setupFixJobFixture(t, "repo-fix-auto", "fix-auto-abc")
	fixture.server.configWatcher.Config().FixBackupAgent = "auto"
	stubAgentPathWithGit(t, "claude")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/fix", fixJobRequest{ParentJobID: fixture.reviewJob.ID})
	w := httptest.NewRecorder()
	fixture.server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var fixJob storage.ReviewJob
	testutil.DecodeJSON(t, w, &fixJob)
	stored, err := fixture.db.GetJobByID(fixJob.ID)
	require.NoError(t, err)
	assert.Equal(t, "test", stored.Agent)
	assert.Equal(t, "claude-code", stored.BackupAgent, "auto is pinned at enqueue")
	assert.Empty(t, stored.BackupModel)
	assert.True(t, stored.BackupAuto, "the pin records that it came from auto")
}

// Ordinary /api/enqueue pins an "auto" backup and the worker honors that
// pin.
func TestOrdinaryEnqueuePinsAutoBackup(t *testing.T) {
	t.Run("stores and uses a concrete pin", func(t *testing.T) {
		noHostMarker(t)
		server, db, _ := newTestServer(t)
		cfg := server.configWatcher.Config()
		cfg.DefaultBackupAgent = "auto"
		stubAgentPathWithGit(t, "claude")
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("main.go", "package main\n", "add main")
		job := enqueueViaHTTP(t, server, EnqueueRequest{
			RepoPath: repo.Path(), GitRef: "HEAD", Agent: "test", Panel: config.PanelNone,
		})
		stored, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, "test", stored.Agent)
		assert.Equal(t, "claude-code", stored.BackupAgent, "auto is pinned at ordinary enqueue")
		assert.Empty(t, stored.BackupModel)
		assert.True(t, stored.BackupAuto, "the pin records that it came from auto")

		cfg.DefaultBackupAgent = ""
		pool := NewWorkerPool(db, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
		claimed, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		pool.failoverOrFailNonRetryableAgentLocked(testWorkerID, claimed, claimed.Agent, "test broke")
		final, err := db.GetJobByID(claimed.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusQueued, final.Status)
		assert.Equal(t, "claude-code", final.Agent, "the stored pin is used, not the emptied workflow backup")
	})

	t.Run("empty pin stays frozen", func(t *testing.T) {
		noHostMarker(t)
		server, db, _ := newTestServer(t)
		cfg := server.configWatcher.Config()
		cfg.DefaultBackupAgent = "auto"
		stubAgentPathWithGit(t)
		repo := testutil.NewGitRepo(t)
		repo.CommitFile("main.go", "package main\n", "add main")
		job := enqueueViaHTTP(t, server, EnqueueRequest{
			RepoPath: repo.Path(), GitRef: "HEAD", Agent: "test", Panel: config.PanelNone,
		})
		stored, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, "test", stored.Agent)
		assert.Empty(t, stored.BackupAgent)
		assert.True(t, stored.BackupAuto)

		stubAgentPathWithGit(t, "claude")
		pool := NewWorkerPool(db, NewStaticConfig(cfg), 1, NewBroadcaster(), nil, nil)
		claimed, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		pool.failoverOrFailNonRetryableAgentLocked(testWorkerID, claimed, claimed.Agent, "test broke")
		final, err := db.GetJobByID(claimed.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusFailed, final.Status)
		assert.Equal(t, "test", final.Agent, "the empty pin is not re-resolved")
		assert.Contains(t, final.Error, "found no available agent when the job was queued")
	})
}

// Goal-review enqueue pins an "auto" backup to a goal-capable agent.
func TestGoalReviewPinsAutoBackup(t *testing.T) {
	noHostMarker(t)
	server, db, _ := newTestServer(t)
	registerGoalReviewPi(t, server.configWatcher.Config())
	repo := testutil.NewGitRepo(t)
	repo.CommitFile(".roborev.toml", "review_agent = \"pi\"\nreview_backup_agent = \"auto\"\n", "configure")
	goalArtifacts(t, repo.Path())
	for _, tc := range []struct {
		path []string
		want string
	}{
		{path: []string{"claude"}, want: "claude-code"},
		{path: []string{"codex"}, want: ""},
		{path: []string{"codex", "claude"}, want: "claude-code"},
	} {
		stubAgentPathWithGit(t, tc.path...)
		out, err := server.enqueueGoalReview(context.Background(), EnqueueRequest{RepoPath: repo.Path()})
		require.NoError(t, err)
		require.Less(t, out.Status, 300, "%v", out.Body)
		jobs, err := db.ListJobs("", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
		require.NoError(t, err)
		require.NotEmpty(t, jobs)
		stored, err := db.GetJobByID(jobs[0].ID)
		require.NoError(t, err)
		assert.Equal(t, "pi", stored.Agent)
		assert.Equal(t, tc.want, stored.BackupAgent, "codex cannot run a goal review, so it is never pinned")
		assert.True(t, stored.BackupAuto, "the pin records that it came from auto")
	}
}

// Goal-review enqueue with unavailable pi and an auto backup must select
// Claude, not reject the request after landing on Codex.
func TestGoalReviewAutoBackupSkipsIncapablePrimary(t *testing.T) {
	noHostMarker(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.CommitFile(".roborev.toml", "review_agent = \"pi\"\nreview_backup_agent = \"auto\"\n", "configure")
	goalArtifacts(t, repo.Path())
	stubAgentPathWithGit(t, "codex", "claude")

	out, err := server.enqueueGoalReview(context.Background(), EnqueueRequest{RepoPath: repo.Path()})
	require.NoError(t, err)
	require.Less(t, out.Status, 300, "%v", out.Body)

	jobs, err := db.ListJobs("", "", 0, 0, storage.WithJobType(storage.JobTypeGoalReview))
	require.NoError(t, err)
	require.NotEmpty(t, jobs)
	stored, err := db.GetJobByID(jobs[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "claude-code", stored.Agent)
	assert.True(t, stored.BackupAuto)
}

// CI matrix enqueue pins an "auto" backup to a concrete agent with no model.
func TestCIMatrixPinsAutoBackup(t *testing.T) {
	noHostMarker(t)
	h := newCIPollerHarness(t, "git@github.com:acme/api.git")
	h.Cfg.DefaultBackupAgent = "auto"
	h.Cfg.DefaultBackupModel = "sonnet"
	stubAgentPath(t, "codex", "claude")
	name, _, backup, backupModel, err := h.Poller.resolveMatrixMemberAgent(
		h.Repo, nil, h.Cfg, config.AgentReviewType{Agent: "codex", ReviewType: "default"}, "thorough",
	)
	require.NoError(t, err)
	assert.Equal(t, "codex", name)
	assert.Equal(t, "claude-code", backup)
	assert.Empty(t, backupModel)
	assert.True(t, backupIsAuto(nil, h.Cfg, config.WorkflowForReviewType("default")), "CI member opts record the auto pin")
}

type daemonHostMode struct {
	name string
	set  func(*testing.T) string
	on   bool
}

var daemonHostModes = []daemonHostMode{
	{name: "no marker", set: noHostMarker},
	{name: "marker 0", set: func(t *testing.T) string {
		dir := noHostMarker(t)
		t.Setenv(agent.HostMarkerEnv, "0")
		return dir
	}},
	{name: "explicit marker", on: true, set: func(t *testing.T) string {
		dir := noHostMarker(t)
		t.Setenv(agent.HostMarkerEnv, "1")
		return dir
	}},
}

// The host ruling, end to end at the daemon: host runs where it is named
// (primary or named backup) only with ROBOREV_HOST_AGENT=1, and is an
// automatic pick (the auto primary, a CI blank agent, an auto backup
// resolved at failover) only with that same marker. Strict enqueue with an auto backup
// never takes host. No agent CLI is on PATH, and no handoff dir is created.
func TestHostRulingByMarkerMode(t *testing.T) {
	for _, mode := range daemonHostModes {
		t.Run(mode.name, func(t *testing.T) {
			hostDir := mode.set(t)
			stubAgentPathWithGit(t)
			named, auto := "", ""
			if mode.on {
				named = "host"
			}
			if mode.on {
				auto = "host"
			}
			for _, row := range []struct {
				name, agent, backup, want string
			}{
				{"explicit host primary", "host", "", named},
				{"explicit host backup", "codex", "host", named},
				{"unset agent", "", "", ""},
				{"unset agent with auto backup", "", "auto", ""},
				{"named primary with auto backup", "gemini", "auto", ""},
				{"auto primary", "auto", "", auto},
				{"strict named primary unavailable", "codex", "", ""},
			} {
				t.Run(row.name, func(t *testing.T) {
					server, db, tmpDir := newTestServer(t)
					cfg := server.configWatcher.Config()
					cfg.DefaultAgent, cfg.DefaultBackupAgent = row.agent, row.backup
					repoDir := filepath.Join(tmpDir, "repo")
					testutil.InitTestGitRepo(t, repoDir)
					req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{RepoPath: repoDir, GitRef: "HEAD"})
					w := httptest.NewRecorder()
					server.httpServer.Handler.ServeHTTP(w, req)
					jobs, err := db.ListJobs("", "", 0, 0)
					require.NoError(t, err)
					if row.want == "" {
						assert.GreaterOrEqual(t, w.Code, 400, w.Body.String())
						assert.Empty(t, jobs, "nothing is enqueued")
						return
					}
					require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
					require.Len(t, jobs, 1)
					assert.Equal(t, row.want, jobs[0].Agent)
				})
			}
			t.Run("CI blank agent", func(t *testing.T) {
				h := newCIPollerHarness(t, "git@github.com:acme/api.git")
				name, _, _, _, err := h.Poller.resolveMatrixMemberAgent(
					h.Repo, nil, h.Cfg, config.AgentReviewType{Agent: "", ReviewType: "default"}, "thorough",
				)
				if auto == "" {
					assert.Error(t, err)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, auto, name)
			})
			t.Run("auto backup resolution", func(t *testing.T) {
				assert.Equal(t, auto, agent.ResolveAutoBackup(nil, config.DefaultConfig(), "codex", nil))
			})
			t.Run("rerun auto", func(t *testing.T) {
				job := &storage.ReviewJob{
					Agent: "codex", JobType: storage.JobTypeReview,
					ReviewType: config.ReviewTypeDefault, RepoPath: t.TempDir(),
				}
				opts, err := resolveRerunOpts(job, config.DefaultConfig(), nil, "auto")
				if err == nil && !mode.on {
					assert.NotEqual(t, "host", opts.Agent)
				}
			})
			assert.NoDirExists(t, hostDir)
		})
	}
}

// enqueueCodexJob queues a codex review with the given stored backup.
func enqueueCodexJob(t *testing.T, wtc *workerTestContext, backup string, backupAuto bool) *storage.ReviewJob {
	t.Helper()
	sha := testutil.GetHeadSHA(t, wtc.TmpDir)
	commit, err := wtc.DB.GetOrCreateCommit(wtc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	_, err = wtc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: wtc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "codex",
		BackupAgent: backup, BackupAuto: backupAuto,
	})
	require.NoError(t, err)
	job, err := wtc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	return job
}

// Runtime failover by marker mode. With ROBOREV_HOST_AGENT=1 an auto backup
// that finds no CLI agent fails over to host. Without the marker, host is
// skipped and the next agent is tried; when none is left the job fails with an
// error naming ROBOREV_HOST_AGENT. A named host backup runs only with
// ROBOREV_HOST_AGENT=1. No handoff dir is created: the job is only requeued.
func TestHostFailoverByMarkerMode(t *testing.T) {
	for _, mode := range daemonHostModes {
		for _, tc := range []struct {
			name, workflow, stored string
			backupAuto             bool
			path                   []string
			want                   string
		}{
			{name: "workflow auto backup with nothing installed", workflow: "auto"},
			{name: "stored auto backup with nothing installed", stored: "auto"},
			{name: "workflow auto backup tries the next agent", workflow: "auto", path: []string{"claude"}, want: "claude-code"},
			{name: "named host backup", stored: "host"},
			{name: "named host backup from workflow", workflow: "host"},
			{name: "empty auto pin", backupAuto: true},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				hostDir := mode.set(t)
				want := tc.want
				switch {
				case want != "" || tc.backupAuto:
				case agentNameIsHost(tc.stored) || agentNameIsHost(tc.workflow):
					if mode.on {
						want = "host"
					}
				case mode.on:
					want = "host"
				}
				wtc := newWorkerTestContext(t, 1)
				cfg := config.DefaultConfig()
				cfg.DefaultBackupAgent = tc.workflow
				wtc.reconfigurePool(cfg)
				job := enqueueCodexJob(t, wtc, tc.stored, tc.backupAuto)
				stubAgentPath(t, tc.path...)

				wtc.Pool.failoverOrFailNonRetryableAgentLocked(testWorkerID, job, job.Agent, "codex broke")

				final, err := wtc.DB.GetJobByID(job.ID)
				require.NoError(t, err)
				if want != "" {
					assert.Equal(t, storage.JobStatusQueued, final.Status)
					assert.Equal(t, want, final.Agent)
				} else {
					assert.Equal(t, storage.JobStatusFailed, final.Status)
					assert.Equal(t, "codex", final.Agent, "no failover to host")
					if tc.backupAuto {
						assert.Contains(t, final.Error, "found no available agent when the job was queued")
					} else {
						assert.Contains(t, final.Error, agent.HostMarkerEnv)
					}
				}
				assert.NoDirExists(t, hostDir)
			})
		}
	}
}

func agentNameIsHost(name string) bool { return agent.CanonicalName(name) == "host" }

// An empty auto pin is final: an agent that became available after enqueue
// is not picked up at failover.
func TestEmptyAutoPinIsFrozen(t *testing.T) {
	noHostMarker(t)
	wtc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "auto"
	wtc.reconfigurePool(cfg)
	job := enqueueCodexJob(t, wtc, "", true)
	stubAgentPath(t, "codex", "claude")

	wtc.Pool.failoverOrFailNonRetryableAgentLocked(testWorkerID, job, job.Agent, "codex broke")

	final, err := wtc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, final.Status)
	assert.Equal(t, "codex", final.Agent, "the empty pin is not re-resolved")
	assert.Contains(t, final.Error, "found no available agent when the job was queued")
}

// A host backup that auto pinned under ROBOREV_HOST_AGENT=1 runs after a
// restart only while that marker is still set. A named host backup is honored
// only with ROBOREV_HOST_AGENT=1.
func TestAutoPinnedHostIsRecheckedOnRestart(t *testing.T) {
	for _, mode := range daemonHostModes {
		t.Run(mode.name, func(t *testing.T) {
			hostDir := mode.set(t)
			wtc := newWorkerTestContext(t, 1)
			stubAgentPath(t)
			wantAuto, wantNamed := "", ""
			if mode.on {
				wantAuto = "host"
			}
			if mode.on {
				wantNamed = "host"
			}
			auto := &storage.ReviewJob{Agent: "codex", BackupAgent: "host", BackupAuto: true}
			assert.Equal(t, wantAuto, wtc.Pool.resolveBackupAgent(auto))
			named := &storage.ReviewJob{Agent: "codex", BackupAgent: "host"}
			assert.Equal(t, wantNamed, wtc.Pool.resolveBackupAgent(named))
			assert.NoDirExists(t, hostDir)
		})
	}
}

// When a host backup is skipped, the retries-exhausted and quota-prefix
// final errors both carry the skipped-host note.
func TestSkippedHostNoteOnFinalErrorPaths(t *testing.T) {
	note := agent.SkippedHostBackupNote("host")
	require.NotEmpty(t, note)

	t.Run("retries exhausted", func(t *testing.T) {
		noHostMarker(t)
		wtc := newWorkerTestContext(t, 1)
		job := enqueueCodexJob(t, wtc, "host", false)
		job = wtc.exhaustRetries(t, job, testWorkerID, job.Agent)
		wtc.Pool.failOrRetryInner(testWorkerID, job, job.Agent, "connection reset", true)
		final, err := wtc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusFailed, final.Status)
		assert.Equal(t, "codex", final.Agent)
		assert.Contains(t, final.Error, note)
	})

	t.Run("quota prefix", func(t *testing.T) {
		noHostMarker(t)
		wtc := newWorkerTestContext(t, 1)
		job := enqueueCodexJob(t, wtc, "host", false)
		wtc.Pool.failOrRetryInner(testWorkerID, job, job.Agent, "resource exhausted: reset after 1h", true)
		final, err := wtc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusFailed, final.Status)
		assert.Equal(t, "codex", final.Agent)
		assert.Contains(t, final.Error, review.QuotaErrorPrefix)
		assert.Contains(t, final.Error, note)
	})
}

// An experiment's single review pins its backup at enqueue and records
// whether the pin came from auto, including an empty pin; a named backup is
// not marked.
func TestExperimentReviewRecordsAutoPin(t *testing.T) {
	for _, tc := range []struct {
		name, backup string
		path         []string
		wantBackup   string
		wantAuto     bool
	}{
		{name: "auto pinned", backup: "auto", path: []string{"codex"}, wantBackup: "codex", wantAuto: true},
		{name: "auto pinned empty", backup: "auto", wantAuto: true},
		{name: "named backup", backup: "claude-code", path: []string{"codex"}, wantBackup: "claude-code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noHostMarker(t)
			server, db, _ := newTestServer(t)
			enabled, ratio := true, 1.0
			server.configWatcher.Config().Experiments = map[string]config.ExperimentDefinition{
				"backup-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
					Config:    map[string]any{"backup_agent": tc.backup},
				},
			}
			repo := testutil.NewGitRepo(t)
			repo.CommitFile("review.go", "package review\n", "add review")
			stubAgentPathWithGit(t, tc.path...)
			job := enqueueViaHTTP(t, server, EnqueueRequest{
				RepoPath: repo.Path(), GitRef: "HEAD", Agent: "test", Panel: config.PanelNone,
			})
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantBackup, stored.BackupAgent)
			assert.Equal(t, tc.wantAuto, stored.BackupAuto)
		})
	}
}

// Panel members and CI panel members record an auto pin.
func TestPanelMembersRecordAutoPin(t *testing.T) {
	noHostMarker(t)
	stubAgentPath(t, "codex", "claude")
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "auto"
	members := []config.ResolvedMember{{Name: "m1", Agent: "codex"}}

	opts, err := panelMemberOpts(targetDescriptor{}, "p", uuid.New(), members, nil, cfg)
	require.NoError(t, err)
	require.Len(t, opts, 1)
	assert.Equal(t, "claude-code", opts[0].BackupAgent)
	assert.True(t, opts[0].BackupAuto, "panel member")

	p := &CIPoller{}
	p.buildReviewPromptFn = func(context.Context, string, string, int64, int, string, string, string, string, *config.RepoConfig, string, *config.Config) (string, error) {
		return "prebuilt prompt", nil
	}
	memberOpts, _, err := p.buildPanelOpts(context.Background(), buildPanelOptsInput{
		repo: &storage.Repo{ID: 1, RootPath: t.TempDir()}, repoCfg: &config.RepoConfig{}, cfg: cfg,
		ghRepo: "acme/api", gitRef: "base..head", synth: config.SynthesisSpec{Agent: "codex"},
		members: []config.ResolvedMember{{Name: "m1", Agent: "codex", BackupAgent: "claude-code"}},
	})
	require.NoError(t, err)
	require.Len(t, memberOpts, 1)
	assert.True(t, memberOpts[0].BackupAuto, "CI panel member")
}

// An auto backup resolved at failover only picks an agent that can run the
// job: goal reviews need a goal agent, custom reviews structured output.
func TestWorkerAutoBackupHonorsReviewType(t *testing.T) {
	for _, tc := range []struct {
		name, reviewType, primary string
		path                      []string
		want                      string
	}{
		{name: "custom skips gemini", reviewType: "custom", primary: "claude-code", path: []string{"gemini"}},
		{name: "default takes gemini", reviewType: config.ReviewTypeDefault, primary: "claude-code", path: []string{"gemini"}, want: "gemini"},
		{name: "goal skips codex", reviewType: config.ReviewTypeGoal, primary: "pi", path: []string{"codex"}},
		{name: "goal takes claude", reviewType: config.ReviewTypeGoal, primary: "pi", path: []string{"codex", "claude"}, want: "claude-code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noHostMarker(t)
			wtc := newWorkerTestContext(t, 1)
			stubAgentPath(t, tc.path...)
			job := &storage.ReviewJob{Agent: tc.primary, ReviewType: tc.reviewType, BackupAgent: "auto"}
			assert.Equal(t, tc.want, wtc.Pool.resolveBackupAgent(job))
		})
	}
}

// A rerun pins an "auto" backup again against the rerun's primary and the
// current agents: an empty pin picks up an agent installed since, and a pin
// that matches the newly selected primary moves to another agent.
func TestRerunRepinsAutoBackup(t *testing.T) {
	for _, tc := range []struct {
		name, pinned, selected      string
		wantAgent, wantBackupPinned string
	}{
		{name: "empty pin", pinned: "", selected: "", wantAgent: "codex", wantBackupPinned: "claude-code"},
		{name: "pin equals new primary", pinned: "claude-code", selected: "claude", wantAgent: "claude-code", wantBackupPinned: "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noHostMarker(t)
			wtc := newWorkerTestContext(t, 1)
			cfg := config.DefaultConfig()
			cfg.DefaultBackupAgent = "auto"
			wtc.reconfigurePool(cfg)
			job := enqueueCodexJob(t, wtc, tc.pinned, true)
			_, err := wtc.DB.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, job.ID)
			require.NoError(t, err)
			stubAgentPath(t, "codex", "claude")

			job, err = wtc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			opts, err := resolveRerunOpts(job, cfg, nil, tc.selected)
			require.NoError(t, err)
			require.NoError(t, wtc.DB.ReenqueueJob(job.ID, opts))

			rerun, err := wtc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAgent, rerun.Agent)
			assert.Equal(t, tc.wantBackupPinned, rerun.BackupAgent)
			assert.True(t, rerun.BackupAuto)
		})
	}
}

// Enqueue-time "auto" picks skip agents that cannot run the review type, for
// an auto primary and an auto backup alike.
func TestEnqueueAutoPickHonorsReviewType(t *testing.T) {
	noHostMarker(t)
	stubAgentPath(t, "gemini", "pi")
	cfg := config.DefaultConfig()
	eligible := autoBackupEligible("custom")

	a, err := agent.GetPreferredOrBackupEligible(nil, "auto", cfg, eligible)
	require.NoError(t, err)
	assert.Equal(t, "pi", a.Name(), "auto primary skips gemini for a custom review")

	a, err = agent.GetPreferredOrBackupEligible(nil, "codex", cfg, eligible, "auto")
	require.NoError(t, err)
	assert.Equal(t, "pi", a.Name(), "auto backup skips gemini for a custom review")
}
