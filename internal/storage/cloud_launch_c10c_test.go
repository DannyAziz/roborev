package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
		repo_identity TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT '',
		orphan_note TEXT NOT NULL DEFAULT '')`)
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

func TestLaunchRowSurvivesRepoDelete(t *testing.T) {
	t.Parallel()
	for _, fk := range []int{1, 0} {
		t.Run(fmt.Sprintf("fk-%d", fk), func(t *testing.T) {
			t.Parallel()
			db := openTestDBWithForeignKeys(t, fk)
			var got int
			require.NoError(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&got))
			require.Equal(t, fk, got)

			job := runningCloudJob(t, db)
			require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok-survive", "https://example.com/acme/repo.git"))
			_, err := db.Exec(`UPDATE review_job_cloud_launches SET agent_id = 'bc_pin' WHERE job_id = ?`, job.ID)
			require.NoError(t, err)
			before := snapshotLaunchRows(t, db)
			require.NotEmpty(t, before)

			_ = db.DeleteRepo(job.RepoID, true)
			assert.Equal(t, before, snapshotLaunchRows(t, db))
		})
	}
}

func openTestDBWithForeignKeys(t *testing.T, fk int) *DB {
	t.Helper()
	base := openTestDB(t)
	var path string
	require.NoError(t, base.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
	require.NoError(t, base.Close())
	raw, err := sql.Open("sqlite", fmt.Sprintf("%s?_pragma=foreign_keys(%d)&_pragma=busy_timeout(30000)", path, fk))
	require.NoError(t, err)
	db := &DB{DB: raw}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func snapshotLaunchRows(t *testing.T, db *DB) []map[string]string {
	t.Helper()
	colRows, err := db.Query(`SELECT name FROM pragma_table_info('review_job_cloud_launches') ORDER BY cid`)
	require.NoError(t, err)
	defer colRows.Close()
	var cols []string
	for colRows.Next() {
		var name string
		require.NoError(t, colRows.Scan(&name))
		cols = append(cols, name)
	}
	require.NoError(t, colRows.Err())
	require.NotEmpty(t, cols)

	quoted := make([]string, len(cols))
	for i, name := range cols {
		quoted[i] = `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
	rows, err := db.Query(`SELECT ` + strings.Join(quoted, ", ") + ` FROM review_job_cloud_launches ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		vals := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		require.NoError(t, rows.Scan(dest...))
		row := make(map[string]string, len(cols))
		for i, name := range cols {
			if vals[i] == nil {
				row[name] = ""
				continue
			}
			row[name] = fmt.Sprint(vals[i])
		}
		out = append(out, row)
	}
	require.NoError(t, rows.Err())
	return out
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
