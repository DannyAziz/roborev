package daemon

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/testutil"
)

const cursorCloudBoundRepo = "https://example.com/acme/bound.git"

// cursorCloudRefuseDelete fails DeleteAgent for one id and delegates the rest.
// A 400 is final, so the sweep sees a failed delete rather than a 404.
type cursorCloudRefuseDelete struct {
	*cursorcloudtest.Fake
	id string
}

func (c *cursorCloudRefuseDelete) DeleteAgent(ctx context.Context, id string) error {
	if id == c.id {
		return &cursorcloud.APIError{Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "refused"}
	}
	return c.Fake.DeleteAgent(ctx, id)
}

// cursorCloudCancelOnList cancels the parent after a successful list so the
// marker sweep's match loop observes a done context before it deletes.
type cursorCloudCancelOnList struct {
	*cursorcloudtest.Fake
	cancel context.CancelFunc
}

func (c *cursorCloudCancelOnList) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	agents, err := c.Fake.ListAgents(ctx)
	c.cancel()
	return agents, err
}

func cursorCloudCleanedFlag(t *testing.T, tc *workerTestContext, jobID int64) int {
	t.Helper()
	var cleaned int
	require.NoError(t, tc.DB.QueryRow(
		`SELECT cleaned FROM review_job_cloud_launches WHERE job_id = ?`, jobID,
	).Scan(&cleaned))
	return cleaned
}

func cursorCloudInsertBareLaunch(t *testing.T, tc *workerTestContext, jobID int64, token, agentID string) {
	t.Helper()
	_, err := tc.DB.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, agent_id, repo_identity, created_at)
		VALUES (?, ?, ?, ?, datetime('now'))`, jobID, token, agentID, cursorCloudRestartRepo)
	require.NoError(t, err)
}

// TestCursorCloudReconcileDeleteFailureStaysUncleaned is M2. Create stored
// the VM and then failed, so reconcile is the path that can mark the row.
// A 500 and a deadline both do that. A failed delete must not run onCleaned.
// Calling it anyway sets cleaned=1 while the VM is still listed.
func TestCursorCloudReconcileDeleteFailureStaysUncleaned(t *testing.T) {
	cases := []struct {
		name      string
		createErr error
	}{
		{
			name: "5xx",
			createErr: &cursorcloud.APIError{
				Op: "create agent", StatusCode: http.StatusInternalServerError, Message: "boom",
			},
		},
		{name: "timeout", createErr: context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtc, _ := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, wtc.TmpDir)
			job := wtc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			fake := &cursorcloudtest.Fake{}
			script := &cloudCreateScript{Fake: fake, createErr: tc.createErr, failDelete: 1}

			_, err := newBoundCloud(t, wtc, job, script).Review(context.Background(), wtc.TmpDir, sha, "prompt", nil)
			assert.Equal(t, 0, cursorCloudCleanedFlag(t, wtc, job.ID))
			require.Error(t, err)
			listed, err := fake.ListAgents(t.Context())
			require.NoError(t, err)
			require.Len(t, listed, 1)
			_, err = fake.GetAgent(t.Context(), listed[0].ID)
			require.NoError(t, err)
		})
	}
}

// TestWorkerBindPersistsExactLaunchTokenAndRepo is m7b/e/g/h. The launch
// row is written by the worker's BindLaunch, not a test double. The stored
// token is the one embedded in the create prompt, and the stored repo is the
// URL passed to that bind, which is not the checkout origin.
func TestWorkerBindPersistsExactLaunchTokenAndRepo(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		Final:        "ok",
		MethodErrors: map[string]error{
			cursorcloudtest.MethodDelete: &cursorcloud.APIError{
				Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "refused",
			},
		},
	}
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: cursorCloudBoundRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	bound := tc.Pool.bindCursorCloudLaunch(testWorkerID, job, cloud)

	_, err := bound.Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)

	var storedToken, storedRepo string
	require.NoError(t, tc.DB.QueryRow(
		`SELECT launch_token, repo_identity FROM review_job_cloud_launches WHERE job_id = ?`, job.ID,
	).Scan(&storedToken, &storedRepo))
	var created cursorcloud.CreateAgentRequest
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodCreate {
			created = call.Request
			break
		}
	}
	require.NotEmpty(t, created.Prompt)
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	daemonID := machineID.String() //nolint:forbidigo // launch marker is prompt text
	prefix := agent.CursorCloudLaunchMarker(daemonID, "")
	idx := strings.Index(created.Prompt, prefix)
	require.GreaterOrEqual(t, idx, 0)
	promptToken := created.Prompt[idx+len(prefix):]
	if nl := strings.IndexByte(promptToken, '\n'); nl >= 0 {
		promptToken = promptToken[:nl]
	}

	assert := assert.New(t)
	assert.Equal(promptToken, storedToken)
	assert.NotEmpty(storedToken)
	assert.Equal(cursorCloudBoundRepo, storedRepo)
	assert.Equal(created.Repo, storedRepo)
	assert.NotEqual(cursorCloudRestartRepo, storedRepo)
}

// TestCursorCloudRestartSweepDeleteFailureStaysUncleaned is the marker-sweep
// error. The saved id is deleted. The other VM for that token refuses delete.
// Dropping the sweep error marks the row cleaned while that VM is still up.
// The token-only subtest is the same error with no saved id: found is true,
// so a guard that only keeps the error when an id is set still cleans the row.
func TestCursorCloudRestartSweepDeleteFailureStaysUncleaned(t *testing.T) {
	t.Run("duplicate after saved delete", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		const token = "launch-token-sweep-delete"
		machineID, err := tc.DB.GetMachineID()
		require.NoError(t, err)
		marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
		saved := cursorCloudSeedAgent(t, fake, sha, marker)
		dup := cursorCloudSeedAgent(t, fake, sha, marker)
		require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, cursorCloudRestartRepo))
		require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, saved.ID))

		err = recoverStaleJobs(context.Background(), tc.DB, &cursorCloudRefuseDelete{Fake: fake, id: dup.ID})
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
		_, getErr := fake.GetAgent(t.Context(), saved.ID)
		require.Error(t, getErr)
		got, getErr := fake.GetAgent(t.Context(), dup.ID)
		require.NoError(t, getErr)
		assert.Equal(t, dup.ID, got.ID)
		assert.Error(t, err)
	})

	t.Run("token only", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		const token = "launch-token-sweep-only"
		cursorCloudBeginToken(t, tc, job.ID, token)
		created := cursorCloudShowToken(t, tc, fake, sha, token)

		err := recoverStaleJobs(context.Background(), tc.DB, &cursorCloudRefuseDelete{Fake: fake, id: created.ID})
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
		got, getErr := fake.GetAgent(t.Context(), created.ID)
		require.NoError(t, getErr)
		assert.Equal(t, created.ID, got.ID)
		assert.Error(t, err)
	})
}

// TestCursorCloudRestartListErrorLeavesDuplicateUncleaned is a list failure
// after the saved id was deleted. The duplicate is only visible to the sweep.
// Turning that list error into a miss marks the row cleaned.
func TestCursorCloudRestartListErrorLeavesDuplicateUncleaned(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-list-err"
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	saved := cursorCloudSeedAgent(t, fake, sha, marker)
	dup := cursorCloudSeedAgent(t, fake, sha, marker)
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, cursorCloudRestartRepo))
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, saved.ID))
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodList: &cursorcloud.APIError{
			Op: "list agents", StatusCode: http.StatusInternalServerError, Message: "unavailable",
		},
	}

	err = recoverStaleJobs(context.Background(), tc.DB, fake)
	require.Error(t, err)
	_, getErr := fake.GetAgent(t.Context(), saved.ID)
	require.Error(t, getErr)
	got, getErr := fake.GetAgent(t.Context(), dup.ID)
	require.NoError(t, getErr)
	assert.Equal(t, dup.ID, got.ID)
	assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
}

// TestCursorCloudRestartCanceledSweepStaysUncleaned is ctx done. A context
// that is already canceled fails the sweep list. Canceling after the list
// returns fails the match loop before the duplicate is deleted. Either way
// the row stays cleaned=0 and the duplicate stays up.
func TestCursorCloudRestartCanceledSweepStaysUncleaned(t *testing.T) {
	t.Run("canceled before list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		const token = "launch-token-ctx-list"
		saved, dup := cursorCloudSeedMarkedPair(t, tc, fake, sha, job.ID, token)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := recoverStaleJobs(ctx, tc.DB, fake)
		require.Error(t, err)
		_, getErr := fake.GetAgent(t.Context(), saved.ID)
		require.Error(t, getErr)
		got, getErr := fake.GetAgent(t.Context(), dup.ID)
		require.NoError(t, getErr)
		assert.Equal(t, dup.ID, got.ID)
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	})

	t.Run("canceled after list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		const token = "launch-token-ctx-match"
		saved, dup := cursorCloudSeedMarkedPair(t, tc, fake, sha, job.ID, token)
		ctx, cancel := context.WithCancel(context.Background())

		err := recoverStaleJobs(ctx, tc.DB, &cursorCloudCancelOnList{Fake: fake, cancel: cancel})
		require.Error(t, err)
		_, getErr := fake.GetAgent(t.Context(), saved.ID)
		require.Error(t, getErr)
		got, getErr := fake.GetAgent(t.Context(), dup.ID)
		require.NoError(t, getErr)
		assert.Equal(t, dup.ID, got.ID)
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	})
}

func cursorCloudSeedMarkedPair(t *testing.T, tc *workerTestContext, fake *cursorcloudtest.Fake, sha string, jobID int64, token string) (cursorcloud.Agent, cursorcloud.Agent) {
	t.Helper()
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	saved := cursorCloudSeedAgent(t, fake, sha, marker)
	dup := cursorCloudSeedAgent(t, fake, sha, marker)
	require.NoError(t, tc.DB.BeginCloudLaunch(jobID, testWorkerID, token, cursorCloudRestartRepo))
	require.NoError(t, tc.DB.AttachCloudAgent(jobID, token, saved.ID))
	return saved, dup
}

// TestCursorCloudRestartEmptyTokenCleansOnlyConfirmedDelete is the empty-token
// gate. There is no marker, so the row stays uncleaned: a list error, a
// non-empty list, an empty list, and a saved-id delete all leave cleaned=0.
// A refused delete also leaves the VM in place.
func TestCursorCloudRestartEmptyTokenCleansOnlyConfirmedDelete(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudInsertBareLaunch(t, tc, job.ID, "", "")
		fake.MethodErrors = map[string]error{
			cursorcloudtest.MethodList: &cursorcloud.APIError{
				Op: "list agents", StatusCode: http.StatusInternalServerError, Message: "unavailable",
			},
		}

		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
		assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodList))
	})

	t.Run("non-empty list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudSeedAgent(t, fake, sha, "foreign")
		cursorCloudInsertBareLaunch(t, tc, job.ID, "", "")

		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
		assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodList))
	})

	t.Run("empty list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudInsertBareLaunch(t, tc, job.ID, "", "")

		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
		var jobErr string
		require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
		assert.Contains(t, jobErr, "orphan risk")
	})

	t.Run("saved id delete refused", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		saved := cursorCloudSeedAgent(t, fake, sha, "saved")
		cursorCloudInsertBareLaunch(t, tc, job.ID, "", saved.ID)
		fake.MethodErrors = map[string]error{
			cursorcloudtest.MethodDelete: &cursorcloud.APIError{
				Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "refused",
			},
		}

		require.Error(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		got, err := fake.GetAgent(t.Context(), saved.ID)
		require.NoError(t, err)
		assert.Equal(t, saved.ID, got.ID)
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	})

	t.Run("saved id deleted", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		saved := cursorCloudSeedAgent(t, fake, sha, "saved")
		cursorCloudInsertBareLaunch(t, tc, job.ID, "", saved.ID)

		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		_, err := fake.GetAgent(t.Context(), saved.ID)
		require.Error(t, err)
		assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	})
}

// TestCursorCloudRestartEmptyTokenCleanKeepsRefusedSibling pins the blank
// token clean to its own row. Two empty-token launches on one job pass the
// confirming-sweep gate. The first sibling's saved-id delete is refused, so
// its VM is still up. The second row's clean must not mark the first one
// cleaned. Cleaning every blank-token row on the job fails this test.
func TestCursorCloudRestartEmptyTokenCleanKeepsRefusedSibling(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	refused := cursorCloudSeedAgent(t, fake, sha, "refused")
	deleted := cursorCloudSeedAgent(t, fake, sha, "deleted")
	cursorCloudInsertBareLaunch(t, tc, job.ID, "", refused.ID)
	cursorCloudInsertBareLaunch(t, tc, job.ID, "", deleted.ID)
	_, err := tc.DB.Exec(`UPDATE review_job_cloud_launches SET sweep_seen = 1 WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET error = ? WHERE id = ?`, agent.CursorCloudPendingOrphanNote(), job.ID)
	require.NoError(t, err)
	cursorCloudAgeLaunch(t, tc.DB, job.ID)

	err = recoverCloudLaunches(context.Background(), tc.DB, &cursorCloudRefuseDelete{Fake: fake, id: refused.ID}, nil)
	require.Error(t, err)
	got, getErr := fake.GetAgent(t.Context(), refused.ID)
	require.NoError(t, getErr)
	assert.Equal(t, refused.ID, got.ID)
	var refusedCleaned, deletedCleaned int
	require.NoError(t, tc.DB.QueryRow(
		`SELECT cleaned FROM review_job_cloud_launches WHERE agent_id = ?`, refused.ID,
	).Scan(&refusedCleaned))
	require.NoError(t, tc.DB.QueryRow(
		`SELECT cleaned FROM review_job_cloud_launches WHERE agent_id = ?`, deleted.ID,
	).Scan(&deletedCleaned))
	assert.Equal(t, 0, refusedCleaned)
	assert.Equal(t, 1, deletedCleaned)
}
