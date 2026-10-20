package storage

import (
	"context"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListUncleanedCloudLaunchKeepsDeletedJob(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/launch-deleted", "abc123")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'w' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	const token = "deleted-job-tok"
	const repoURL = "https://example.com/acme/old.git"
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", token, repoURL))
	require.NoError(t, deleteReviewJob(t, db, job.ID))

	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	row, ok := cloudLaunchByToken(rows, token)
	require.True(t, ok)
	assert.Equal(t, token, row.Token)
	assert.Equal(t, repoURL, row.RepoIdentity)
	assert.Empty(t, row.RepoPath)
}

// TestSQLiteJobIDIsNotReusedAfterDelete reads the migrated table. The
// initial CREATE uses INTEGER PRIMARY KEY, and the job-id rebuild adds
// AUTOINCREMENT, which does not reuse a deleted rowid. Postgres
// review_jobs.id is SERIAL (nextval); TestPostgresJobIDIsNotReusedAfterDelete
// checks that when a database is configured.
//
// A forced id collision still cannot mark another launch cleaned or hide it.
// MarkCloudLaunchCleaned writes one launch id, and MarkCloudLaunchCleanedByToken
// writes one token. The deleted-job listing pin is the hide that recovery had.
func TestSQLiteJobIDIsNotReusedAfterDelete(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	var jobsDDL string
	require.NoError(t, db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'review_jobs'`).Scan(&jobsDDL))
	assert.Contains(t, jobsDDL, "AUTOINCREMENT")

	_, _, live := createJobChain(t, db, "/tmp/reuse-live", "abc123")
	_, _, oldJob := createJobChain(t, db, "/tmp/reuse-old", "def456")
	require.Greater(t, oldJob.ID, live.ID)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'w' WHERE id IN (?, ?)`, oldJob.ID, live.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(oldJob.ID, "w", "old-tok", "https://example.com/acme/old.git"))
	require.NoError(t, db.BeginCloudLaunch(live.ID, "w", "live-tok", "https://example.com/acme/live.git"))
	require.NoError(t, deleteReviewJob(t, db, oldJob.ID))

	newRepo := createRepo(t, db, "/tmp/reuse-new")
	next, err := db.EnqueueJob(EnqueueOpts{RepoID: newRepo.ID, GitRef: "fff", Agent: "test"})
	require.NoError(t, err)
	assert.NotEqual(t, oldJob.ID, next.ID)

	// The only way to collide is an explicit id. Cleaning that new launch by
	// its token must leave the old launch and the other live launch uncleaned
	// and still listed.
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'w2' WHERE id = ?`, next.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(next.ID, "w2", "new-tok", "https://example.com/acme/new.git"))
	_, err = db.Exec(`INSERT INTO review_jobs (id, repo_id, git_ref, agent, status) VALUES (?, ?, 'forced', 'test', 'failed')`, oldJob.ID, newRepo.ID)
	require.NoError(t, err)
	require.NoError(t, db.MarkCloudLaunchCleanedByToken("new-tok"))

	var oldCleaned, liveCleaned int
	require.NoError(t, db.QueryRow(`SELECT cleaned FROM review_job_cloud_launches WHERE launch_token = 'old-tok'`).Scan(&oldCleaned))
	require.NoError(t, db.QueryRow(`SELECT cleaned FROM review_job_cloud_launches WHERE launch_token = 'live-tok'`).Scan(&liveCleaned))
	assert.Equal(t, 0, oldCleaned)
	assert.Equal(t, 0, liveCleaned)
	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	_, oldOK := cloudLaunchByToken(rows, "old-tok")
	_, liveOK := cloudLaunchByToken(rows, "live-tok")
	_, newOK := cloudLaunchByToken(rows, "new-tok")
	assert.True(t, oldOK)
	assert.True(t, liveOK)
	assert.False(t, newOK)
}

func TestPostgresJobIDIsNotReusedAfterDelete(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	var idType string
	err := pool.pool.QueryRow(ctx, `SELECT data_type || ' ' || COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'roborev' AND table_name = 'review_jobs' AND column_name = 'id'`).Scan(&idType)
	require.NoError(t, err)
	assert.Contains(t, idType, "integer")
	assert.Contains(t, idType, "nextval")

	var repoID, first, second int64
	err = pool.pool.QueryRow(ctx, `INSERT INTO repos (identity) VALUES ($1) RETURNING id`, "job-id-reuse-"+uuid.New().String()).Scan(&repoID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM review_jobs WHERE repo_id = $1`, repoID)
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM repos WHERE id = $1`, repoID)
	})
	err = pool.pool.QueryRow(ctx, `INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'abc', 'test', 'failed', NOW(), $3) RETURNING id`, uuid.New(), repoID, uuid.New()).Scan(&first)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `DELETE FROM review_jobs WHERE id = $1`, first)
	require.NoError(t, err)
	err = pool.pool.QueryRow(ctx, `INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'def', 'test', 'failed', NOW(), $3) RETURNING id`, uuid.New(), repoID, uuid.New()).Scan(&second)
	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

func deleteReviewJob(t *testing.T, db *DB, jobID int64) error {
	t.Helper()
	conn, err := db.Conn(t.Context())
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	_, err = conn.ExecContext(t.Context(), `DELETE FROM review_jobs WHERE id = ?`, jobID)
	return err
}

func cloudLaunchByToken(rows []CloudLaunch, token string) (CloudLaunch, bool) {
	for _, row := range rows {
		if row.Token == token {
			return row, true
		}
	}
	return CloudLaunch{}, false
}
