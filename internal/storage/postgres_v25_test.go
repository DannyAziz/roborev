//go:build postgres

package storage

import (
	"context"
	_ "embed"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// postgresV24HostBackendR6Schema is schemas/postgres_v24.sql from
// DannyAziz/roborev host-backend-r6 (7e36e524), copied byte for byte. It is
// v23 plus review_jobs.backup_auto and has no review_job_cloud_launches.
//
//go:embed testdata/postgres_v24_host_backend_r6.sql
var postgresV24HostBackendR6Schema string

// e387CloudLaunchDDL is the launch table e387fe29 created: no repo_identity,
// created_at, orphan_note or sweep_seen.
const e387CloudLaunchDDL = `CREATE TABLE roborev.review_job_cloud_launches (
	id BIGSERIAL PRIMARY KEY,
	job_id INTEGER NOT NULL REFERENCES roborev.review_jobs(id) ON DELETE CASCADE,
	launch_token TEXT NOT NULL,
	agent_id TEXT NOT NULL DEFAULT '',
	cleaned BOOLEAN NOT NULL DEFAULT FALSE)`

// pgSchemaVersions lists every schema_version row, lowest first.
func pgSchemaVersions(t *testing.T, pool *PgPool) []int {
	t.Helper()
	rows, err := pool.pool.Query(t.Context(), `SELECT version FROM roborev.schema_version ORDER BY version`)
	require.NoError(t, err)
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		require.NoError(t, rows.Scan(&v))
		out = append(out, v)
	}
	require.NoError(t, rows.Err())
	return out
}

// pgColumnShape is every roborev column as table.column type nullable
// default, sorted, so two install paths can be compared.
func pgColumnShape(t *testing.T, pool *PgPool) []string {
	t.Helper()
	rows, err := pool.pool.Query(t.Context(), `
		SELECT table_name || '.' || column_name || ' ' || data_type || ' ' ||
		       is_nullable || ' ' || COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'roborev'
		ORDER BY table_name, column_name`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var col string
		require.NoError(t, rows.Scan(&col))
		out = append(out, col)
	}
	require.NoError(t, rows.Err())
	return out
}

// freshPgV25Shape installs a fresh database and returns its column shape.
func freshPgV25Shape(t *testing.T) []string {
	t.Helper()
	raw, err := NewPgPool(t.Context(), getTestPostgresURL(t), DefaultPgPoolConfig())
	require.NoError(t, err)
	_, err = raw.pool.Exec(t.Context(), `DROP SCHEMA IF EXISTS roborev CASCADE`)
	raw.Close()
	require.NoError(t, err)
	pool := openTestPgPool(t)
	defer pool.Close()
	return pgColumnShape(t, pool)
}

// dropRoborevSchemaAtCleanup leaves the shared database empty so the next
// test installs fresh instead of inheriting a seeded or refused schema.
func dropRoborevSchemaAtCleanup(t *testing.T) {
	t.Helper()
	connString := getTestPostgresURL(t)
	t.Cleanup(func() {
		ctx := context.Background()
		pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
		if err != nil {
			return
		}
		defer pool.Close()
		_, _ = pool.pool.Exec(ctx, `DROP SCHEMA IF EXISTS roborev CASCADE`)
	})
}

// seedPgV25Job inserts one repo and one job straight into the old schema.
func seedPgV25Job(t *testing.T, pool *PgPool, identity string) (int64, uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	var repoID int64
	require.NoError(t, pool.pool.QueryRow(ctx,
		`INSERT INTO roborev.repos (identity) VALUES ($1) RETURNING id`, identity).Scan(&repoID))
	jobUUID := uuid.New()
	_, err := pool.pool.Exec(ctx, `
		INSERT INTO roborev.review_jobs (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'abc', 'test', 'done', NOW(), $3)`, jobUUID, repoID, uuid.New())
	require.NoError(t, err)
	return repoID, jobUUID
}

func TestIntegration_PgV25FreshDatabase(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	dropRoborevSchemaAtCleanup(t)
	shape := freshPgV25Shape(t)
	pool := openTestPgPool(t)

	assert.Equal(t, []int{25}, pgSchemaVersions(t, pool))
	assert.Contains(t, shape, "review_job_cloud_launches.repo_identity text NO ''::text")
	assert.Contains(t, shape, "review_job_cloud_launches.sweep_seen integer NO 0")
	for _, col := range shape {
		assert.NotContains(t, col, "backup_auto")
	}
}

// TestIntegration_PgV25UpgradesV23 opens a v23 database that holds a job
// and an e387fe29 launch row. It ends at 25 with no 24 recorded, keeps both
// rows, and matches a fresh install column for column, including job_id.
func TestIntegration_PgV25UpgradesV23(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	dropRoborevSchemaAtCleanup(t)
	fresh := freshPgV25Shape(t)

	old := openTestPgPoolRawAtVersion(t, 23)
	ctx := t.Context()
	_, jobUUID := seedPgV25Job(t, old, "v23-upgrade.example")
	_, err := old.pool.Exec(ctx, e387CloudLaunchDDL)
	require.NoError(t, err)
	_, err = old.pool.Exec(ctx, `
		INSERT INTO roborev.review_job_cloud_launches (job_id, launch_token, agent_id)
		SELECT id, 'tok-v23', 'bc_v23' FROM roborev.review_jobs WHERE uuid = $1`, jobUUID)
	require.NoError(t, err)

	pool := openTestPgPool(t)
	assert.Equal(t, []int{23, 25}, pgSchemaVersions(t, pool))
	assert.Equal(t, fresh, pgColumnShape(t, pool))

	var status string
	require.NoError(t, pool.pool.QueryRow(ctx, `SELECT status FROM review_jobs WHERE uuid = $1`, jobUUID).Scan(&status))
	assert.Equal(t, "done", status)
	var token, agentID, identity, note string
	var cleaned bool
	require.NoError(t, pool.pool.QueryRow(ctx, `
		SELECT launch_token, agent_id, cleaned, repo_identity, orphan_note
		FROM review_job_cloud_launches`).Scan(&token, &agentID, &cleaned, &identity, &note))
	assert.Equal(t, "tok-v23", token)
	assert.Equal(t, "bc_v23", agentID)
	assert.False(t, cleaned)
	assert.Empty(t, identity)
	assert.Equal(t, CloudLaunchPendingNote, note)
}

// TestIntegration_PgV25UpgradesHostBackendR6V24 builds the database from
// host-backend-r6's v24 DDL. It opens without the "newer" refusal, ends at
// 25, keeps backup_auto and its values, and gains the launch table. A sync
// write from this binary leaves backup_auto alone.
func TestIntegration_PgV25UpgradesHostBackendR6V24(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	dropRoborevSchemaAtCleanup(t)
	fresh := freshPgV25Shape(t)

	old := openTestPgPoolRawAtVersion(t, 24)
	ctx := t.Context()
	repoID, jobUUID := seedPgV25Job(t, old, "r6-upgrade.example")
	_, err := old.pool.Exec(ctx, `UPDATE roborev.review_jobs SET backup_auto = TRUE WHERE uuid = $1`, jobUUID)
	require.NoError(t, err)

	pool := openTestPgPool(t)
	assert.Equal(t, []int{24, 25}, pgSchemaVersions(t, pool))
	want := append([]string{"review_jobs.backup_auto boolean NO false"}, fresh...)
	assert.ElementsMatch(t, want, pgColumnShape(t, pool))

	machineID := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machineID, "v25"))
	require.NoError(t, pool.UpsertJob(ctx, SyncableJob{
		UUID: jobUUID, GitRef: "abc", Agent: "test", Status: "failed",
		SourceMachineID: machineID, EnqueuedAt: time.Now(),
	}, repoID, nil))
	newUUID := uuid.New()
	require.NoError(t, pool.UpsertJob(ctx, SyncableJob{
		UUID: newUUID, GitRef: "def", Agent: "test", Status: "done",
		SourceMachineID: machineID, EnqueuedAt: time.Now(),
	}, repoID, nil))

	var status string
	var auto bool
	require.NoError(t, pool.pool.QueryRow(ctx,
		`SELECT status, backup_auto FROM review_jobs WHERE uuid = $1`, jobUUID).Scan(&status, &auto))
	assert.Equal(t, "failed", status)
	assert.True(t, auto, "r6's backup_auto must survive the upgrade and a v25 write")
	require.NoError(t, pool.pool.QueryRow(ctx,
		`SELECT backup_auto FROM review_jobs WHERE uuid = $1`, newUUID).Scan(&auto))
	assert.False(t, auto)
	pulled, _, err := pool.PullJobs(ctx, uuid.New(), "", 10)
	require.NoError(t, err)
	assert.Len(t, pulled, 2)
}

// TestIntegration_PgV25Reopen opens a v25 database twice. Nothing is
// recorded, lowered or lost on the second open.
func TestIntegration_PgV25Reopen(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	dropRoborevSchemaAtCleanup(t)
	fresh := freshPgV25Shape(t)
	pool := openTestPgPool(t)
	ctx := t.Context()
	_, jobUUID := seedPgV25Job(t, pool, "v25-reopen.example")
	_, err := pool.pool.Exec(ctx, `
		INSERT INTO review_job_cloud_launches (job_id, launch_token, repo_identity, orphan_note)
		SELECT id, 'tok-v25', 'https://example.com/r.git', $2 FROM review_jobs WHERE uuid = $1`, jobUUID, CloudLaunchPendingNote)
	require.NoError(t, err)
	cols := pgColumnNames(t, ctx, pool, "review_job_cloud_launches")
	before := pgTableRows(t, ctx, pool, "review_job_cloud_launches", cols)

	require.NoError(t, pool.EnsureSchema(ctx))
	assert.Equal(t, []int{25}, pgSchemaVersions(t, pool))
	assert.Equal(t, fresh, pgColumnShape(t, pool))
	assert.Equal(t, before, pgTableRows(t, ctx, pool, "review_job_cloud_launches", cols))
}

// TestIntegration_PgV26Refused records 26 on a v25 database. EnsureSchema
// refuses it as newer and writes no version row.
func TestIntegration_PgV26Refused(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	dropRoborevSchemaAtCleanup(t)
	freshPgV25Shape(t)
	pool := openTestPgPool(t)
	ctx := t.Context()
	_, err := pool.pool.Exec(ctx, `INSERT INTO schema_version (version) VALUES (26)`)
	require.NoError(t, err)

	err = pool.EnsureSchema(ctx)
	require.EqualError(t, err, fmt.Sprintf("database schema version 26 is newer than supported version %d", 25))
	assert.Equal(t, []int{25, 26}, pgSchemaVersions(t, pool))
}
