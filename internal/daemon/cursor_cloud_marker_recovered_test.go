package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/testutil"
)

// TestCursorCloudRestartMarkerRecoveredCleansOnLaterSweep crashes after
// CreateAgent and before the id is saved. The first restart finds the VM by
// marker and deletes it. A later empty sweep past the delay cleans the row.
func TestCursorCloudRestartMarkerRecoveredCleansOnLaterSweep(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	created := cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-recovered", false)
	cursorCloudAgeLaunchPastLag(t, tc.DB, job.ID)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	_, err := fake.GetAgent(t.Context(), created.ID)
	require.Error(t, err)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// TestCursorCloudRestartNeverFoundStaysUncleaned has no VM for the token.
// Empty sweeps past the delay never clean a row whose VM was not deleted.
func TestCursorCloudRestartNeverFoundStaysUncleaned(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudBeginToken(t, tc, job.ID, "launch-token-never-found")
	cursorCloudAgeLaunchPastLag(t, tc.DB, job.ID)

	for range 3 {
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	}
}

// TestCursorCloudRestartDeletesLegacyMarker seeds a VM whose prompt has the
// bare token marker an older build wrote. Restart finds and deletes it.
func TestCursorCloudRestartDeletesLegacyMarker(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-legacy-marker"
	legacy := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker("", token))
	cursorCloudBeginToken(t, tc, job.ID, token)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	_, err := fake.GetAgent(t.Context(), legacy.ID)
	require.Error(t, err)
}
