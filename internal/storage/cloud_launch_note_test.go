package storage

import (
	"context"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkerWriteZeroRowsReturnsError is the pin for a marker write that
// updates nothing. Returning nil after RowsAffected()==0 fails this test.
func TestMarkerWriteZeroRowsReturnsError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/marker-zero", "abc123")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, cleaned, orphan_note) VALUES (?, 'cleaned-tok', 1, '')`, job.ID)
	require.NoError(t, err)

	err = db.NoteLaunchOrphanRisk("missing-tok", CloudLaunchPendingNote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not recorded")
	err = db.NoteLaunchOrphanRisk("cleaned-tok", CloudLaunchPendingNote)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not recorded")
	var note string
	require.NoError(t, db.QueryRow(`SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = 'cleaned-tok'`).Scan(&note))
	assert.Empty(t, note)
}

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

	err = db.NoteLaunchOrphanRisk("keep-tok", "cursor-cloud orphan risk: second")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not recorded")
	var note string
	require.NoError(t, db.QueryRow(`SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = 'keep-tok'`).Scan(&note))
	assert.Equal(t, "cursor-cloud orphan risk: first", note)

	var id int64
	require.NoError(t, db.QueryRow(`SELECT id FROM review_job_cloud_launches WHERE launch_token = 'keep-tok'`).Scan(&id))
	require.NoError(t, db.MarkCloudLaunchCleaned(id))
	require.NoError(t, db.QueryRow(`SELECT orphan_note FROM review_job_cloud_launches WHERE id = ?`, id).Scan(&note))
	assert.Empty(t, note)
}

func TestMarkCloudLaunchCleanedByTokenClearsOrphanNote(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/note-clean-token", "abc456")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'clear-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	require.NoError(t, db.MarkCloudLaunchCleanedByToken("clear-tok"))
	var cleaned int
	var note string
	require.NoError(t, db.QueryRow(
		`SELECT cleaned, orphan_note FROM review_job_cloud_launches WHERE launch_token = 'clear-tok'`,
	).Scan(&cleaned, &note))
	assert.Equal(t, 1, cleaned)
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

func TestCloudLaunchMigrationBackfillsUncleanedMarker(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, pendingJob := createJobChain(t, db, "/tmp/marker-pending", "abc123")
	_, _, doneJob := createJobChain(t, db, "/tmp/marker-done", "def456")
	_, err := db.Exec(`DROP TABLE review_job_cloud_launches`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE review_job_cloud_launches (
		id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL, launch_token TEXT NOT NULL,
		agent_id TEXT NOT NULL DEFAULT '', cleaned INTEGER NOT NULL DEFAULT 0,
		repo_identity TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, agent_id, cleaned, repo_identity) VALUES
		(?, 'pending-tok', 'bc_old', 0, 'https://example.com/acme/old.git'),
		(?, 'done-tok', 'bc_done', 1, 'https://example.com/acme/old.git')`, pendingJob.ID, doneJob.ID)
	require.NoError(t, err)

	require.NoError(t, db.ensureCloudLaunchTable())
	require.NoError(t, db.ensureCloudLaunchTable())

	var pending, done string
	require.NoError(t, db.QueryRow(`SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = 'pending-tok'`).Scan(&pending))
	require.NoError(t, db.QueryRow(`SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = 'done-tok'`).Scan(&done))
	assert.Contains(t, pending, "orphan risk")
	assert.Empty(t, done)
	warning, err := db.LaunchOrphanWarning(pendingJob.ID)
	require.NoError(t, err)
	assert.Contains(t, warning, "orphan risk")
	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	var saw bool
	for _, row := range rows {
		if row.Token == "pending-tok" {
			saw = true
		}
		assert.NotEqual(t, "done-tok", row.Token)
	}
	assert.True(t, saw)
}

func TestCloudLaunchMigrationKeepsEmptyTokenUncleaned(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, blankJob := createJobChain(t, db, "/tmp/token-blank", "abc123")
	_, _, nullJob := createJobChain(t, db, "/tmp/token-null", "def456")
	_, _, cleanedJob := createJobChain(t, db, "/tmp/token-cleaned", "fff789")
	_, err := db.Exec(`DROP TABLE review_job_cloud_launches`)
	require.NoError(t, err)
	// The pre-upgrade column is nullable, so an older binary can store NULL
	// or ''. NOT NULL without a default does not stop an explicit empty string.
	_, err = db.Exec(`CREATE TABLE review_job_cloud_launches (
		id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL, launch_token TEXT,
		agent_id TEXT NOT NULL DEFAULT '', cleaned INTEGER NOT NULL DEFAULT 0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO review_job_cloud_launches (job_id, launch_token, agent_id, cleaned) VALUES
		(?, '', 'bc_blank', 0),
		(?, NULL, 'bc_null', 0),
		(?, '', 'bc_done', 1)`, blankJob.ID, nullJob.ID, cleanedJob.ID)
	require.NoError(t, err)

	require.NoError(t, db.ensureCloudLaunchTable())

	type gotRow struct {
		token, note string
		cleaned     int
	}
	var blank, nullTok, done gotRow
	require.NoError(t, db.QueryRow(`SELECT COALESCE(launch_token, '<nil>'), orphan_note, cleaned
		FROM review_job_cloud_launches WHERE agent_id = 'bc_blank'`).Scan(&blank.token, &blank.note, &blank.cleaned))
	require.NoError(t, db.QueryRow(`SELECT COALESCE(launch_token, '<nil>'), orphan_note, cleaned
		FROM review_job_cloud_launches WHERE agent_id = 'bc_null'`).Scan(&nullTok.token, &nullTok.note, &nullTok.cleaned))
	require.NoError(t, db.QueryRow(`SELECT COALESCE(launch_token, '<nil>'), orphan_note, cleaned
		FROM review_job_cloud_launches WHERE agent_id = 'bc_done'`).Scan(&done.token, &done.note, &done.cleaned))
	assert.Empty(t, blank.token)
	assert.Equal(t, 0, blank.cleaned)
	assert.Contains(t, blank.note, "orphan risk")
	assert.Empty(t, nullTok.token)
	assert.Equal(t, 0, nullTok.cleaned)
	assert.Contains(t, nullTok.note, "orphan risk")
	assert.Empty(t, done.token)
	assert.Equal(t, 1, done.cleaned)
	assert.Empty(t, done.note)
}

func TestPostgresCloudLaunchMigrationBackfillsUncleanedMarker(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	token := "pg-marker-" + uuid.New().String()
	cleanedToken := token + "-cleaned"
	t.Cleanup(func() {
		_, _ = pool.pool.Exec(context.Background(), `DELETE FROM review_job_cloud_launches WHERE launch_token IN ($1, $2)`, token, cleanedToken)
	})
	_, err := pool.pool.Exec(ctx, `ALTER TABLE review_job_cloud_launches DROP COLUMN IF EXISTS orphan_note`)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `INSERT INTO review_job_cloud_launches (job_id, launch_token, cleaned) VALUES
		(1, $1, FALSE), (1, $2, TRUE)`, token, cleanedToken)
	require.NoError(t, err)

	pool.ensureLocalCloudLaunchTable(ctx)

	var pending, done string
	require.NoError(t, pool.pool.QueryRow(ctx, `SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = $1`, token).Scan(&pending))
	require.NoError(t, pool.pool.QueryRow(ctx, `SELECT orphan_note FROM review_job_cloud_launches WHERE launch_token = $1`, cleanedToken).Scan(&done))
	assert.Contains(t, pending, "orphan risk")
	assert.Empty(t, done)
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
