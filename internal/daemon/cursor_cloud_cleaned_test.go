package daemon

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const cursorCloudPassJSON = `{"schema_version":2,"summary":"No issues found.","verdict":"pass","findings":[]}`

// cloudCreateScript creates the requested agent, optional duplicates that
// carry the same prompt, then returns createErr. failDelete is the 1-based
// DeleteAgent call that refuses; 4xx other than 404 is not retried.
type cloudCreateScript struct {
	*cursorcloudtest.Fake
	createErr  error
	extra      int
	failDelete int
	deletes    int
}

func (c *cloudCreateScript) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	ag, err := c.Fake.CreateAgent(ctx, req)
	if err != nil {
		return ag, err
	}
	for range c.extra {
		if _, err := c.Fake.CreateAgent(ctx, req); err != nil {
			return ag, err
		}
	}
	if c.createErr != nil {
		return ag, c.createErr
	}
	return ag, nil
}

func (c *cloudCreateScript) DeleteAgent(ctx context.Context, id string) error {
	c.deletes++
	if c.failDelete > 0 && c.deletes == c.failDelete {
		return &cursorcloud.APIError{Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "refused"}
	}
	return c.Fake.DeleteAgent(ctx, id)
}

func newBoundCloud(t *testing.T, tc *workerTestContext, job *storage.ReviewJob, client cursorcloud.Client) agent.Agent {
	t.Helper()
	cloud := &agent.CursorCloudAgent{Client: client, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	return tc.Pool.bindCursorCloudLaunch(testWorkerID, job, cloud)
}

func assertLaunchCleaned(t *testing.T, tc *workerTestContext, jobID int64, fake *cursorcloudtest.Fake, goneID string) {
	t.Helper()
	n, err := tc.DB.CountUncleanedCloudLaunches(jobID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	if goneID != "" {
		_, err = fake.GetAgent(t.Context(), goneID)
		require.Error(t, err)
	}
}

// TestCursorCloud429ReapMarksCleaned is C0/C0A. A 429 whose reconcile finds
// and deletes the VM marks the launch cleaned. Aging that row must not write
// an orphan note. Leaving cleaned=0 fails the count, then the aged restart
// writes the note.
func TestCursorCloud429ReapMarksCleaned(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{}
	script := &cloudCreateScript{
		Fake:      fake,
		createErr: &cursorcloud.APIError{Op: "create agent", StatusCode: http.StatusTooManyRequests, Message: "too many requests"},
	}
	_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	deleted := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
	require.NotEmpty(t, deleted)
	assertLaunchCleaned(t, tc, job.ID, fake, deleted[0])

	cursorCloudAgeLaunch(t, tc.DB, job.ID)
	logs := cursorCloudCaptureLog(t)
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.NotContains(t, logs.String(), "orphan risk")
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.NotContains(t, updated.Error, "orphan risk")
	assertLaunchCleaned(t, tc, job.ID, fake, "")
}

// TestCursorCloudInFlightCreateReapMarksCleaned is J2. The POST was sent, the
// response never arrived, and the VM is already listed. Reconcile deletes it
// and marks the launch cleaned, same as the 429 reap.
func TestCursorCloudInFlightCreateReapMarksCleaned(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{}
	script := &cloudCreateScript{Fake: fake, createErr: context.DeadlineExceeded}
	_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	deleted := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
	require.NotEmpty(t, deleted)
	assertLaunchCleaned(t, tc, job.ID, fake, deleted[0])
}

// TestCursorCloudSuccessDeleteMarksCleaned is the in-process success path.
// Nil and 404 both confirm the delete, so the launch is cleaned before the
// process can crash between delete and the next start.
func TestCursorCloudSuccessDeleteMarksCleaned(t *testing.T) {
	for _, mode := range []string{"nil", "404"} {
		t.Run(mode, func(t *testing.T) {
			tc, _ := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
			if mode == "404" {
				fake.MethodErrors = map[string]error{
					cursorcloudtest.MethodDelete: &cursorcloud.APIError{
						Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
					},
				}
			}
			text, err := newBoundCloud(t, tc, job, fake).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
			require.NoError(t, err)
			assert.Equal(t, "ok", text)
			n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
			require.NoError(t, err)
			assert.Equal(t, 0, n)
			if mode == "nil" {
				deleted := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
				require.NotEmpty(t, deleted)
				_, err = fake.GetAgent(t.Context(), deleted[0])
				require.Error(t, err)
			}
		})
	}
}

// TestCursorCloudDuplicateCreateBeforeCleaned is the provider-side duplicate.
// Both VMs must be deleted with nil or 404 before the launch is cleaned.
// A refused second delete leaves the row uncleaned and that VM in place.
func TestCursorCloudDuplicateCreateBeforeCleaned(t *testing.T) {
	t.Run("both deleted", func(t *testing.T) {
		tc, _ := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
		script := &cloudCreateScript{Fake: fake, extra: 1}
		text, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", text)
		for _, id := range []string{"bc_fake_1", "bc_fake_2"} {
			_, getErr := fake.GetAgent(t.Context(), id)
			require.Error(t, getErr, id)
		}
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
	t.Run("second delete refused", func(t *testing.T) {
		tc, _ := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
		script := &cloudCreateScript{Fake: fake, extra: 1, failDelete: 2}
		text, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", text)
		_, err = fake.GetAgent(t.Context(), "bc_fake_1")
		require.Error(t, err)
		got, err := fake.GetAgent(t.Context(), "bc_fake_2")
		require.NoError(t, err)
		assert.Equal(t, "bc_fake_2", got.ID)
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})
}

// TestCursorCloudRestartDuplicateWithoutRepoStaysUncleaned is restart.go's
// unusable-repository branch. The saved id's delete is confirmed, and a
// second VM with the same token is still running. Cleaning the row there
// drops the duplicate.
func TestCursorCloudRestartDuplicateWithoutRepoStaysUncleaned(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-dup"
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	saved := cursorCloudSeedAgent(t, fake, sha, marker)
	dup := cursorCloudSeedAgent(t, fake, sha, marker)
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, ""))
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, saved.ID))
	tc.GitRepo.Run("remote", "remove", "origin")

	err = recoverStaleJobs(context.Background(), tc.DB, fake)
	_, getErr := fake.GetAgent(t.Context(), saved.ID)
	require.Error(t, getErr)
	got, getErr := fake.GetAgent(t.Context(), dup.ID)
	require.NoError(t, getErr)
	assert.Equal(t, dup.ID, got.ID)
	n, countErr := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, countErr)
	assert.Equal(t, 1, n)
	require.Error(t, err)
}

// TestCursorCloudRestartCleansAfterAppliedDelete is C2. The delete already
// removed the VM, then the process died before marking cleaned. Restart's
// 404 for the saved id, plus a list that does not show another VM, marks it.
func TestCursorCloudRestartCleansAfterAppliedDelete(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-c2"
	cursorCloudBeginToken(t, tc, job.ID, token)
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, "bc_already_gone"))
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodDelete: &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
		},
	}

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestCursorCloudRestartResweepsAgedOrphan pins the 24h miss. The note does
// not mark the row cleaned, so the next start lists it again.
func TestCursorCloudRestartResweepsAgedOrphan(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudBeginToken(t, tc, job.ID, "launch-token-resweep")
	cursorCloudAgeLaunch(t, tc.DB, job.ID)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	lists := cursorCloudCallCount(fake, cursorcloudtest.MethodList)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Greater(t, cursorCloudCallCount(fake, cursorcloudtest.MethodList), lists)
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestUpdateInterruptDiscardsSuccessfulResult pins worker.go's post-success
// update check. The discard log is emitted only on that return. The review
// row stays absent.
func TestUpdateInterruptDiscardsSuccessfulResult(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	const agentName = "update-discard-result"
	var jobID int64
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			tc.Pool.InterruptJobsForUpdate([]int64{jobID})
			return cursorCloudPassJSON, nil
		},
	})
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agentName)
	jobID = job.ID
	logs := cursorCloudCaptureLog(t)

	tc.Pool.processJob(testWorkerID, job)

	assert.Contains(t, logs.String(), "discarded successful result for update-interrupted job")
	stored := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, 0, stored.RetryCount)
	_, err := tc.DB.GetReviewByJobID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestUpdateInterruptDiscardsSynthesisResult pins synthesis_worker.go's
// post-success update check. Removing that return hands the document back
// and leaves the attempt running.
func TestUpdateInterruptDiscardsSynthesisResult(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	const synthAgent = "synth-discard-result"
	var jobID int64
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: synthAgent,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			tc.Pool.InterruptJobsForUpdate([]int64{jobID})
			return cursorCloudPassJSON, nil
		},
	})
	_, _, synth := enqueuePanelRun(t, tc, "discard-synth", []memberSpec{{name: "m0", agent: "test"}})
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	_, err := tc.DB.Exec(
		`UPDATE review_jobs SET status = 'running', worker_id = ?, agent = ?, source = '', git_ref = ? WHERE id = ?`,
		testWorkerID, synthAgent, sha, synth.ID,
	)
	require.NoError(t, err)
	job, err := tc.DB.GetJobByID(synth.ID)
	require.NoError(t, err)
	jobID = job.ID

	doc, _, _, runErr := tc.Pool.runSynthesisAgent(context.Background(), testWorkerID, job, nil, "prompt")

	require.ErrorIs(t, runErr, errSynthesisCanceled)
	assert.Empty(t, doc.Summary)
	stored := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, 0, stored.RetryCount)
	_, err = tc.DB.GetReviewByJobID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}
