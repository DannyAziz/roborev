package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFailSentCloudLaunchLeavesRowUncleaned is the pin for a sent-launch
// failure that also marks the launch cleaned. The job fails. The row stays
// cleaned=0 and keeps its orphan note.
func TestFailSentCloudLaunchLeavesRowUncleaned(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`, "w-fail", job.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w-fail", "fail-sent-tok", "https://example.com/acme/repo.git"))

	failed, err := db.FailSentCloudLaunch(job.ID, "w-fail")
	require.NoError(t, err)
	assert.True(t, failed)

	var cleaned int
	var note, jobErr string
	require.NoError(t, db.QueryRow(
		`SELECT cleaned, orphan_note FROM review_job_cloud_launches WHERE launch_token = ?`, "fail-sent-tok",
	).Scan(&cleaned, &note))
	require.NoError(t, db.QueryRow(`SELECT error FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
	assert.Equal(t, 0, cleaned)
	assert.Equal(t, CloudLaunchPendingNote, note)
	assert.Equal(t, CloudLaunchInterruptedError, jobErr)
}

// TestAttachCloudAgentZeroRowsReturnsError is the pin for binding an agent
// id when no launch row matches. A zero-row update returns an error and does
// not write the id onto a different token.
func TestAttachCloudAgentZeroRowsReturnsError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`, "w-attach", job.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w-attach", "attach-tok", "https://example.com/acme/repo.git"))

	err = db.AttachCloudAgent(job.ID, "missing-tok", "bc_missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not updated")

	require.NoError(t, db.AttachCloudAgent(job.ID, "attach-tok", "bc_bound"))
	err = db.AttachCloudAgent(job.ID, "attach-tok", "bc_again")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not updated")

	var agentID string
	require.NoError(t, db.QueryRow(
		`SELECT agent_id FROM review_job_cloud_launches WHERE launch_token = ?`, "attach-tok",
	).Scan(&agentID))
	assert.Equal(t, "bc_bound", agentID)
}

// TestLaunchOrphanWarningSkipsCleanedRow is the pin for the cleaned predicate
// on the warning read. A cleaned row keeps no warning, even when a note was
// left on it.
func TestLaunchOrphanWarningSkipsCleanedRow(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, cleaned, orphan_note) VALUES (?, 'clean-note', 1, ?)`,
		job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	warning, err := db.LaunchOrphanWarning(job.ID)
	require.NoError(t, err)
	assert.Empty(t, warning)
}

// TestMarkCloudLaunchCleanedByTokenZeroRowsErrors is the pin for a token
// clean whose SELECT matches nothing (cloud_launch.go n == 0). Success would
// report a confirmed delete of a row that is not there.
func TestMarkCloudLaunchCleanedByTokenZeroRowsErrors(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'keep-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	err = db.MarkCloudLaunchCleanedByToken(job.ID, job.RepoID, "missing-tok")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not cleaned")

	var cleaned int
	var note string
	require.NoError(t, db.QueryRow(
		`SELECT cleaned, orphan_note FROM review_job_cloud_launches WHERE launch_token = 'keep-tok'`,
	).Scan(&cleaned, &note))
	assert.Equal(t, 0, cleaned)
	assert.Equal(t, CloudLaunchPendingNote, note)
}

// TestNoteCloudLaunchRiskLeavesRowUncleaned is the pin for recording the
// job error. That write does not mark the launch cleaned or clear its note.
func TestNoteCloudLaunchRiskLeavesRowUncleaned(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'risk-tok', ?)`, job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	const msg = "cursor-cloud orphan risk: pending cleanup for agent bc_risk"
	require.NoError(t, db.NoteCloudLaunchRisk(job.ID, msg))

	var cleaned int
	var note, jobErr string
	require.NoError(t, db.QueryRow(
		`SELECT cleaned, orphan_note FROM review_job_cloud_launches WHERE launch_token = 'risk-tok'`,
	).Scan(&cleaned, &note))
	require.NoError(t, db.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
	assert.Equal(t, 0, cleaned)
	assert.Equal(t, CloudLaunchPendingNote, note)
	assert.Contains(t, jobErr, "orphan risk")
}

// TestBeginCloudLaunchMissReturnsUpdateError is the pin for a launch insert
// that matches no running row. The helper reports that nothing was updated.
// The note path does not run, and no launch row is stored.
func TestBeginCloudLaunchMissReturnsUpdateError(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")

	err := db.BeginCloudLaunch(job.ID, "w-miss", "miss-tok", "https://example.com/acme/repo.git")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not updated")
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM review_job_cloud_launches WHERE job_id = ?`, job.ID).Scan(&n))
	assert.Equal(t, 0, n)
}

// TestFailSentCloudLaunchKeepsStoredOrphanNote is the pin for the job error
// FailSentCloudLaunch writes. The launch row is uncleaned and the job error
// already names the agent that may still be running. The interrupted error
// is added, and that orphan note stays. Writing the interrupted error over it
// drops the warning from the job.
func TestFailSentCloudLaunchKeepsStoredOrphanNote(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`, "w-keep", job.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w-keep", "keep-note-tok", "https://example.com/acme/repo.git"))
	const msg = "cursor-cloud orphan risk: pending cleanup for agent bc_keep: status 500"
	require.NoError(t, db.NoteCloudLaunchRisk(job.ID, msg))

	failed, err := db.FailSentCloudLaunch(job.ID, "w-keep")
	require.NoError(t, err)
	assert.True(t, failed)

	var jobErr string
	require.NoError(t, db.QueryRow(`SELECT error FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
	assert.Contains(t, jobErr, CloudLaunchInterruptedError)
	assert.Contains(t, jobErr, msg)
}
