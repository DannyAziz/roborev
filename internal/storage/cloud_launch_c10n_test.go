package storage

import (
	"context"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkCloudLaunchCleanedByTokenLeavesOtherJobAndRepo is the shared-token
// pin. Cleaning one job and repo must leave the same token uncleaned on
// another job in that repo and on another repo. The unique index is dropped
// so the rows can exist; the index pin is separate.
func TestMarkCloudLaunchCleanedByTokenLeavesOtherJobAndRepo(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, err := db.Exec(`DROP INDEX IF EXISTS idx_review_job_cloud_launches_token`)
	require.NoError(t, err)

	repoA, _, jobA := createJobChain(t, db, t.TempDir(), "aaa111")
	commitA2 := createCommit(t, db, repoA.ID, "aaa222")
	jobA2 := enqueueJob(t, db, repoA.ID, commitA2.ID, "aaa222")
	_, _, jobB := createJobChain(t, db, t.TempDir(), "bbb333")

	const token = "shared-tok"
	insert := func(jobID int64) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO review_job_cloud_launches
			(job_id, launch_token, orphan_note) VALUES (?, ?, 'note')`, jobID, token)
		require.NoError(t, err)
	}
	insert(jobA.ID)
	insert(jobA2.ID)
	insert(jobB.ID)

	require.NoError(t, db.MarkCloudLaunchCleanedByToken(jobA.ID, repoA.ID, token))

	cleaned := func(jobID int64) int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRow(
			`SELECT cleaned FROM review_job_cloud_launches WHERE job_id = ? AND launch_token = ?`,
			jobID, token,
		).Scan(&n))
		return n
	}
	assert := assert.New(t)
	assert.Equal(1, cleaned(jobA.ID))
	assert.Equal(0, cleaned(jobA2.ID))
	assert.Equal(0, cleaned(jobB.ID))
}

// TestCloudLaunchDuplicateTokenRejected is the unique-index pin. A second
// row with the same non-empty token fails. Blank tokens are not launch tokens.
func TestCloudLaunchDuplicateTokenRejected(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	var indexSQL string
	require.NoError(t, db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_review_job_cloud_launches_token'`,
	).Scan(&indexSQL))
	require.Contains(t, strings.ToUpper(indexSQL), "UNIQUE")

	_, _, jobA := createJobChain(t, db, t.TempDir(), "dup-a")
	_, _, jobB := createJobChain(t, db, t.TempDir(), "dup-b")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches (job_id, launch_token) VALUES (?, 'dup-tok')`, jobA.ID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO review_job_cloud_launches (job_id, launch_token) VALUES (?, 'dup-tok')`, jobB.ID)
	require.Error(t, err)
	assert.Contains(t, strings.ToUpper(err.Error()), "UNIQUE")
}

// TestMarkCloudLaunchCleanedByTokenMarksMatchingRow is the existing clean
// path. The listed job and repo mark that row, and a different repo does not.
func TestMarkCloudLaunchCleanedByTokenMarksMatchingRow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo, _, job := createJobChain(t, db, t.TempDir(), "right111")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'right-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	err = db.MarkCloudLaunchCleanedByToken(job.ID, repo.ID+1, "right-tok")
	require.Error(t, err)
	assert := assert.New(t)
	assert.Contains(err.Error(), "not cleaned")

	var cleaned int
	var note string
	require.NoError(t, db.QueryRow(
		`SELECT cleaned, orphan_note FROM review_job_cloud_launches WHERE launch_token = 'right-tok'`,
	).Scan(&cleaned, &note))
	assert.Equal(0, cleaned)
	assert.Equal(CloudLaunchPendingNote, note)

	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	row, ok := cloudLaunchByToken(rows, "right-tok")
	require.True(t, ok)
	assert.Equal(job.ID, row.JobID)
	assert.Equal(repo.ID, row.RepoID)

	require.NoError(t, db.MarkCloudLaunchCleanedByToken(row.JobID, row.RepoID, row.Token))
	require.NoError(t, db.QueryRow(
		`SELECT cleaned, orphan_note FROM review_job_cloud_launches WHERE launch_token = 'right-tok'`,
	).Scan(&cleaned, &note))
	assert.Equal(1, cleaned)
	assert.Empty(note)
}

func TestPostgresCloudLaunchDuplicateTokenRejected(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	token := "c10n-dup-" + uuid.New().String()
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM review_job_cloud_launches WHERE launch_token = $1`, token)
	})

	var indexSQL string
	require.NoError(t, pool.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = 'idx_review_job_cloud_launches_token'`).Scan(&indexSQL))
	require.Contains(t, strings.ToUpper(indexSQL), "UNIQUE")

	_, err := pool.pool.Exec(ctx, `INSERT INTO review_job_cloud_launches (job_id, launch_token) VALUES (1, $1)`, token)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `INSERT INTO review_job_cloud_launches (job_id, launch_token) VALUES (2, $1)`, token)
	require.Error(t, err)
	assert.Contains(t, strings.ToUpper(err.Error()), "UNIQUE")
}
