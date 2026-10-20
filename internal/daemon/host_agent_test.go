package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// noHostMarker leaves no marker of either kind: ROBOREV_HOST_AGENT unset and
// no Cursor cloud agent variables.
func noHostMarker(t *testing.T) string {
	t.Helper()
	for _, kv := range append(os.Environ(), agent.HostMarkerEnv+"=", "CURSOR_AGENT=") {
		if key, _, _ := strings.Cut(kv, "="); key == agent.HostMarkerEnv || strings.HasPrefix(key, "CURSOR_AGENT") || strings.HasPrefix(key, "CLOUD_AGENT_") {
			t.Setenv(key, "")
			require.NoError(t, os.Unsetenv(key))
		}
	}
	hostDir := filepath.Join(t.TempDir(), "host")
	t.Setenv("ROBOREV_HOST_DIR", hostDir)
	return hostDir
}

// An explicit host with no marker is rejected at enqueue: no job is queued
// and no handoff dir is created.
func TestEnqueueExplicitHostWithoutMarkerFailsFast(t *testing.T) {
	hostDir := noHostMarker(t)
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir, GitRef: "HEAD", Agent: "host",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), agent.HostMarkerEnv)
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs)
	assert.NoDirExists(t, hostDir)
}

// Mutant pin: if HostMarker.Allows(HostNamed) treats Detected as sufficient,
// this named-host enqueue returns 201 and queues a job.
func TestEnqueueNamedHostDetectedWithoutMarkerFails(t *testing.T) {
	hostDir := noHostMarker(t)
	t.Setenv("CURSOR_AGENT", "1")
	t.Setenv("CLOUD_AGENT_FLAKE", "1")
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir, GitRef: "HEAD", Agent: "host",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.NotEqual(t, http.StatusCreated, w.Code, w.Body.String())
	assert.GreaterOrEqual(t, w.Code, 400, w.Body.String())
	assert.Contains(t, w.Body.String(), agent.HostMarkerEnv)
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs, "nothing is queued")
	assert.NoDirExists(t, hostDir)
}

// A queued host job whose daemon has no marker fails on its first attempt,
// without retries or failover, and never creates a handoff dir.
func TestWorkerExplicitHostWithoutMarkerFailsWithoutRetry(t *testing.T) {
	hostDir := noHostMarker(t)
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "test"
	tc.reconfigurePool(cfg)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "host")

	tc.Pool.processJob(testWorkerID, job)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, "host", updated.Agent, "no failover to the backup")
	assert.Equal(t, 0, updated.RetryCount)
	assert.Contains(t, updated.Error, agent.HostMarkerEnv)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	assert.Nil(t, claimed, "nothing is requeued")
	assert.NoDirExists(t, hostDir)
}

// Any other failure to get the agent keeps the upstream path: the job is
// retried on the same agent.
func TestWorkerNonHostGetAgentErrorIsStillRetried(t *testing.T) {
	noHostMarker(t)
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	require.NoError(t, os.Symlink(gitPath, filepath.Join(bin, "git")))
	t.Setenv("PATH", bin)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")

	tc.Pool.processJob(testWorkerID, job)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, "codex", updated.Agent)
	assert.Equal(t, 1, updated.RetryCount)
}

func hostMarkerOn(t *testing.T, timeout string) string {
	t.Helper()
	t.Setenv(agent.HostMarkerEnv, "1")
	t.Setenv("ROBOREV_HOST_TIMEOUT", timeout)
	hostDir := filepath.Join(t.TempDir(), "host")
	t.Setenv("ROBOREV_HOST_DIR", hostDir)
	return hostDir
}

func claimHostSynthesis(t *testing.T, tc *workerTestContext, name, backup string) *storage.ReviewJob {
	t.Helper()
	return claimSynthesis(t, tc, name, "host", backup)
}

func claimSynthesis(t *testing.T, tc *workerTestContext, name, synthAgent, backup string) *storage.ReviewJob {
	t.Helper()
	const member = "host-synth-member"
	registerPassingAgent(t, member)
	runUUID, members, _ := enqueuePanelRun(t, tc, name, []memberSpec{
		{name: "m0", agent: member}, {name: "m1", agent: member},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent)
	setSynthesisBackup(t, tc, runUUID, backup)
	completeMember(t, tc, members[0].ID, member, "Finding A")
	completeMember(t, tc, members[1].ID, member, "Finding B")
	return releaseAndClaimSynthesis(t, tc, runUUID)
}

func setSynthesisBackup(t *testing.T, tc *workerTestContext, runUUID uuid.UUID, backup string) {
	t.Helper()
	_, err := tc.DB.Exec(
		"UPDATE review_jobs SET backup_agent = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'",
		backup, runUUID,
	)
	require.NoError(t, err)
}

// A host synthesis with no marker fails on its first attempt, keeps its agent
// (no failover to the configured backup) and never creates a handoff dir.
func TestSynthesisExplicitHostWithoutMarkerFailsWithoutRetry(t *testing.T) {
	hostDir := noHostMarker(t)
	tc := newWorkerTestContext(t, 1)
	synth := claimHostSynthesis(t, tc, "host-nomarker", "test")

	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	updated := tc.assertJobStatus(t, synth.ID, storage.JobStatusFailed)
	assert.Equal(t, "host", updated.Agent, "no failover to the backup")
	assert.Equal(t, 0, updated.RetryCount)
	assert.Contains(t, updated.Error, agent.HostMarkerEnv)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	assert.Nil(t, claimed, "nothing is requeued")
	assert.NoDirExists(t, hostDir)
}

// A host handoff timeout during synthesis burns no same-agent retries: it
// fails over once when a backup is set and fails when none is.
func TestSynthesisHostTimeoutSkipsRetries(t *testing.T) {
	for _, tt := range []struct {
		name, backup, wantStatus, wantAgent string
	}{
		{"no backup", "", string(storage.JobStatusFailed), "host"},
		{"backup", "test", string(storage.JobStatusQueued), "test"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			hostMarkerOn(t, "50ms")
			tc := newWorkerTestContext(t, 1)
			synth := claimHostSynthesis(t, tc, "host-timeout", tt.backup)

			tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

			updated := tc.assertJobStatus(t, synth.ID, storage.JobStatus(tt.wantStatus))
			assert.Equal(t, tt.wantAgent, updated.Agent)
			assert.Equal(t, 0, updated.RetryCount, "a host timeout is not retried on host")
			if tt.backup == "" {
				assert.Contains(t, updated.Error, "host handoff timeout")
			}
		})
	}
}

// Only host synthesis errors take the typed path; any other agent's run error,
// even one marked unavailable, keeps its same-agent retry.
func TestSynthesisNonHostErrorIsStillRetried(t *testing.T) {
	noHostMarker(t)
	const synthAgent = "synth-flaky"
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: synthAgent,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			return "", agent.MarkUnavailable(errors.New("synth broke"))
		},
	})
	tc := newWorkerTestContext(t, 1)
	synth := claimSynthesis(t, tc, "synth-retry", synthAgent, "test")

	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	updated := tc.assertJobStatus(t, synth.ID, storage.JobStatusQueued)
	assert.Equal(t, "test", updated.Agent, "unavailable synthesis fails over instead of stringifying the mark")
	assert.Equal(t, 0, updated.RetryCount)
}

// A stored host backup is only resolved when host is allowed, so a job never
// hops to host without the marker, and the final error names the marker.
func TestStoredHostBackupNeedsMarker(t *testing.T) {
	const primary = "host-backup-primary"
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: primary,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			return "", agent.MarkUnavailable(errors.New("primary broke"))
		},
	})
	for _, tt := range []struct {
		name   string
		marker func(*testing.T)
		want   string
	}{
		{"no marker", func(t *testing.T) { noHostMarker(t) }, ""},
		{"detected only", func(t *testing.T) {
			t.Setenv(agent.HostMarkerEnv, "")
			t.Setenv("CURSOR_AGENT", "1")
			t.Setenv("CLOUD_AGENT_TEST_DETECTION", "1")
		}, ""},
		{"explicit marker", func(t *testing.T) { hostMarkerOn(t, "1m") }, "host"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.marker(t)
			tc := newWorkerTestContext(t, 1)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, primary)
			_, err := tc.DB.Exec(`UPDATE review_jobs SET backup_agent='host' WHERE id=?`, job.ID)
			require.NoError(t, err)
			job.BackupAgent = "host"
			assert.Equal(t, tt.want, tc.Pool.resolveBackupAgent(job))

			tc.Pool.processJob(testWorkerID, job)

			updated, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			if tt.want == "" {
				assert.Equal(t, storage.JobStatusFailed, updated.Status)
				assert.Equal(t, primary, updated.Agent, "never hops to host")
				assert.Contains(t, updated.Error, agent.HostMarkerEnv)
			} else {
				assert.Equal(t, storage.JobStatusQueued, updated.Status)
				assert.Equal(t, "host", updated.Agent)
			}
		})
	}
}
