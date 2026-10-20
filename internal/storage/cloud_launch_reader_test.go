package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetJobByIDShowsLaunchOrphanWarning(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/reader-direct", "abc123")
	_, _, quiet := createJobChain(t, db, "/tmp/reader-direct-quiet", "def456")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'direct-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, got.OrphanWarning, "orphan risk")
	assert.Contains(t, got.Error, "orphan risk")

	other, err := db.GetJobByID(quiet.ID)
	require.NoError(t, err)
	assert.Empty(t, other.OrphanWarning)
}

func TestGetJobsWithReviewsByIDsShowsLaunchOrphanWarning(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/reader-batch", "abc123")
	_, _, quiet := createJobChain(t, db, "/tmp/reader-batch-quiet", "def456")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'batch-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	got, err := db.GetJobsWithReviewsByIDs([]int64{job.ID, quiet.ID})
	require.NoError(t, err)
	assert.Contains(t, got[job.ID].Job.OrphanWarning, "orphan risk")
	assert.Contains(t, got[job.ID].Job.Error, "orphan risk")
	assert.Empty(t, got[quiet.ID].Job.OrphanWarning)
}

func TestGetJobByIDSurfacesLaunchLookupError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/reader-direct-err", "abc123")
	hideLaunchOrphanColumn(t, db)

	_, err := db.GetJobByID(job.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orphan_note")
}

func TestGetJobsWithReviewsByIDsSurfacesLaunchLookupError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/reader-batch-err", "abc123")
	hideLaunchOrphanColumn(t, db)

	_, err := db.GetJobsWithReviewsByIDs([]int64{job.ID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orphan_note")
}

func hideLaunchOrphanColumn(t *testing.T, db *DB) {
	t.Helper()
	_, err := db.Exec(`ALTER TABLE review_job_cloud_launches RENAME TO review_job_cloud_launches_src`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE VIEW review_job_cloud_launches AS
		SELECT id, job_id, launch_token, agent_id, cleaned, repo_identity, created_at
		FROM review_job_cloud_launches_src`)
	require.NoError(t, err)
}
