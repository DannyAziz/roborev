package daemon

import (
	"context"
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

func cursorCloudLaunchCleaned(t *testing.T, db *storage.DB, id int64) int {
	t.Helper()
	var cleaned int
	require.NoError(t, db.QueryRow(
		`SELECT cleaned FROM review_job_cloud_launches WHERE id = ?`, id,
	).Scan(&cleaned))
	return cleaned
}

// TestCursorCloudSweepDuplicatePromptErrorStaysUncleaned is the sweep error
// that never becomes a confirmed match. The saved id is already gone. The
// duplicate's conversation read fails, so the sweep cannot tell that VM is
// this token. Swallowing that error marks the row cleaned while the VM is
// still listed. A refused delete is a different pin: there the match is
// already known.
func TestCursorCloudSweepDuplicatePromptErrorStaysUncleaned(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	_, dup := cursorCloudSeedMarkedPair(t, tc, fake, sha, job.ID, "launch-token-sweep-prompt")
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodPrompt: &cursorcloud.APIError{
			Op: "conversation", StatusCode: http.StatusInternalServerError, Message: "unavailable",
		},
	}

	err := recoverStaleJobs(context.Background(), tc.DB, fake)
	assert := assert.New(t)
	assert.Equal(0, cursorCloudCleanedFlag(t, tc, job.ID))
	require.ErrorContains(t, err, "orphan agent")
	require.ErrorContains(t, err, dup.ID)
	got, getErr := fake.GetAgent(t.Context(), dup.ID)
	require.NoError(t, getErr)
	assert.Equal(dup.ID, got.ID)
}

// TestCursorCloudSweepDuplicateDeleteFailureReportsOrphan is the report for
// a duplicate the sweep matched and then failed to delete. The row stays
// uncleaned, and both the returned error and the cleanup log name that VM
// as an orphan. Dropping the orphan sentence leaves a bare delete error.
func TestCursorCloudSweepDuplicateDeleteFailureReportsOrphan(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-sweep-report"
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	saved := cursorCloudSeedAgent(t, fake, sha, marker)
	dup := cursorCloudSeedAgent(t, fake, sha, marker)
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, cursorCloudRestartRepo))
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, saved.ID))
	logs := cursorCloudCaptureLog(t)

	err = recoverStaleJobs(context.Background(), tc.DB, &cursorCloudRefuseDelete{Fake: fake, id: dup.ID})
	assert := assert.New(t)
	assert.Equal(0, cursorCloudCleanedFlag(t, tc, job.ID))
	require.ErrorContains(t, err, "orphan agent")
	require.ErrorContains(t, err, dup.ID)
	assert.Contains(logs.String(), "orphan agent")
	assert.Contains(logs.String(), dup.ID)
	got, getErr := fake.GetAgent(t.Context(), dup.ID)
	require.NoError(t, getErr)
	assert.Equal(dup.ID, got.ID)
}

// TestCursorCloudSweepDuplicateCleansEveryTokenRow is the clean after a
// duplicate sweep confirms the token is gone. A second row carries the same
// token and no agent id. Cleaning only the row id that ran the sweep leaves
// that sibling uncleaned. The token clean covers both rows.
func TestCursorCloudSweepDuplicateCleansEveryTokenRow(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-sweep-rows"
	saved, dup := cursorCloudSeedMarkedPair(t, tc, fake, sha, job.ID, token)
	var sweptID int64
	require.NoError(t, tc.DB.QueryRow(
		`SELECT id FROM review_job_cloud_launches WHERE job_id = ? AND launch_token = ?`,
		job.ID, token,
	).Scan(&sweptID))
	res, err := tc.DB.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, agent_id, repo_identity, created_at, orphan_note)
		VALUES (?, ?, '', ?, datetime('now'), ?)`,
		job.ID, token, cursorCloudRestartRepo, storage.CloudLaunchPendingNote)
	require.NoError(t, err)
	siblingID, err := res.LastInsertId()
	require.NoError(t, err)

	err = recoverStaleJobs(context.Background(), tc.DB, fake)
	assert := assert.New(t)
	// One pass deletes the VM and does not clean. The lag gate waits for a
	// later sweep. Both rows stay uncleaned until that sweep.
	assert.Equal(0, cursorCloudLaunchCleaned(t, tc.DB, sweptID))
	assert.Equal(0, cursorCloudLaunchCleaned(t, tc.DB, siblingID))
	_, getErr := fake.GetAgent(t.Context(), saved.ID)
	require.Error(t, getErr)
	_, getErr = fake.GetAgent(t.Context(), dup.ID)
	require.Error(t, getErr)
	require.ErrorContains(t, err, "not listed yet")

	cursorCloudAgeLaunch(t, tc.DB, job.ID)
	err = recoverStaleJobs(context.Background(), tc.DB, fake)
	require.NoError(t, err)
	assert.Equal(1, cursorCloudLaunchCleaned(t, tc.DB, sweptID))
	assert.Equal(1, cursorCloudLaunchCleaned(t, tc.DB, siblingID))
}
