package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"os"
	"sync"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const cursorCloudRestartRepo = "https://example.com/acme/repo.git"

// TestCursorCloudRestartStopsPersistedAgent is the crash pin: agent A is
// created, the daemon dies with the row still running, and restart must
// stop and delete A, create nothing else, and fail the job for good.
func TestCursorCloudRestartStopsPersistedAgent(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	local := tc.createJobWithAgent(t, sha, "codex")
	_, err := tc.DB.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, local.ID)
	require.NoError(t, err)
	created := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-a", true)
	require.NoError(t, os.MkdirAll(JobLogDir(), 0o700))
	logLine := "cursor-cloud: created agent " + created.ID + "\n"
	require.NoError(t, os.WriteFile(JobLogPath(job.ID), []byte(logLine), 0o600))

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(t.Context(), tc.DB, fake))
	cursorCloudAssertNoRelaunch(t, tc, job.ID, fake, creates)

	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodStop))
	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	_, err = fake.GetAgent(t.Context(), created.ID)
	require.Error(t, err)
	body, err := os.ReadFile(JobLogPath(job.ID))
	require.NoError(t, err)
	assert.Equal(t, logLine, string(body))
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, storage.CloudLaunchInterruptedError, updated.Error)
	assert.Equal(t, 0, updated.RetryCount)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	claimed, err := tc.DB.ClaimJob(testWorkerID + "-again")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, local.ID, claimed.ID)
}

// TestCursorCloudRestartSweepsMarkerWithoutID is the crash between the
// create POST and persisting the agent id. The launch token is already
// stored, and the marker sweep must delete A.
func TestCursorCloudRestartSweepsMarkerWithoutID(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	created := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-no-id", false)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	cursorCloudAssertNoRelaunch(t, tc, job.ID, fake, creates)

	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodStop))
	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	_, err := fake.GetAgent(t.Context(), created.ID)
	require.Error(t, err)
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, storage.CloudLaunchInterruptedError, updated.Error)
	assert.Equal(t, 0, updated.RetryCount)
}

// TestCursorCloudRestartLeavesForeignMarker is the negative pin. A sweep
// may stop or delete only the agent whose marker is this daemon and this
// job's launch token. Removing that scope deletes a foreign agent and
// this test fails.
func TestCursorCloudRestartLeavesForeignMarker(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	const token = "launch-token-scope"
	ours := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(machineID.String(), token))                       //nolint:forbidigo // launch marker is prompt text
	otherDaemon := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(uuid.New().String(), token))               //nolint:forbidigo // foreign daemon id in the marker
	otherToken := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(machineID.String(), "someone-elses-token")) //nolint:forbidigo // launch marker is prompt text
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, cursorCloudRestartRepo))

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))

	assert.Equal(t, []string{ours.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodStop))
	assert.Equal(t, []string{ours.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	_, err = fake.GetAgent(t.Context(), ours.ID)
	require.Error(t, err)
	for _, id := range []string{otherDaemon.ID, otherToken.ID} {
		got, getErr := fake.GetAgent(t.Context(), id)
		require.NoError(t, getErr)
		assert.Equal(t, id, got.ID)
	}
}

// TestCursorCloudRestartKeepsEveryAttempt crashes on A, restarts into a
// cleanup that does not confirm the delete, then crashes again on attempt
// B. The final restart stops and deletes both. A single overwritten column
// loses A and this test fails.
func TestCursorCloudRestartKeepsEveryAttempt(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	agentA := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-a", true)
	fake.DeleteFailures = 3

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.Error(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	_, err := fake.GetAgent(t.Context(), agentA.ID)
	require.NoError(t, err)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "failed delete must not mark cleanup done")

	_, err = tc.DB.Exec(
		`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`,
		testWorkerID, job.ID,
	)
	require.NoError(t, err)
	agentB := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-b", true)
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	creates = cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	cursorCloudAssertNoRelaunch(t, tc, job.ID, fake, creates)
	for _, id := range []string{agentA.ID, agentB.ID} {
		assert.Contains(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodStop), id)
		assert.Contains(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete), id)
		_, getErr := fake.GetAgent(t.Context(), id)
		require.Error(t, getErr)
	}
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, storage.CloudLaunchInterruptedError, updated.Error)
}

// TestCursorCloudRestartRetires404 marks cleanup done when delete reports
// the agent is already gone, and not when delete fails.
func TestCursorCloudRestartRetires404(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	created := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-404", true)
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodDelete: &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
		},
	}

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	_, err = fake.GetAgent(t.Context(), created.ID)
	require.NoError(t, err, "404 means the server already dropped the agent; cleanup must not invent a second delete")
}

// TestCursorCloudRestartKeepsUnlistedToken is lag 1. The first restart lists
// before the token-only agent exists, so the row stays uncleaned. The next
// restart sees it and deletes it, and does not create another agent.
func TestCursorCloudRestartKeepsUnlistedToken(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-lag"
	cursorCloudBeginToken(t, tc, job.ID, token)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.Error(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	created := cursorCloudShowToken(t, tc, fake, sha, token)
	creates = cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	cursorCloudAssertNoRelaunch(t, tc, job.ID, fake, creates)
	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	_, err = fake.GetAgent(t.Context(), created.ID)
	require.Error(t, err)
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestCursorCloudRestartKeepsTokenAcrossTwoLags is lag 2. Two restarts both
// miss the token-only agent. The row stays uncleaned, then the list catches
// up and that same row is deleted.
func TestCursorCloudRestartKeepsTokenAcrossTwoLags(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-two-lags"
	cursorCloudBeginToken(t, tc, job.ID, token)

	for range 2 {
		require.Error(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	}

	created := cursorCloudShowToken(t, tc, fake, sha, token)
	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	_, err := fake.GetAgent(t.Context(), created.ID)
	require.Error(t, err)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestCursorCloudRestartReportsUnlistedOrphan ages a token-only miss past
// the sweep window. The row stays uncleaned and the job error and log name
// the orphan risk. A young miss does not.
func TestCursorCloudRestartReportsUnlistedOrphan(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudBeginToken(t, tc, job.ID, "launch-token-bound")
	logs := cursorCloudCaptureLog(t)

	require.Error(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NotContains(t, logs.String(), "orphan risk")

	cursorCloudAgeLaunch(t, tc.DB, job.ID)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, logs.String(), "orphan risk")
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, updated.Error, "orphan risk")
	assert.Contains(t, updated.Error, storage.CloudLaunchInterruptedError)
}

// TestCursorCloudRestartAgesCrashBeforePost is a token saved before the
// create POST. There is no VM. Once the row is older than the sweep window,
// restart reports the orphan risk and does not mark it cleaned.
func TestCursorCloudRestartAgesCrashBeforePost(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudBeginToken(t, tc, job.ID, "launch-token-no-post")
	cursorCloudAgeLaunch(t, tc.DB, job.ID)
	logs := cursorCloudCaptureLog(t)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodStop))
	assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, logs.String(), "orphan risk")
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, updated.Error, "orphan risk")
}

// TestCursorCloudRestartKeepsEmptyToken is the empty-token pin. Restart
// must not mark a ” or NULL token cleaned. The orphan warning stays, and
// no delete runs, because there is no marker that can prove the VM is gone.
func TestCursorCloudRestartKeepsEmptyToken(t *testing.T) {
	for _, tokenNull := range []bool{false, true} {
		name := "empty"
		if tokenNull {
			name = "null"
		}
		t.Run(name, func(t *testing.T) {
			tc, fake := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			if tokenNull {
				cursorCloudAllowNullToken(t, tc.DB)
				_, err := tc.DB.Exec(`INSERT INTO review_job_cloud_launches
					(job_id, launch_token, repo_identity, created_at)
					VALUES (?, NULL, ?, datetime('now'))`, job.ID, cursorCloudRestartRepo)
				require.NoError(t, err)
			} else {
				// BeginCloudLaunch refuses an empty token because the pending
				// note is keyed by token. The row is inserted the way a NULL
				// token is: already stored, with nothing to sweep.
				_, err := tc.DB.Exec(`INSERT INTO review_job_cloud_launches
					(job_id, launch_token, repo_identity, created_at)
					VALUES (?, '', ?, datetime('now'))`, job.ID, cursorCloudRestartRepo)
				require.NoError(t, err)
				_, err = tc.DB.Exec(`UPDATE review_jobs SET agent_invoked = 1 WHERE id = ?`, job.ID)
				require.NoError(t, err)
			}
			logs := cursorCloudCaptureLog(t)

			require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
			n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			assert.Contains(t, logs.String(), "orphan risk")
			assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
			updated, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Contains(t, updated.Error, "orphan risk")

			var stored sql.NullString
			var cleaned int
			require.NoError(t, tc.DB.QueryRow(
				`SELECT launch_token, cleaned FROM review_job_cloud_launches WHERE job_id = ?`, job.ID,
			).Scan(&stored, &cleaned))
			assert.Equal(t, 0, cleaned)
			assert.Equal(t, !tokenNull, stored.Valid)
			if stored.Valid {
				assert.Empty(t, stored.String)
			}
		})
	}
}

// cursorCloudAllowNullToken rebuilds the launch table in this test database
// so a NULL token can be inserted. Production migration of that column is
// not this test's job.
func cursorCloudAllowNullToken(t *testing.T, db *storage.DB) {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE review_job_cloud_launches_nullable (
		id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL, launch_token TEXT,
		agent_id TEXT NOT NULL DEFAULT '', cleaned INTEGER NOT NULL DEFAULT 0,
		repo_identity TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO review_job_cloud_launches_nullable
		(id, job_id, launch_token, agent_id, cleaned, repo_identity, created_at)
		SELECT id, job_id, launch_token, agent_id, cleaned, repo_identity, created_at
		FROM review_job_cloud_launches`)
	require.NoError(t, err)
	_, err = db.Exec(`DROP TABLE review_job_cloud_launches`)
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE review_job_cloud_launches_nullable RENAME TO review_job_cloud_launches`)
	require.NoError(t, err)
}

// TestCursorCloudRestartUsesStoredRepoIdentity changes origin after the
// launch row is written. Cleanup still deletes the agent from the create-time
// repository and leaves an agent on the new remote alone.
func TestCursorCloudRestartUsesStoredRepoIdentity(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-repo"
	const moved = "https://example.com/acme/moved.git"
	cursorCloudBeginToken(t, tc, job.ID, token)
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	ours := cursorCloudSeedAgent(t, fake, sha, marker)
	tc.GitRepo.Run("remote", "set-url", "origin", moved)
	decoy, err := fake.CreateAgent(t.Context(), cursorcloud.CreateAgentRequest{
		Repo: moved, Ref: sha, Prompt: "review\n" + marker + "\n",
	})
	require.NoError(t, err)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	_, err = fake.GetAgent(t.Context(), ours.ID)
	require.Error(t, err)
	got, err := fake.GetAgent(t.Context(), decoy.ID)
	require.NoError(t, err)
	assert.Equal(t, decoy.ID, got.ID)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

type cursorCloudPanicWriter struct{}

func (cursorCloudPanicWriter) Write([]byte) (int, error) { panic("output exploded") }

// TestCursorCloudPanicDeleteFailureLeavesLaunch is the c7 pin. Cleanup output
// panics and the delete fails. The delete note is still logged, and the
// launch row is not marked cleaned.
func TestCursorCloudPanicDeleteFailureLeavesLaunch(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodStop: errors.New("stop exploded"),
		cursorcloudtest.MethodDelete: &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "delete refused",
		},
	}
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	cloud.DaemonID = machineID.String() //nolint:forbidigo // launch marker is prompt text
	cloud.BindLaunch = func(token, repoURL string) error {
		return tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, repoURL)
	}
	cloud.BindAgent = func(token, id string) error {
		return tc.DB.AttachCloudAgent(job.ID, token, id)
	}

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	require.PanicsWithValue(t, "output exploded", func() {
		_, _ = cloud.Review(context.Background(), tc.TmpDir, sha, "prompt", cursorCloudPanicWriter{})
	})

	assert.Contains(t, logs.String(), "cursor-cloud: delete agent")
	assert.Equal(t, 1, cursorCloudCallCount(fake, cursorcloudtest.MethodDelete))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	ids := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
	require.NotEmpty(t, ids)
	_, err = fake.GetAgent(t.Context(), ids[0])
	require.NoError(t, err)
}

type cursorCloudStopPanicWriter struct{}

func (cursorCloudStopPanicWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("stop agent")) {
		panic("output exploded")
	}
	return len(p), nil
}

// TestCursorCloudStopErrorWritePanicStillDeletes panics at the stop-error
// note, after delete. The status line is written and does not panic.
func TestCursorCloudStopErrorWritePanicStillDeletes(t *testing.T) {
	fake := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		Final:        "ok",
		MethodErrors: map[string]error{cursorcloudtest.MethodStop: errors.New("stop exploded")},
	}
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	sha := "0123456789abcdef0123456789abcdef01234567"
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		_, _ = cloud.Review(context.Background(), t.TempDir(), sha, "prompt", cursorCloudStopPanicWriter{})
	}()
	assert.Equal(t, "output exploded", panicked)
	assert.Equal(t, 1, cursorCloudCallCount(fake, cursorcloudtest.MethodDelete))
}

// TestCursorCloudDeleteFailureSurfacesOrphanRisk is a delete that fails all
// of its attempts. The launch stays uncleaned and the job error and log show
// the orphan risk in this process, not on a later start.
func TestCursorCloudDeleteFailureSurfacesOrphanRisk(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = "ok"
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodDelete: &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusInternalServerError, Message: "unavailable",
		},
	}
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	bound := tc.Pool.bindCursorCloudLaunch(testWorkerID, job, cloud)
	logs := cursorCloudCaptureLog(t)

	_, err := bound.Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	assert.Equal(t, 3, cursorCloudCallCount(fake, cursorcloudtest.MethodDelete))
	assert.Contains(t, logs.String(), "orphan risk")
	assert.Contains(t, logs.String(), "pending cleanup")
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, updated.Error, "orphan risk")
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestCursorCloudRestartFailsSynthesisBackupLaunch crashes a synthesis job
// whose backup is cursor-cloud. Restart fails it, does not create again, and
// keeps the log.
func TestCursorCloudRestartFailsSynthesisBackupLaunch(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	_, _, synth := enqueuePanelRun(t, tc, "cloud-backup", []memberSpec{{name: "m0", agent: "test"}})
	_, err := tc.DB.Exec(
		`UPDATE review_jobs SET status = 'running', worker_id = ?, agent = ?, backup_agent = ? WHERE id = ?`,
		testWorkerID, "test", agent.CursorCloudHost, synth.ID,
	)
	require.NoError(t, err)
	cursorCloudPersistAttempt(t, tc, fake, synth.ID, "launch-token-synth", true)
	require.NoError(t, os.MkdirAll(JobLogDir(), 0o700))
	const logLine = "cursor-cloud: created agent bc_synth\n"
	require.NoError(t, os.WriteFile(JobLogPath(synth.ID), []byte(logLine), 0o600))

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	updated := tc.assertJobStatus(t, synth.ID, storage.JobStatusFailed)
	assert.Equal(t, storage.CloudLaunchInterruptedError, updated.Error)
	body, err := os.ReadFile(JobLogPath(synth.ID))
	require.NoError(t, err)
	assert.Equal(t, logLine, string(body))
	claimed, err := tc.DB.ClaimJob(testWorkerID + "-synth")
	require.NoError(t, err)
	if claimed != nil {
		assert.NotEqual(t, synth.ID, claimed.ID)
	}
}

func TestCursorCloudUnboundLaunchIsRefused(t *testing.T) {
	_, err := (&agent.CursorCloudAgent{}).Review(
		context.Background(), t.TempDir(),
		"0123456789abcdef0123456789abcdef01234567", "prompt", nil,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launch binding")
}

// TestCursorCloudRestartUsesStoredIdentityWithoutRemote removes origin after
// the token-only row is stored. Recovery deletes that agent with the stored
// repository and does not sweep redacted-url.
func TestCursorCloudRestartUsesStoredIdentityWithoutRemote(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-no-remote"
	cursorCloudBeginToken(t, tc, job.ID, token)
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	ours := cursorCloudSeedAgent(t, fake, sha, marker)
	tc.GitRepo.Run("remote", "remove", "origin")

	lists := cursorCloudCallCount(fake, cursorcloudtest.MethodList)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	_, err = fake.GetAgent(t.Context(), ours.ID)
	require.Error(t, err)
	_, err = agent.DeleteCursorCloudLaunchByMarker(
		context.Background(), fake, machineID.String(), token, config.RedactedURL, //nolint:forbidigo // launch marker is prompt text
	)
	require.Error(t, err)
	assert.Equal(t, lists+1, cursorCloudCallCount(fake, cursorcloudtest.MethodList), "redacted-url must not start another sweep")
}

// TestCursorCloudRestartNotesLegacyGoneRemote is the pin for a token-only
// row stored before repo_identity existed. The checkout remote is gone, so
// recovery cannot list. Every start surfaces the orphan marker and leaves
// the row uncleaned. A silent success, or a sweep error with no marker,
// fails this test.
func TestCursorCloudRestartNotesLegacyGoneRemote(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudLegacyToken(t, tc, job.ID, "launch-token-legacy-gone")
	tc.GitRepo.Run("remote", "remove", "origin")
	logs := cursorCloudCaptureLog(t)

	cursorCloudAssertLegacySweepOrphan(t, tc, fake, job.ID, logs)
}

// TestCursorCloudRestartNotesLegacyUnusableRepo is the same pin when the
// stored checkout itself cannot be read. repo_identity is empty and the
// repo path is not a git checkout, so there is still nothing to list.
func TestCursorCloudRestartNotesLegacyUnusableRepo(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudLegacyToken(t, tc, job.ID, "launch-token-legacy-unusable")
	_, err := tc.DB.Exec(`UPDATE repos SET root_path = ? WHERE id = ?`, t.TempDir(), job.RepoID)
	require.NoError(t, err)
	logs := cursorCloudCaptureLog(t)

	cursorCloudAssertLegacySweepOrphan(t, tc, fake, job.ID, logs)
}

// TestCursorCloudRestartDeletesLegacyRowWithLiveRemote is the delete half of
// the legacy pin. repo_identity is empty, and origin still names the
// repository the agent was launched on. Restart deletes that agent. A second
// start leaves it cleaned. Skipping the row leaves the agent in place.
func TestCursorCloudRestartDeletesLegacyRowWithLiveRemote(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-legacy-live"
	cursorCloudLegacyToken(t, tc, job.ID, token)
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	created := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(machineID.String(), token)) //nolint:forbidigo // launch marker is prompt text

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	_, err = fake.GetAgent(t.Context(), created.ID)
	require.Error(t, err)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	_, err = fake.GetAgent(t.Context(), created.ID)
	require.Error(t, err)
}

// TestCursorCloudRestartDeletesAcceptedFileHostLaunch is the round trip for
// an odd shape both validators accept. The launch POSTs file://localhost/...,
// and the restart sweep deletes that agent from the stored identity.
func TestCursorCloudRestartDeletesAcceptedFileHostLaunch(t *testing.T) {
	const raw = "file://localhost/tmp/repo.git"
	cleaned := config.StripURLCredentials(raw)
	require.NotEqual(t, config.RedactedURL, cleaned)
	require.NotEmpty(t, config.RemoteIdentity(cleaned))
	require.True(t, agent.CursorCloudRepoUsable(cleaned))

	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	fake := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		Final:        "ok",
		MethodErrors: map[string]error{
			cursorcloudtest.MethodDelete: &cursorcloud.APIError{
				Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "refused",
			},
		},
	}
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: raw}
	cloud.DaemonID = machineID.String() //nolint:forbidigo // launch marker is prompt text
	cloud.BindLaunch = func(token, repoURL string) error {
		return tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, repoURL)
	}

	_, err = cloud.Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	assert.Equal(t, 1, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	listed, err := fake.ListAgents(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, cleaned, listed[0].SourceRepository)
	var stored string
	require.NoError(t, tc.DB.QueryRow(`SELECT repo_identity FROM review_job_cloud_launches WHERE job_id = ?`, job.ID).Scan(&stored))
	assert.Equal(t, cleaned, stored)

	fake.MethodErrors = nil
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	_, err = fake.GetAgent(t.Context(), listed[0].ID)
	require.Error(t, err)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestCursorCloudUpdateInterruptDoesNotRelaunch(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-update", true)
	tc.Pool.InterruptJobsForUpdate([]int64{job.ID})

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	handled := tc.Pool.handleUpdateInterruption(context.Background(), testWorkerID, job)
	assert.True(t, handled)
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, storage.CloudLaunchInterruptedError, updated.Error)
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	claimed, err := tc.DB.ClaimJob(testWorkerID + "-update")
	require.NoError(t, err)
	if claimed != nil {
		assert.NotEqual(t, job.ID, claimed.ID)
	}
}

// TestDoneContextPersistsNoCloudLaunch pins the order of the pre-create
// checks. A context that is already done must not insert a launch row and
// must not call create. Moving the context check to after BindLaunch leaves
// a row and this test fails.
func TestDoneContextPersistsNoCloudLaunch(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	bound := tc.Pool.bindCursorCloudLaunch(testWorkerID, job, cloud)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	_, err := bound.Review(ctx, tc.TmpDir, sha, "review the commit", nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, before, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	n, nerr := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, nerr)
	assert.Equal(t, 0, n)
}

// TestCursorCloudRestartReaps429Create is a create that stores an agent and
// then returns 429. The in-process reconcile lists once and stops on that
// 429, so the agent is still there. The startup sweep lists again: a list
// 429 leaves the token row uncleaned. A later miss keeps the row. The next
// list deletes the agent and does not create another.
func TestCursorCloudRestartReaps429Create(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	tc, fake := cursorCloudRestartFixture(t)
	provider := &create429Cloud{Fake: fake}
	provider.setListErr(&cursorcloud.APIError{
		Op: "list agents", StatusCode: http.StatusTooManyRequests, Message: "too many requests",
	})
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	cloud := &agent.CursorCloudAgent{Client: provider, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	agent.Register(cloud)
	t.Cleanup(func() { agent.Register(prev) })

	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	tc.Pool.processJob(testWorkerID, job)

	require.NotEmpty(t, provider.createdID)
	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	assert.Equal(t, 1, creates)
	_, err = fake.GetAgent(t.Context(), provider.createdID)
	require.NoError(t, err, "a 429 create that stored an agent must still be reachable before restart")
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "token-only row stays uncleaned until its marked agent is found and deleted")

	err = recoverStaleJobs(context.Background(), tc.DB, provider)
	require.Error(t, err)
	require.ErrorContains(t, err, "too many requests")
	assert.Equal(t, 2, provider.listCalls(), "create reconcile and the startup sweep each consume one list 429")
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a list 429 leaves the token row uncleaned")

	provider.setListErr(nil)
	provider.setHide(true)
	require.Error(t, recoverStaleJobs(context.Background(), tc.DB, provider))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	_, err = fake.GetAgent(t.Context(), provider.createdID)
	require.NoError(t, err)
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a later list that misses the agent keeps the token row uncleaned")

	provider.setHide(false)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, provider))
	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	_, err = fake.GetAgent(t.Context(), provider.createdID)
	require.Error(t, err)
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// create429Cloud stores the launch, then answers 429. listErr withholds the
// agent from the in-process reconcile. hide is a later list that does not
// contain it.
type create429Cloud struct {
	*cursorcloudtest.Fake
	mu        sync.Mutex
	listErr   error
	hide      bool
	createdID string
	lists     int
}

func (c *create429Cloud) listCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists
}

func (c *create429Cloud) setListErr(err error) {
	c.mu.Lock()
	c.listErr = err
	c.mu.Unlock()
}

func (c *create429Cloud) setHide(hide bool) {
	c.mu.Lock()
	c.hide = hide
	c.mu.Unlock()
}

func (c *create429Cloud) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	created, err := c.Fake.CreateAgent(ctx, req)
	if err != nil {
		return cursorcloud.Agent{}, err
	}
	c.mu.Lock()
	c.createdID = created.ID
	c.mu.Unlock()
	return cursorcloud.Agent{}, &cursorcloud.APIError{
		Op: "create agent", StatusCode: http.StatusTooManyRequests, Message: "too many requests",
	}
}

func (c *create429Cloud) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	c.mu.Lock()
	c.lists++
	listErr := c.listErr
	hide := c.hide
	c.mu.Unlock()
	if listErr != nil {
		return nil, listErr
	}
	if hide {
		return nil, nil
	}
	return c.Fake.ListAgents(ctx)
}

func cursorCloudRestartFixture(t *testing.T) (*workerTestContext, *cursorcloudtest.Fake) {
	t.Helper()
	tc := newWorkerTestContext(t, 1)
	tc.GitRepo.Run("remote", "add", "origin", cursorCloudRestartRepo)
	fake := &cursorcloudtest.Fake{}
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	agent.Register(cloud)
	t.Cleanup(func() { agent.Register(prev) })
	return tc, fake
}

func cursorCloudPersistAttempt(t *testing.T, tc *workerTestContext, fake *cursorcloudtest.Fake, jobID int64, token string, withID bool) cursorcloud.Agent {
	t.Helper()
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	created := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(machineID.String(), token)) //nolint:forbidigo // launch marker is prompt text
	require.NoError(t, tc.DB.BeginCloudLaunch(jobID, testWorkerID, token, cursorCloudRestartRepo))
	_, err = tc.DB.Exec(`UPDATE review_jobs SET agent_invoked = 1 WHERE id = ?`, jobID)
	require.NoError(t, err)
	if withID {
		require.NoError(t, tc.DB.AttachCloudAgent(jobID, token, created.ID))
	}
	return created
}

func cursorCloudSeedAgent(t *testing.T, fake *cursorcloudtest.Fake, sha, prompt string) cursorcloud.Agent {
	t.Helper()
	created, err := fake.CreateAgent(t.Context(), cursorcloud.CreateAgentRequest{
		Repo: cursorCloudRestartRepo, Ref: sha, Prompt: "review\n" + prompt + "\n",
	})
	require.NoError(t, err)
	return created
}

func cursorCloudAssertNoRelaunch(t *testing.T, tc *workerTestContext, jobID int64, fake *cursorcloudtest.Fake, createsBefore int) {
	t.Helper()
	current, err := tc.DB.GetJobByID(jobID)
	require.NoError(t, err)
	if current.Status == storage.JobStatusQueued {
		claimed, claimErr := tc.DB.ClaimJob(testWorkerID + "-relaunch")
		require.NoError(t, claimErr)
		if claimed != nil && claimed.ID == jobID {
			tc.Pool.processJob(testWorkerID+"-relaunch", claimed)
		}
	}
	assert.Equal(t, createsBefore, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
}

func cursorCloudCallCount(fake *cursorcloudtest.Fake, method string) int {
	n := 0
	for _, call := range fake.Calls() {
		if call.Method == method {
			n++
		}
	}
	return n
}

func cursorCloudLegacyToken(t *testing.T, tc *workerTestContext, jobID int64, token string) {
	t.Helper()
	cursorCloudBeginToken(t, tc, jobID, token)
	_, err := tc.DB.Exec(`UPDATE review_job_cloud_launches SET repo_identity = '' WHERE job_id = ?`, jobID)
	require.NoError(t, err)
}

func cursorCloudAssertLegacySweepOrphan(t *testing.T, tc *workerTestContext, fake *cursorcloudtest.Fake, jobID int64, logs *bytes.Buffer) {
	t.Helper()
	assert := assert.New(t)
	for range 2 {
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(jobID)
		require.NoError(t, err)
		assert.Equal(1, n)
		updated, err := tc.DB.GetJobByID(jobID)
		require.NoError(t, err)
		assert.Equal(storage.JobStatusFailed, updated.Status)
		assert.Contains(updated.Error, "orphan risk")
		assert.Contains(updated.Error, storage.CloudLaunchInterruptedError)
	}
	assert.Contains(logs.String(), "orphan risk")
	assert.Empty(cursorCloudCallIDs(fake, cursorcloudtest.MethodList))
	assert.Empty(cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
}

func cursorCloudBeginToken(t *testing.T, tc *workerTestContext, jobID int64, token string) {
	t.Helper()
	require.NoError(t, tc.DB.BeginCloudLaunch(jobID, testWorkerID, token, cursorCloudRestartRepo))
	_, err := tc.DB.Exec(`UPDATE review_jobs SET agent_invoked = 1 WHERE id = ?`, jobID)
	require.NoError(t, err)
}

func cursorCloudShowToken(t *testing.T, tc *workerTestContext, fake *cursorcloudtest.Fake, sha, token string) cursorcloud.Agent {
	t.Helper()
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	return cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(machineID.String(), token)) //nolint:forbidigo // launch marker is prompt text
}

func cursorCloudAgeLaunch(t *testing.T, db *storage.DB, jobID int64) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_job_cloud_launches SET created_at = datetime('now', '-25 hours') WHERE job_id = ?`, jobID)
	require.NoError(t, err)
}

func cursorCloudCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &logs
}

func cursorCloudCallIDs(fake *cursorcloudtest.Fake, method string) []string {
	var ids []string
	for _, call := range fake.Calls() {
		if call.Method == method {
			ids = append(ids, call.ID)
		}
	}
	return ids
}
