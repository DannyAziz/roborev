package storage

import "fmt"

const CloudLaunchInterruptedError = "cursor-cloud launch interrupted by daemon restart; not relaunched"

type CloudLaunch struct {
	ID       int64
	JobID    int64
	Token    string
	AgentID  string
	RepoPath string
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

func (db *DB) BeginCloudLaunch(jobID int64, workerID, token string) error {
	return db.execLaunch(`INSERT INTO review_job_cloud_launches (job_id, launch_token)
		SELECT id, ? FROM review_jobs WHERE id = ? AND status = 'running' AND worker_id = ?`,
		token, jobID, workerID)
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
	rows, err := db.Query(`SELECT l.id, l.job_id, l.launch_token, COALESCE(l.agent_id, ''), COALESCE(r.root_path, '')
		FROM review_job_cloud_launches l JOIN review_jobs j ON j.id = l.job_id
		LEFT JOIN repos r ON r.id = j.repo_id WHERE l.cleaned = 0 ORDER BY l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CloudLaunch
	for rows.Next() {
		var row CloudLaunch
		if err := rows.Scan(&row.ID, &row.JobID, &row.Token, &row.AgentID, &row.RepoPath); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (db *DB) CountUncleanedCloudLaunches(jobID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM review_job_cloud_launches WHERE job_id = ? AND cleaned = 0`, jobID).Scan(&n)
	return n, err
}
