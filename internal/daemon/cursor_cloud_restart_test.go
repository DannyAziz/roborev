package daemon

import (
	"context"
	"net/http"
	"os"
	"testing"
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
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token))

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
	require.NoError(t, tc.DB.BeginCloudLaunch(jobID, testWorkerID, token))
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

func cursorCloudCallIDs(fake *cursorcloudtest.Fake, method string) []string {
	var ids []string
	for _, call := range fake.Calls() {
		if call.Method == method {
			ids = append(ids, call.ID)
		}
	}
	return ids
}
