package daemon

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const cursorCloudRestartRepo = "https://example.com/acme/repo.git"

// TestCursorCloudRestartStopsPersistedAgent: the daemon died with a saved
// agent id. Restart fails the job without relaunching, deletes the id, and
// cleans the row in the same pass once the marker list is empty.
func TestCursorCloudRestartStopsPersistedAgent(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	created := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-a", true)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.NoError(t, cursorCloudRecover(t, tc, fake))
	cursorCloudAssertNoRelaunch(t, tc, job.ID, fake, creates)

	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodStop))
	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, storage.CloudLaunchInterruptedError)
	assert.Equal(t, 0, cursorCloudUncleaned(t, tc, job.ID))
}

// TestCursorCloudRestartSweepsMarkerWithoutID: the crash fell between the
// create and saving the id. The marker sweep deletes the VM and that sweep
// cleans the row. Agents with another daemon or token are left alone.
func TestCursorCloudRestartSweepsMarkerWithoutID(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	created := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-no-id", false)
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	otherDaemon := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(uuid.New().String(), "launch-token-no-id")) //nolint:forbidigo // foreign daemon id in the marker
	otherToken := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(machineID.String(), "someone-elses-token"))  //nolint:forbidigo // launch marker is prompt text

	require.NoError(t, cursorCloudRecover(t, tc, fake))

	assert.Equal(t, []string{created.ID}, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	for _, id := range []string{otherDaemon.ID, otherToken.ID} {
		_, getErr := fake.GetAgent(t.Context(), id)
		require.NoError(t, getErr)
	}
	assert.Equal(t, 0, cursorCloudUncleaned(t, tc, job.ID))
}

// TestCursorCloudRestartUnlistedAmbiguousCreate: no id and no listed VM.
// A launch younger than the reconcile window may not be listed yet and
// stays uncleaned. An older one was never created and is cleaned.
func TestCursorCloudRestartUnlistedAmbiguousCreate(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudBeginToken(t, tc, job.ID, "launch-token-unlisted")

	require.NoError(t, cursorCloudRecover(t, tc, fake))
	assert.Equal(t, 1, cursorCloudUncleaned(t, tc, job.ID))

	_, err := tc.DB.Exec(`UPDATE review_job_cloud_launches SET created_at = datetime('now', '-1 hour') WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, cursorCloudRecover(t, tc, fake))
	assert.Equal(t, 0, cursorCloudUncleaned(t, tc, job.ID))
	assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
}

// TestCursorCloudRestartSkipsConfirmedDelete: the in-process delete of the
// id was confirmed but its marker list failed. Restart does not delete the
// id again; it sweeps the marker and cleans.
func TestCursorCloudRestartSkipsConfirmedDelete(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudBeginToken(t, tc, job.ID, "launch-token-deleted")
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, "launch-token-deleted", "bc_gone"))
	require.NoError(t, tc.DB.MarkCloudAgentDeleted(job.ID, "launch-token-deleted"))

	require.NoError(t, cursorCloudRecover(t, tc, fake))

	assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodStop))
	assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete))
	assert.Equal(t, 0, cursorCloudUncleaned(t, tc, job.ID))
}

// TestCursorCloudRestartListsOncePerRepo: several rows in one repository
// share one agent list.
func TestCursorCloudRestartListsOncePerRepo(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	var jobs []int64
	for _, token := range []string{"launch-token-1", "launch-token-2", "launch-token-3"} {
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudPersistAttempt(t, tc, fake, job.ID, token, false)
		jobs = append(jobs, job.ID)
	}
	counting := &countListCloud{Fake: fake}

	require.NoError(t, cursorCloudRecover(t, tc, counting))

	assert.Equal(t, int32(1), counting.lists.Load())
	assert.Len(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete), 3)
	for _, id := range jobs {
		assert.Equal(t, 0, cursorCloudUncleaned(t, tc, id))
	}
}

// TestCursorCloudRestartDeleteFailureStaysUncleaned: a delete that does not
// finish leaves the row for the next start.
func TestCursorCloudRestartDeleteFailureStaysUncleaned(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-refused", true)
	fake.MethodErrors = map[string]error{cursorcloudtest.MethodDelete: &cursorcloud.APIError{
		Op: "delete agent", StatusCode: http.StatusForbidden, Message: "forbidden",
	}}

	require.Error(t, cursorCloudRecover(t, tc, fake))
	assert.Equal(t, 1, cursorCloudUncleaned(t, tc, job.ID))
}

// TestServerCloudLaunchRecoveryRunsInBackground: startup only reads the
// rows. The sweep runs after the call returns.
func TestServerCloudLaunchRecoveryRunsInBackground(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-server", true)
	block := make(chan struct{})
	srv := &Server{db: tc.DB, cloudLaunchClient: &blockListCloud{Fake: fake, block: block}}

	srv.startCloudLaunchRecovery(t.Context())
	assert.Equal(t, 1, cursorCloudUncleaned(t, tc, job.ID))
	close(block)

	// The sweep writes SQLite from its own goroutine.
	assert.Eventually(t, func() bool {
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		return err == nil && n == 0
	}, 5*time.Second, 10*time.Millisecond)
}

type countListCloud struct {
	*cursorcloudtest.Fake
	lists atomic.Int32
}

func (c *countListCloud) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	c.lists.Add(1)
	return c.Fake.ListAgents(ctx)
}

type blockListCloud struct {
	*cursorcloudtest.Fake
	block chan struct{}
}

func (c *blockListCloud) StopAgent(ctx context.Context, id string) error {
	<-c.block
	return c.Fake.StopAgent(ctx, id)
}

// cursorCloudRecover runs what a daemon start runs: stale jobs are reset,
// then the uncleaned launches are swept.
func cursorCloudRecover(t *testing.T, tc *workerTestContext, client cursorcloud.Client) error {
	t.Helper()
	require.NoError(t, tc.DB.ResetStaleJobs())
	rows, err := tc.DB.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	if len(rows) == 0 {
		return nil
	}
	return recoverCloudLaunches(t.Context(), tc.DB, client, rows)
}

func cursorCloudUncleaned(t *testing.T, tc *workerTestContext, jobID int64) int {
	t.Helper()
	n, err := tc.DB.CountUncleanedCloudLaunches(jobID)
	require.NoError(t, err)
	return n
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
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	created := cursorCloudShowToken(t, tc, fake, sha, token)
	cursorCloudBeginToken(t, tc, jobID, token)
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
	return len(cursorCloudCallIDs(fake, method))
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

func cursorCloudCaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &logs
}
