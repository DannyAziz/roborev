package storage

import (
	"context"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteLaunchOrphanRiskZeroRowsErrors(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	err := db.NoteLaunchOrphanRisk("missing-token", CloudLaunchPendingNote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not recorded")
	require.Error(t, db.NoteLaunchOrphanRisk("", CloudLaunchPendingNote))
	require.Error(t, db.NoteLaunchOrphanRisk("tok", " "))
}

func TestNoteLaunchOrphanRiskDoesNotReplaceSetNote(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/note-keep", "abc123")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'keep-tok', 'cursor-cloud orphan risk: first')`, job.ID)
	require.NoError(t, err)

	require.NoError(t, db.NoteLaunchOrphanRisk("keep-tok", "cursor-cloud orphan risk: second"))
	var note string
	require.NoError(t, db.QueryRow(`SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = 'keep-tok'`).Scan(&note))
	assert.Equal(t, "cursor-cloud orphan risk: first", note)

	var id int64
	require.NoError(t, db.QueryRow(`SELECT id FROM review_job_cloud_launches WHERE launch_token = 'keep-tok'`).Scan(&id))
	require.NoError(t, db.MarkCloudLaunchCleaned(id))
	require.NoError(t, db.QueryRow(`SELECT orphan_note FROM review_job_cloud_launches WHERE id = ?`, id).Scan(&note))
	assert.Empty(t, note)
}

func TestCloudLaunchSurvivesJobDeleteWithForeignKeysOn(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/note-fk-on", "abc123")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'fk-on-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)
	var before int64
	require.NoError(t, db.QueryRow(`SELECT id FROM review_job_cloud_launches WHERE launch_token = 'fk-on-tok'`).Scan(&before))

	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM review_jobs WHERE id = ?`, job.ID)
	require.NoError(t, err)

	var id int64
	var note string
	require.NoError(t, db.QueryRow(`SELECT id, orphan_note FROM review_job_cloud_launches WHERE launch_token = 'fk-on-tok'`).Scan(&id, &note))
	assert.Equal(t, before, id)
	assert.Equal(t, CloudLaunchPendingNote, note)
	warning, err := db.LaunchOrphanWarning(job.ID)
	require.NoError(t, err)
	assert.Equal(t, CloudLaunchPendingNote, warning)
	var ddl string
	require.NoError(t, db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'review_job_cloud_launches'`).Scan(&ddl))
	assert.NotContains(t, strings.ToUpper(ddl), "ON DELETE CASCADE")
}

func TestPostgresCloudLaunchSurvivesJobDelete(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	token := "pg-survive-" + uuid.New().String()
	var repoID int64
	err := pool.pool.QueryRow(ctx, `INSERT INTO repos (identity) VALUES ($1) RETURNING id`, "cloud-launch-"+token).Scan(&repoID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM review_job_cloud_launches WHERE launch_token = $1`, token)
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM review_jobs WHERE repo_id = $1`, repoID)
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM repos WHERE id = $1`, repoID)
	})
	var jobID int64
	err = pool.pool.QueryRow(ctx, `INSERT INTO review_jobs
		(uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'abc', 'test', 'failed', NOW(), $3) RETURNING id`,
		uuid.New(), repoID, uuid.New()).Scan(&jobID)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `INSERT INTO review_job_cloud_launches (job_id, launch_token, orphan_note)
		VALUES ($1, $2, $3)`, jobID, token, CloudLaunchPendingNote)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `DELETE FROM review_jobs WHERE id = $1`, jobID)
	require.NoError(t, err)
	var note string
	require.NoError(t, pool.pool.QueryRow(ctx, `SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = $1`, token).Scan(&note))
	assert.Equal(t, CloudLaunchPendingNote, note)
}

func TestLaunchOrphanWarningIgnoresMissingJobRow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/note-join", "abc123")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'join-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `DELETE FROM review_jobs WHERE id = ?`, job.ID)
	require.NoError(t, err)

	warning, err := db.LaunchOrphanWarning(job.ID)
	require.NoError(t, err)
	assert.Equal(t, CloudLaunchPendingNote, warning)
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM review_jobs WHERE id = ?`, job.ID).Scan(&n))
	assert.Equal(t, 0, n)
}
