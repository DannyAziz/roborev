package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeginCloudLaunchCreatedAtIsInsertTime(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	_, err := db.Exec(`DROP TABLE review_job_cloud_launches`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE review_job_cloud_launches (
		id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL, launch_token TEXT NOT NULL,
		agent_id TEXT NOT NULL DEFAULT '', cleaned INTEGER NOT NULL DEFAULT 0,
		repo_identity TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	job := runningCloudJob(t, db)
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok", "https://example.com/acme/repo.git"))
	var created string
	require.NoError(t, db.QueryRow(`SELECT created_at FROM review_job_cloud_launches WHERE job_id = ?`, job.ID).Scan(&created))
	assert.NotEmpty(t, created)
	_, err = db.Exec(`UPDATE review_job_cloud_launches SET created_at = datetime('now') WHERE created_at = ''`)
	require.NoError(t, err)
	var again string
	require.NoError(t, db.QueryRow(`SELECT created_at FROM review_job_cloud_launches WHERE job_id = ?`, job.ID).Scan(&again))
	assert.Equal(t, created, again)
}

func TestDeleteRepoRefusesUncleanedLaunchWhenFKOn(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	job := runningCloudJob(t, db)
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok", "https://example.com/acme/repo.git"))
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	var fk int
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk))
	require.Equal(t, 1, fk)

	err = db.DeleteRepo(job.RepoID, true)
	require.ErrorIs(t, err, ErrRepoHasCloudLaunch)
	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, job.ID, rows[0].JobID)
	assert.Equal(t, "https://example.com/acme/repo.git", rows[0].RepoIdentity)
}

func TestUncleanedLaunchStaysVisibleWhenFKOff(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	job := runningCloudJob(t, db)
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok", "https://example.com/acme/repo.git"))
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `DELETE FROM review_jobs WHERE id = ?`, job.ID)
	require.NoError(t, err)

	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "https://example.com/acme/repo.git", rows[0].RepoIdentity)
	assert.Empty(t, rows[0].RepoPath)
}

func runningCloudJob(t *testing.T, db *DB) *ReviewJob {
	t.Helper()
	repo := createRepo(t, db, t.TempDir())
	commit := createCommit(t, db, repo.ID, "abc123")
	job := enqueueJob(t, db, repo.ID, commit.ID, "abc123")
	setJobStatus(t, db, job.ID, JobStatusRunning)
	_, err := db.Exec(`UPDATE review_jobs SET worker_id = 'w' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	job.RepoID = repo.ID
	return job
}
