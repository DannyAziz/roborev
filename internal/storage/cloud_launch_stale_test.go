package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResetStaleJobsFailsUncleanedCloudLaunch pins the restart predicate that
// fails a running attempt when a launch row is still uncleaned, and the
// cursor-cloud attempt that already called the agent. A plain running job is
// requeued. Reverting that WHERE clause fails one of these three rows.
func TestResetStaleJobsFailsUncleanedCloudLaunch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, t.TempDir())
	launchJob := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "sha-launch").ID, "sha-launch")
	invoked := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "sha-invoked").ID, "sha-invoked")
	plain := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "sha-plain").ID, "sha-plain")

	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`, "stale-worker", launchJob.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(launchJob.ID, "stale-worker", "tok-stale", "https://example.com/acme/repo.git"))
	_, err = db.Exec(
		`UPDATE review_jobs SET status = 'running', worker_id = ?, agent = ?, agent_invoked = 1 WHERE id = ?`,
		"stale-worker", "cursor-cloud", invoked.ID,
	)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`, "stale-worker", plain.ID)
	require.NoError(t, err)

	require.NoError(t, db.ResetStaleJobs())

	assert := assert.New(t)
	failedLaunch, err := db.GetJobByID(launchJob.ID)
	require.NoError(t, err)
	assert.Equal(JobStatusFailed, failedLaunch.Status)
	assert.Equal(CloudLaunchInterruptedError, failedLaunch.Error)
	failedInvoked, err := db.GetJobByID(invoked.ID)
	require.NoError(t, err)
	assert.Equal(JobStatusFailed, failedInvoked.Status)
	assert.Equal(CloudLaunchInterruptedError, failedInvoked.Error)
	requeued, err := db.GetJobByID(plain.ID)
	require.NoError(t, err)
	assert.Equal(JobStatusQueued, requeued.Status)
}
