package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const keepOrphanNote = "cursor-cloud orphan risk: keep-this-token"

func TestOrphanNoteShapeMatrix(t *testing.T) {
	t.Parallel()
	paths := []struct {
		name string
		red  string
		run  func(t *testing.T, db *DB, job *ReviewJob)
	}{
		{name: "retry", red: "retry cleared orphan note while launch uncleaned", run: runRetry},
		{name: "rerun", red: "rerun cleared orphan note while launch uncleaned", run: runRerun},
		{name: "fail", red: "fail cleared orphan note while launch uncleaned", run: runFail},
		{name: "cancel", red: "cancel cleared orphan note while launch uncleaned", run: runCancel},
		{name: "stale", red: "ResetStaleJobs cleared orphan note while launch uncleaned", run: runStale},
		{name: "sync", red: "sync replay cleared orphan note while launch uncleaned", run: runSync},
		{name: "backup", red: "backup-agent failure cleared orphan note while launch uncleaned", run: runBackupFail},
	}
	for _, path := range paths {
		for _, noteSet := range []bool{true, false} {
			for _, cleaned := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/note=%t/cleaned=%t", path.name, noteSet, cleaned), func(t *testing.T) {
					db, job := seedOrphanShape(t, noteSet, cleaned)
					path.run(t, db, job)
					stored := storedJobError(t, db, job.ID)
					if noteSet && !cleaned {
						assert.Contains(t, stored, keepOrphanNote, path.red)
						assert.Equal(t, keepOrphanNote, storedLaunchNote(t, db, job.ID), path.red)
						return
					}
					assert.NotContains(t, stored, keepOrphanNote)
					if !noteSet || cleaned {
						assert.Empty(t, storedLaunchNote(t, db, job.ID))
					}
				})
			}
		}
	}
}

func seedOrphanShape(t *testing.T, noteSet, cleaned bool) (*DB, *ReviewJob) {
	t.Helper()
	db := openTestDB(t)
	_, _, job := createJobChain(t, db, t.TempDir(), "abc123sha")
	errText := ""
	if noteSet {
		errText = keepOrphanNote
	}
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'w1', error = NULLIF(?, '') WHERE id = ?`, errText, job.ID)
	require.NoError(t, err)
	note := ""
	if noteSet && !cleaned {
		note = keepOrphanNote
	}
	cleanedBit := 0
	if cleaned {
		cleanedBit = 1
	}
	_, err = db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note, cleaned) VALUES (?, ?, ?, ?)`,
		job.ID, fmt.Sprintf("tok-%d", job.ID), note, cleanedBit)
	require.NoError(t, err)
	return db, job
}

func storedJobError(t *testing.T, db *DB, jobID int64) string {
	t.Helper()
	var text string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, jobID).Scan(&text))
	return text
}

func storedLaunchNote(t *testing.T, db *DB, jobID int64) string {
	t.Helper()
	var note string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(orphan_note, '') FROM review_job_cloud_launches WHERE job_id = ?`, jobID).Scan(&note))
	return note
}

func runRetry(t *testing.T, db *DB, job *ReviewJob) {
	t.Helper()
	ok, err := db.RetryJob(job.ID, "w1", 3, 0)
	require.NoError(t, err)
	require.True(t, ok)
}

func runRerun(t *testing.T, db *DB, job *ReviewJob) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET status = 'failed', worker_id = NULL WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}))
}

func runFail(t *testing.T, db *DB, job *ReviewJob) {
	t.Helper()
	ok, err := db.FailJob(job.ID, "w1", "disk failed")
	require.NoError(t, err)
	require.True(t, ok)
}

func runCancel(t *testing.T, db *DB, job *ReviewJob) {
	t.Helper()
	require.NoError(t, db.CancelJobWithError(job.ID, "timeout cancel"))
}

func runStale(t *testing.T, db *DB, job *ReviewJob) {
	t.Helper()
	require.NoError(t, db.ResetStaleJobs())
	_ = job
}

func runSync(t *testing.T, db *DB, job *ReviewJob) {
	t.Helper()
	require.NotNil(t, job.UUID)
	require.NotNil(t, job.SourceMachineID)
	require.NoError(t, db.UpsertPulledJob(PulledJob{
		UUID:            *job.UUID,
		GitRef:          job.GitRef,
		Agent:           job.Agent,
		Status:          "failed",
		Error:           "origin replay",
		EnqueuedAt:      job.EnqueuedAt,
		UpdatedAt:       time.Now().Add(time.Hour),
		SourceMachineID: *job.SourceMachineID,
	}, job.RepoID, job.CommitID))
}

func runBackupFail(t *testing.T, db *DB, job *ReviewJob) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET agent = 'backup-fail' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	ok, err := db.FailBackupAgentJob(job.ID, "w1", "backup blew up")
	require.NoError(t, err)
	require.True(t, ok)
}

// TestClassifyRewriteKeepsOrphanNote is the auto-design classify pin. A
// classify attempt that launched cursor-cloud leaves an uncleaned launch
// and an orphan note on the job error. Promoting it to a design review, or
// marking it skipped with an error detail, rewrites that error in place.
// Writing NULL or the detail over the note drops the warning from the job.
func TestClassifyRewriteKeepsOrphanNote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, db *DB, jobID int64)
	}{
		{name: "promote", run: func(t *testing.T, db *DB, jobID int64) {
			require.NoError(t, db.PromoteClassifyToDesignReview(jobID, "w1", "claude-code", ""))
		}},
		{name: "skip", run: func(t *testing.T, db *DB, jobID int64) {
			require.NoError(t, db.MarkClassifyAsSkippedDesign(jobID, "w1", "classifier failed", "classifier: timeout"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openTestDB(t)
			t.Cleanup(func() { db.Close() })
			jobID := seedRunningClassify(t, db, t.TempDir(), "abc", "w1")
			_, err := db.Exec(`INSERT INTO review_job_cloud_launches
				(job_id, launch_token, orphan_note) VALUES (?, ?, ?)`, jobID, fmt.Sprintf("classify-tok-%d", jobID), keepOrphanNote)
			require.NoError(t, err)
			require.NoError(t, db.NoteCloudLaunchRisk(jobID, keepOrphanNote))

			tc.run(t, db, jobID)
			assert.Contains(t, storedJobError(t, db, jobID), keepOrphanNote)
			assert.Equal(t, keepOrphanNote, storedLaunchNote(t, db, jobID))
		})
	}
}
