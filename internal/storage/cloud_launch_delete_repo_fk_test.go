package storage

import (
	"context"
	"fmt"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
)

// TestDeleteRepoForeignKeysOnMatrix is the FK-on pin for repo delete.
// PRAGMA foreign_keys is connection-local. The older refusal test enables it
// on a spare connection, so DeleteRepo's own connection still has it off.
// This test opens the database with foreign_keys on for every connection.
func TestDeleteRepoForeignKeysOnMatrix(t *testing.T) {
	t.Parallel()
	db := openTestDBWithForeignKeys(t, 1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	var fk int
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk))
	require.Equal(t, 1, fk)

	assertDeleteRepoLaunchMatrix(t, db)

	// The table ensureCloudLaunchTable creates still declares ON DELETE CASCADE.
	// keepCloudLaunchAfterJobDelete rebuilds it away on Open. A database that
	// still has that constraint must not lose the launch row when jobs go.
	_, err = db.Exec(`DROP TABLE review_job_cloud_launches`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE review_job_cloud_launches (
		id INTEGER PRIMARY KEY,
		job_id INTEGER NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
		launch_token TEXT NOT NULL,
		agent_id TEXT NOT NULL DEFAULT '',
		cleaned INTEGER NOT NULL DEFAULT 0,
		repo_identity TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT '',
		orphan_note TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk))
	require.Equal(t, 1, fk, "DeleteRepo's connection class still has foreign keys on")

	t.Run("cascade-fk", func(t *testing.T) {
		assertDeleteRepoLaunchMatrix(t, db)
	})
}

func assertDeleteRepoLaunchMatrix(t *testing.T, db *DB) {
	t.Helper()
	cases := []struct {
		cascade bool
		kind    string
		wantErr error
		repo    bool
		launch  bool
	}{
		{cascade: true, kind: "uncleaned", wantErr: ErrRepoHasCloudLaunch, repo: true, launch: true},
		{cascade: false, kind: "uncleaned", wantErr: ErrRepoHasCloudLaunch, repo: true, launch: true},
		{cascade: true, kind: "cleaned", wantErr: nil, repo: false, launch: true},
		{cascade: false, kind: "cleaned", wantErr: ErrRepoHasJobs, repo: true, launch: true},
		{cascade: true, kind: "none", wantErr: nil, repo: false, launch: false},
		{cascade: false, kind: "none", wantErr: ErrRepoHasJobs, repo: true, launch: false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("cascade-%v-%s", tc.cascade, tc.kind), func(t *testing.T) {
			job := runningCloudJob(t, db)
			token := fmt.Sprintf("matrix-%v-%s-%d", tc.cascade, tc.kind, job.ID)
			if tc.kind != "none" {
				require.NoError(t, db.BeginCloudLaunch(job.ID, "w", token, "https://example.com/acme/repo.git"))
				if tc.kind == "cleaned" {
					_, err := db.Exec(`UPDATE review_job_cloud_launches SET cleaned = 1 WHERE launch_token = ?`, token)
					require.NoError(t, err)
				}
			}
			err := db.DeleteRepo(job.RepoID, tc.cascade)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			var launches, repos int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM review_job_cloud_launches WHERE launch_token = ?`, token).Scan(&launches))
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM repos WHERE id = ?`, job.RepoID).Scan(&repos))
			if tc.launch {
				require.Equal(t, 1, launches)
			} else {
				require.Equal(t, 0, launches)
			}
			if tc.repo {
				require.Equal(t, 1, repos)
			} else {
				require.Equal(t, 0, repos)
			}
		})
	}
}

func TestPostgresDeleteRepoForeignKeysOnMatrix(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	// An older binary in this stack created the launch table with
	// ON DELETE CASCADE. CREATE TABLE IF NOT EXISTS leaves that constraint.
	_, err := pool.pool.Exec(ctx, `DROP TABLE IF EXISTS review_job_cloud_launches`)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `CREATE TABLE review_job_cloud_launches (
		id BIGSERIAL PRIMARY KEY,
		job_id INTEGER NOT NULL REFERENCES review_jobs(id) ON DELETE CASCADE,
		launch_token TEXT NOT NULL,
		agent_id TEXT NOT NULL DEFAULT '',
		cleaned BOOLEAN NOT NULL DEFAULT FALSE,
		repo_identity TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		orphan_note TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	pool.ensureLocalCloudLaunchTable(ctx)

	cases := []struct {
		cascade bool
		kind    string
		wantErr error
		repo    bool
		launch  bool
	}{
		{cascade: true, kind: "uncleaned", wantErr: ErrRepoHasCloudLaunch, repo: true, launch: true},
		{cascade: false, kind: "uncleaned", wantErr: ErrRepoHasCloudLaunch, repo: true, launch: true},
		{cascade: true, kind: "cleaned", wantErr: nil, repo: false, launch: true},
		{cascade: false, kind: "cleaned", wantErr: ErrRepoHasJobs, repo: true, launch: true},
		{cascade: true, kind: "none", wantErr: nil, repo: false, launch: false},
		{cascade: false, kind: "none", wantErr: ErrRepoHasJobs, repo: true, launch: false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("cascade-%v-%s", tc.cascade, tc.kind), func(t *testing.T) {
			token := fmt.Sprintf("pg-matrix-%v-%s-%s", tc.cascade, tc.kind, uuid.New().String())
			var repoID int64
			err := pool.pool.QueryRow(ctx, `INSERT INTO repos (identity) VALUES ($1) RETURNING id`, token).Scan(&repoID)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = pool.pool.Exec(context.Background(), `DELETE FROM review_job_cloud_launches WHERE launch_token = $1`, token)
				_, _ = pool.pool.Exec(context.Background(), `DELETE FROM review_jobs WHERE repo_id = $1`, repoID)
				_, _ = pool.pool.Exec(context.Background(), `DELETE FROM commits WHERE repo_id = $1`, repoID)
				_, _ = pool.pool.Exec(context.Background(), `DELETE FROM repos WHERE id = $1`, repoID)
			})
			var jobID int64
			err = pool.pool.QueryRow(ctx, `INSERT INTO review_jobs
				(uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
				VALUES ($1, $2, 'abc', 'test', 'running', NOW(), $3) RETURNING id`,
				uuid.New(), repoID, uuid.New()).Scan(&jobID)
			require.NoError(t, err)
			if tc.kind != "none" {
				cleaned := tc.kind == "cleaned"
				_, err = pool.pool.Exec(ctx, `INSERT INTO review_job_cloud_launches
					(job_id, launch_token, cleaned) VALUES ($1, $2, $3)`, jobID, token, cleaned)
				require.NoError(t, err)
			}
			err = pool.DeleteRepo(ctx, repoID, tc.cascade)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			var launches, repos int
			require.NoError(t, pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM review_job_cloud_launches WHERE launch_token = $1`, token).Scan(&launches))
			require.NoError(t, pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM repos WHERE id = $1`, repoID).Scan(&repos))
			if tc.launch {
				require.Equal(t, 1, launches)
			} else {
				require.Equal(t, 0, launches)
			}
			if tc.repo {
				require.Equal(t, 1, repos)
			} else {
				require.Equal(t, 0, repos)
			}
		})
	}
}
