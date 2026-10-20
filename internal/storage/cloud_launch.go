package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// CloudLaunchPendingNote is stored on the launch row before POST /v0/agents.
const CloudLaunchPendingNote = "cursor-cloud orphan risk: launch pending cleanup"

const CloudLaunchInterruptedError = "cursor-cloud launch interrupted by daemon restart; not relaunched"

type CloudLaunch struct {
	ID    int64
	JobID int64
	// Token is the column text when it is not NULL. TokenNull means the
	// column itself is NULL; Token is then left unset.
	Token     string
	TokenNull bool
	AgentID   string
	RepoPath  string
	// RepoIdentity is the repository URL sent when this launch was created.
	// Recovery lists against it. A later change to the checkout remote is not
	// the create target.
	RepoIdentity string
	CreatedAt    string
}

func (db *DB) execLaunch(query string, args ...any) error {
	res, err := db.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("cursor-cloud: launch row was not updated")
	}
	return nil
}

func (db *DB) BeginCloudLaunch(jobID int64, workerID, token, repoURL string) error {
	if err := db.execLaunch(`INSERT INTO review_job_cloud_launches (job_id, launch_token, repo_identity, created_at)
		SELECT id, ?, ?, datetime('now') FROM review_jobs WHERE id = ? AND status = 'running' AND worker_id = ?`,
		token, repoURL, jobID, workerID); err != nil {
		return err
	}
	// The note is written before the caller may POST. A failed write removes the
	// new row and returns, so the POST does not run.
	if err := db.NoteLaunchOrphanRisk(token, CloudLaunchPendingNote); err != nil {
		_, _ = db.Exec(`DELETE FROM review_job_cloud_launches WHERE job_id = ? AND launch_token = ? AND orphan_note = ''`, jobID, token)
		return err
	}
	return nil
}

func (db *DB) AttachCloudAgent(jobID int64, token, agentID string) error {
	return db.execLaunch(`UPDATE review_job_cloud_launches SET agent_id = ?
		WHERE job_id = ? AND launch_token = ? AND agent_id = ''`, agentID, jobID, token)
}

// MarkCloudLaunchCleaned is the only statement that clears orphan_note.
func (db *DB) MarkCloudLaunchCleaned(launchID int64) error {
	_, err := db.Exec(`UPDATE review_job_cloud_launches SET cleaned = 1, orphan_note = '' WHERE id = ?`, launchID)
	return err
}

// MarkCloudLaunchCleanedByToken clears every uncleaned row for token.
// The token is unique per launch row and the row can outlive its job, so
// the job id is not part of the key. orphan_note changes only inside
// MarkCloudLaunchCleaned. Callers mark cleaned only after a nil or 404
// delete of every VM this token could own.
func (db *DB) MarkCloudLaunchCleanedByToken(token string) error {
	rows, err := db.Query(`SELECT id FROM review_job_cloud_launches WHERE launch_token = ? AND cleaned = 0 ORDER BY id`, token)
	if err != nil {
		return err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if err := db.MarkCloudLaunchCleaned(id); err != nil {
			return err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("cursor-cloud: launch was not cleaned")
	}
	return nil
}

func (db *DB) ListUncleanedCloudLaunches() ([]CloudLaunch, error) {
	rows, err := db.Query(`SELECT l.id, l.job_id, l.launch_token, COALESCE(l.agent_id, ''), COALESCE(r.root_path, ''),
		COALESCE(l.repo_identity, ''), COALESCE(l.created_at, '')
		FROM review_job_cloud_launches l
		LEFT JOIN review_jobs j ON j.id = l.job_id
		LEFT JOIN repos r ON r.id = j.repo_id
		WHERE l.cleaned = 0 ORDER BY l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CloudLaunch
	for rows.Next() {
		var row CloudLaunch
		var token sql.NullString
		if err := rows.Scan(&row.ID, &row.JobID, &token, &row.AgentID, &row.RepoPath, &row.RepoIdentity, &row.CreatedAt); err != nil {
			return nil, err
		}
		row.TokenNull = !token.Valid
		if token.Valid {
			row.Token = token.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// NoteCloudLaunchRisk records a launch that is still uncleaned. The job error
// is what status JSON shows. The row is not marked cleaned.
func (db *DB) NoteCloudLaunchRisk(jobID int64, msg string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE review_jobs SET error = CASE
		WHEN instr(COALESCE(error, ''), 'orphan risk') > 0 THEN error
		ELSE TRIM(COALESCE(error, '') || ' ' || ?)
		END, updated_at = ?, synced_at = NULL WHERE id = ?`, msg, now, jobID)
	return err
}

// FailSentCloudLaunch fails a running cursor-cloud attempt that already
// launched. Other jobs are unchanged, and the launch row stays uncleaned.
func (db *DB) FailSentCloudLaunch(jobID int64, workerID string) (bool, error) {
	now := time.Now().Format(time.RFC3339)
	res, err := db.Exec(`UPDATE review_jobs
		SET status = 'failed', worker_id = NULL, finished_at = ?, error = ?,
		    updated_at = ?, synced_at = NULL
		WHERE id = ? AND status = 'running' AND worker_id = ?
		  AND (id IN (SELECT job_id FROM review_job_cloud_launches WHERE cleaned = 0)
		    OR (agent = 'cursor-cloud' AND agent_invoked = 1))`,
		now, CloudLaunchInterruptedError, now, jobID, workerID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (db *DB) CountUncleanedCloudLaunches(jobID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM review_job_cloud_launches WHERE job_id = ? AND cleaned = 0`, jobID).Scan(&n)
	return n, err
}

// NoteLaunchOrphanRisk fills an empty orphan_note. It does not change a note
// that is already set. A write that updates zero rows returns an error,
// including when the token is missing or the note was already recorded.
func (db *DB) NoteLaunchOrphanRisk(token, msg string) error {
	token = strings.TrimSpace(token)
	msg = strings.TrimSpace(msg)
	if token == "" || msg == "" {
		return fmt.Errorf("cursor-cloud: launch note was not recorded")
	}
	res, err := db.Exec(`UPDATE review_job_cloud_launches SET orphan_note = ?
		WHERE launch_token = ? AND cleaned = 0 AND orphan_note = ''`, msg, token)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("cursor-cloud: launch note was not recorded")
	}
	return nil
}

// LaunchOrphanWarning reads uncleaned notes without requiring the job row.
func (db *DB) LaunchOrphanWarning(jobID int64) (string, error) {
	rows, err := db.Query(`SELECT l.orphan_note
		FROM review_job_cloud_launches l
		LEFT JOIN review_jobs j ON j.id = l.job_id
		WHERE l.job_id = ? AND l.cleaned = 0 AND l.orphan_note != ''
		ORDER BY l.id`, jobID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var notes []string
	for rows.Next() {
		var note string
		if err := rows.Scan(&note); err != nil {
			return "", err
		}
		notes = append(notes, note)
	}
	return strings.Join(notes, " "), rows.Err()
}

// ApplyLaunchOrphanWarning copies the launch note onto the API job status.
func ApplyLaunchOrphanWarning(job *ReviewJob, warning string) {
	warning = strings.TrimSpace(warning)
	if job == nil || warning == "" {
		return
	}
	job.OrphanWarning = warning
	if !strings.Contains(job.Error, warning) {
		job.Error = strings.TrimSpace(strings.TrimSpace(job.Error) + " " + warning)
	}
}

// keepCloudLaunchAfterJobDelete drops the job foreign key so deleting a job
// leaves the launch row, its id, and orphan_note in place. Ids are copied.
func (db *DB) keepCloudLaunchAfterJobDelete() error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='review_job_cloud_launches'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(ddl), "REFERENCES") {
		return nil
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE review_job_cloud_launches_keep (
			id INTEGER PRIMARY KEY,
			job_id INTEGER,
			launch_token TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			cleaned INTEGER NOT NULL DEFAULT 0,
			repo_identity TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT '',
			orphan_note TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO review_job_cloud_launches_keep
			(id, job_id, launch_token, agent_id, cleaned, repo_identity, created_at, orphan_note)
			SELECT id, job_id, launch_token, agent_id, cleaned,
				repo_identity, created_at, orphan_note
			FROM review_job_cloud_launches`,
		`DROP TABLE review_job_cloud_launches`,
		`ALTER TABLE review_job_cloud_launches_keep RENAME TO review_job_cloud_launches`,
	} {
		if _, err = tx.Exec(stmt); err != nil {
			return fmt.Errorf("keep cloud launch after job delete: %w", err)
		}
	}
	return tx.Commit()
}

// AttachLaunchOrphanWarnings fills OrphanWarning for an API job listing.
// A launch lookup error is returned; it is not skipped.
func (db *DB) AttachLaunchOrphanWarnings(jobs []ReviewJob) error {
	for i := range jobs {
		warning, err := db.LaunchOrphanWarning(jobs[i].ID)
		if err != nil {
			return fmt.Errorf("launch orphan warning: %w", err)
		}
		ApplyLaunchOrphanWarning(&jobs[i], warning)
	}
	return nil
}
