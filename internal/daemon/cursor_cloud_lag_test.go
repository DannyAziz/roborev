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

// TestCreateErrorReconcileOneListDoesNotClean is the create-error callback.
// One list that deletes the VM does not mark the row cleaned, so restart
// still sweeps it. The pending note stays.
func TestCreateErrorReconcileOneListDoesNotClean(t *testing.T) {
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
	_, err = fake.GetAgent(t.Context(), deleted[0])
	require.Error(t, err)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, storage.CloudLaunchPendingNote, cursorCloudLaunchNote(t, tc.DB, job.ID))
}

// TestRestartSavedIDEmptyListRespectsLag is the saved-id restart. found is
// false. Before the lag window the row stays uncleaned. After the window the
// empty sweep cleans. A found sweep past the window still does not.
func TestRestartSavedIDEmptyListRespectsLag(t *testing.T) {
	t.Run("before window", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		const token = "launch-token-before-lag"
		cursorCloudBeginToken(t, tc, job.ID, token)
		require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, "bc_already_gone"))
		fake.MethodErrors = map[string]error{
			cursorcloudtest.MethodDelete: &cursorcloud.APIError{
				Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
			},
		}
		err := recoverStaleJobs(context.Background(), tc.DB, fake)
		require.ErrorContains(t, err, "not listed yet")
		n, nerr := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, nerr)
		assert.Equal(t, 1, n)
		assert.Equal(t, storage.CloudLaunchPendingNote, cursorCloudLaunchNote(t, tc.DB, job.ID))
	})
	t.Run("after window", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		const token = "launch-token-after-lag"
		cursorCloudBeginToken(t, tc, job.ID, token)
		require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, "bc_already_gone"))
		cursorCloudAgeLaunchPastLag(t, tc.DB, job.ID)
		fake.MethodErrors = map[string]error{
			cursorcloudtest.MethodDelete: &cursorcloud.APIError{
				Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
			},
		}
		require.ErrorContains(t, recoverStaleJobs(context.Background(), tc.DB, fake), "not listed yet")
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
	t.Run("found past window", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		const token = "launch-token-found-past-lag"
		machineID, err := tc.DB.GetMachineID()
		require.NoError(t, err)
		marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
		saved := cursorCloudSeedAgent(t, fake, sha, marker)
		other := cursorCloudSeedAgent(t, fake, sha, marker)
		require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, cursorCloudRestartRepo))
		require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, saved.ID))
		cursorCloudAgeLaunchPastLag(t, tc.DB, job.ID)
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		_, err = fake.GetAgent(t.Context(), saved.ID)
		require.Error(t, err)
		_, err = fake.GetAgent(t.Context(), other.ID)
		require.Error(t, err)
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Equal(t, storage.CloudLaunchPendingNote, cursorCloudLaunchNote(t, tc.DB, job.ID))
	})
}

// TestEmptyAgentIDAgedNonEmptyListNotesOrphan is the empty-AgentID age-out.
// The list contains this token's VM. The note is still written, and the row
// is not cleaned. Skipping the note because the list was non-empty fails.
func TestEmptyAgentIDAgedNonEmptyListNotesOrphan(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-aged-listed"
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	cursorCloudSeedAgent(t, fake, sha, marker)
	cursorCloudBeginToken(t, tc, job.ID, token)
	cursorCloudAgeLaunch(t, tc.DB, job.ID)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	var jobErr string
	require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
	assert.Contains(t, jobErr, "was not listed within")
}
