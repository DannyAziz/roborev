package storage

import (
	"fmt"
	"strings"
	"time"
)

// CloudLaunchPendingNote is stored on the launch row before POST /v0/agents.
const CloudLaunchPendingNote = "cursor-cloud orphan risk: launch pending cleanup"

const CloudLaunchInterruptedError = "cursor-cloud launch interrupted by daemon restart; not relaunched"

type CloudLaunch struct {
	ID      int64
	JobID   int64
	RepoID  int64
	Token   string
	AgentID string
	// AgentDeleted means the delete of AgentID was confirmed.
	AgentDeleted bool
	RepoPath     string
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

// BeginCloudLaunch records the launch, with its pending orphan note, before
// POST /v0/agents. A job that is no longer this worker's running attempt
// inserts nothing and returns an error, so the POST does not run.
func (db *DB) BeginCloudLaunch(jobID int64, workerID, token, repoURL string) error {
	return db.execLaunch(`INSERT INTO review_job_cloud_launches (job_id, launch_token, repo_identity, orphan_note)
		SELECT id, ?, ?, ? FROM review_jobs WHERE id = ? AND status = 'running' AND worker_id = ?`,
		token, repoURL, CloudLaunchPendingNote, jobID, workerID)
}

func (db *DB) AttachCloudAgent(jobID int64, token, agentID string) error {
	return db.execLaunch(`UPDATE review_job_cloud_launches SET agent_id = ?
		WHERE job_id = ? AND launch_token = ? AND agent_id = ''`, agentID, jobID, token)
}

// MarkCloudLaunchCleaned marks one launch cleaned. A cleaned row no longer
// produces an orphan warning.
func (db *DB) MarkCloudLaunchCleaned(launchID int64) error {
	_, err := db.Exec(`UPDATE review_job_cloud_launches SET cleaned = 1, orphan_note = '' WHERE id = ?`, launchID)
	return err
}

// MarkCloudAgentDeleted records a confirmed delete of the launch's agent id.
// Restart then skips the delete by id and only sweeps by marker.
func (db *DB) MarkCloudAgentDeleted(jobID int64, token string) error {
	_, err := db.Exec(`UPDATE review_job_cloud_launches SET agent_deleted = 1
		WHERE job_id = ? AND launch_token = ? AND agent_id != ''`, jobID, token)
	return err
}

// MarkCloudLaunchCleanedByToken clears uncleaned rows for this job, repo,
// and token. The same token on another job or repo stays uncleaned.
// After the job row is deleted, recovery still cleans that launch by job id
// and token. orphan_note changes only inside MarkCloudLaunchCleaned.
func (db *DB) MarkCloudLaunchCleanedByToken(jobID, repoID int64, token string) error {
	rows, err := db.Query(`SELECT l.id
		FROM review_job_cloud_launches l
		LEFT JOIN review_jobs j ON j.id = l.job_id
		WHERE l.cleaned = 0 AND l.job_id = ? AND l.launch_token = ?
		  AND (j.repo_id = ? OR j.id IS NULL)
		ORDER BY l.id`, jobID, token, repoID)
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
	rows, err := db.Query(`SELECT l.id, l.job_id, COALESCE(j.repo_id, 0), l.launch_token, l.agent_id, l.agent_deleted, COALESCE(r.root_path, ''),
		l.repo_identity, l.created_at
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
		if err := rows.Scan(&row.ID, &row.JobID, &row.RepoID, &row.Token, &row.AgentID, &row.AgentDeleted, &row.RepoPath, &row.RepoIdentity, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// currentAttemptCloudLaunchSQL is the fail-not-relaunch predicate for a
// running review_jobs row. A launch row counts only for this attempt:
// created_at is at or after started_at. Cleanup can mark that row cleaned
// before CompleteJob, so cleaned is not the key. A row from an earlier
// attempt is not, cleaned or not. A running row with no started_at has no
// later attempt to separate, so any of its launch rows still count.
//
// agent_invoked with agent cursor-cloud is this attempt's primary. Attempt
// resets clear it. backup_agent is the configured failover, not evidence
// that the backup ran.
func currentAttemptCloudLaunchSQL() string {
	created := sqliteNormalizedTimestampExpr("l.created_at")
	started := sqliteNormalizedTimestampExpr("review_jobs.started_at")
	return `EXISTS (
		SELECT 1 FROM review_job_cloud_launches l
		WHERE l.job_id = review_jobs.id
		  AND (
			COALESCE(review_jobs.started_at, '') = ''
			OR ` + created + ` >= ` + started + `
		  )
	) OR (review_jobs.agent_invoked = 1 AND review_jobs.agent = 'cursor-cloud')`
}

// FailSentCloudLaunch fails a running attempt that already launched a
// cursor-cloud VM on this attempt. A sent row still counts after cleanup
// marks it cleaned. A configured cursor-cloud backup that did not launch,
// and a launch row from an earlier attempt, do not. Other jobs are unchanged.
func (db *DB) FailSentCloudLaunch(jobID int64, workerID string) (bool, error) {
	now := time.Now().Format(time.RFC3339)
	res, err := db.Exec(`UPDATE review_jobs
		SET status = 'failed', worker_id = NULL, finished_at = ?, error = ?,
		    updated_at = ?, synced_at = NULL
		WHERE id = ? AND status = 'running' AND worker_id = ?
		  AND (`+currentAttemptCloudLaunchSQL()+`)`,
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

// FormatLaunchOrphanWarning is the line readers print for an uncleaned launch note.
func FormatLaunchOrphanWarning(job *ReviewJob) string {
	if job == nil {
		return ""
	}
	warning := strings.TrimSpace(job.OrphanWarning)
	if warning == "" {
		return ""
	}
	return "Warning: " + warning
}

// AttachLaunchOrphanWarnings fills OrphanWarning from uncleaned launch rows,
// one query per 500 jobs.
func (db *DB) AttachLaunchOrphanWarnings(jobs []ReviewJob) error {
	notes := map[int64][]string{}
	for start := 0; start < len(jobs); start += 500 {
		batch := jobs[start:min(start+500, len(jobs))]
		ids := make([]any, len(batch))
		for i := range batch {
			ids[i] = batch[i].ID
		}
		if err := db.collectOrphanNotes(notes, ids); err != nil {
			return fmt.Errorf("launch orphan warning: %w", err)
		}
	}
	for i := range jobs {
		jobs[i].OrphanWarning = strings.Join(notes[jobs[i].ID], " ")
	}
	return nil
}

func (db *DB) collectOrphanNotes(notes map[int64][]string, ids []any) error {
	rows, err := db.Query(`SELECT job_id, orphan_note FROM review_job_cloud_launches
		WHERE cleaned = 0 AND orphan_note != '' AND job_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)
		ORDER BY id`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jobID int64
		var note string
		if err := rows.Scan(&jobID, &note); err != nil {
			return err
		}
		notes[jobID] = append(notes[jobID], note)
	}
	return rows.Err()
}
