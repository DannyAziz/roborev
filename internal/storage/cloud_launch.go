package storage

import (
	"fmt"
	"time"
)

const CloudLaunchInterruptedError = "cursor-cloud launch interrupted by daemon restart; not relaunched"

type CloudLaunch struct {
	ID       int64
	JobID    int64
	Token    string
	AgentID  string
	RepoPath string
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
	return db.execLaunch(`INSERT INTO review_job_cloud_launches (job_id, launch_token, repo_identity, created_at)
		SELECT id, ?, ?, datetime('now') FROM review_jobs WHERE id = ? AND status = 'running' AND worker_id = ?`,
		token, repoURL, jobID, workerID)
}

func (db *DB) AttachCloudAgent(jobID int64, token, agentID string) error {
	return db.execLaunch(`UPDATE review_job_cloud_launches SET agent_id = ?
		WHERE job_id = ? AND launch_token = ? AND agent_id = ''`, agentID, jobID, token)
}

func (db *DB) MarkCloudLaunchCleaned(launchID int64) error {
	_, err := db.Exec(`UPDATE review_job_cloud_launches SET cleaned = 1 WHERE id = ?`, launchID)
	return err
}

func (db *DB) ListUncleanedCloudLaunches() ([]CloudLaunch, error) {
	rows, err := db.Query(`SELECT l.id, l.job_id, l.launch_token, COALESCE(l.agent_id, ''), COALESCE(r.root_path, ''),
		COALESCE(l.repo_identity, ''), COALESCE(l.created_at, '')
		FROM review_job_cloud_launches l
		LEFT JOIN review_jobs j ON j.id = l.job_id
		LEFT JOIN repos r ON r.id = j.repo_id WHERE l.cleaned = 0 ORDER BY l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CloudLaunch
	for rows.Next() {
		var row CloudLaunch
		if err := rows.Scan(&row.ID, &row.JobID, &row.Token, &row.AgentID, &row.RepoPath, &row.RepoIdentity, &row.CreatedAt); err != nil {
			return nil, err
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
