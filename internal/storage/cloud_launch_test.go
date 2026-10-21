package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	verifierWorker      = "verifier-worker"
	cloudCommandLine    = "cursor-cloud (Cursor cloud agents API)"
	primaryCommandLine  = "codex review"
	verifierCloudBackup = "cursor-cloud"
)

// TestVerifierPrimaryAttemptWithCloudBackupRequeuesBeforeCloudLaunch is bug
// (a). The primary has run, cursor-cloud is only the configured backup, and
// this attempt has no launch row. Restart must queue the job. The old
// predicate failed it because backup_agent was cursor-cloud.
func TestVerifierPrimaryAttemptWithCloudBackupRequeuesBeforeCloudLaunch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	job := seedVerifierAttempt(t, db, verifierShape{
		cloudPrimary: false, backupSelected: false, launch: "none",
	})
	require.NoError(t, db.ResetStaleJobs())

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, got.Status)
	assert.NotEqual(t, CloudLaunchInterruptedError, got.Error)
}

// TestVerifierCleanedLaunchFromPriorRerunDoesNotPoisonRestart is bug (b).
// A cleaned launch row from the previous attempt must not fail a later
// manual rerun whose current attempt has not launched cursor-cloud.
func TestVerifierCleanedLaunchFromPriorRerunDoesNotPoisonRestart(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	job := seedVerifierAttempt(t, db, verifierShape{
		cloudPrimary: false, backupSelected: false, launch: "prior",
	})
	require.NoError(t, db.ResetStaleJobs())

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, got.Status)
	assert.NotEqual(t, CloudLaunchInterruptedError, got.Error)
}

// TestVerifierPlannedUpdateFalsePositiveDoesNotFail is the planned-update
// path (FailSentCloudLaunch). The same two false positives must stay
// running so the caller can queue them. A sent, uncleaned launch from this
// attempt must still fail.
func TestVerifierPlannedUpdateFalsePositiveDoesNotFail(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	t.Run("primary with cloud backup before launch", func(t *testing.T) {
		job := seedVerifierAttempt(t, db, verifierShape{
			cloudPrimary: false, backupSelected: false, launch: "none",
		})
		assertPlannedUpdateRequeues(t, db, job.ID)
	})
	t.Run("cleaned launch from prior rerun", func(t *testing.T) {
		job := seedVerifierAttempt(t, db, verifierShape{
			cloudPrimary: false, backupSelected: false, launch: "prior",
		})
		assertPlannedUpdateRequeues(t, db, job.ID)
	})
	t.Run("sent uncleaned this attempt still fails", func(t *testing.T) {
		job := seedVerifierAttempt(t, db, verifierShape{
			cloudPrimary: false, backupSelected: false, launch: "sent",
		})
		failed, err := db.FailSentCloudLaunch(job.ID, verifierWorker)
		require.NoError(t, err)
		assert.True(t, failed)
		assertVerifierFailed(t, db, job.ID)
	})
}

// TestCloudLaunchRestartShapeMatrix is cloud-as-primary vs backup, backup
// selected or not, and launch row none / sent-uncleaned this attempt /
// cleaned this attempt / prior attempt. Sent-uncleaned and cleaned rows
// from this attempt fail. A prior row does not. No row fails only when this
// attempt's agent is cursor-cloud and it has already been invoked.
func TestCloudLaunchRestartShapeMatrix(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	type cell struct {
		shape verifierShape
		id    int64
	}
	var cells []cell
	for _, primary := range []bool{true, false} {
		for _, selected := range []bool{true, false} {
			for _, launch := range []string{"none", "sent", "cleaned", "prior"} {
				shape := verifierShape{
					cloudPrimary: primary, backupSelected: selected, launch: launch,
				}
				job := seedVerifierAttempt(t, db, shape)
				cells = append(cells, cell{shape: shape, id: job.ID})
			}
		}
	}

	require.NoError(t, db.ResetStaleJobs())

	for _, cell := range cells {
		t.Run(cell.shape.name(), func(t *testing.T) {
			if cell.shape.wantFailed() {
				assertVerifierFailed(t, db, cell.id)
				return
			}
			got, err := db.GetJobByID(cell.id)
			require.NoError(t, err)
			assert.Equal(t, JobStatusQueued, got.Status)
			assert.NotEqual(t, CloudLaunchInterruptedError, got.Error)
		})
	}
}

type verifierShape struct {
	cloudPrimary   bool
	backupSelected bool
	launch         string
}

func (s verifierShape) name() string {
	role := "backup"
	if s.cloudPrimary {
		role = "primary"
	}
	selected := "not-selected"
	if s.backupSelected {
		selected = "selected"
	}
	return role + "/" + selected + "/" + s.launch
}

func (s verifierShape) wantFailed() bool {
	switch s.launch {
	case "sent", "cleaned":
		return true
	case "prior":
		return false
	default:
		return s.cloudPrimary
	}
}

func seedVerifierAttempt(t *testing.T, db *DB, shape verifierShape) *ReviewJob {
	t.Helper()
	repo := createRepo(t, db, t.TempDir())
	sha := shape.name()
	job := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, sha).ID, sha)

	agentName := "codex"
	backup := verifierCloudBackup
	command := primaryCommandLine
	if shape.cloudPrimary {
		agentName = "cursor-cloud"
		command = cloudCommandLine
		if !shape.backupSelected {
			backup = "codex"
		}
	} else if shape.backupSelected {
		command = cloudCommandLine
	}
	invoked := 1
	if shape.launch == "prior" && shape.cloudPrimary {
		// A new cloud-primary attempt that has not invoked must not be
		// failed by the previous attempt's row. agent_invoked=1 without a
		// current row is the separate primary pin.
		invoked = 0
	}
	started := time.Now().Add(-time.Minute).UTC()
	if shape.launch == "prior" {
		started = time.Now().UTC()
	}
	_, err := db.Exec(`UPDATE review_jobs
		SET status = 'running', worker_id = ?, agent = ?, backup_agent = ?,
		    agent_invoked = ?, command_line = ?, started_at = ?
		WHERE id = ?`,
		verifierWorker, agentName, backup, invoked, command, preciseTimestampAt(started), job.ID)
	require.NoError(t, err)

	switch shape.launch {
	case "none":
	case "sent", "cleaned":
		insertVerifierLaunch(t, db, job.ID, shape.name(), shape.launch == "cleaned", "")
	case "prior":
		insertVerifierLaunch(t, db, job.ID, shape.name(), true, formatSQLiteTimestamp(started.Add(-2*time.Hour)))
	default:
		t.Fatalf("unknown launch shape %q", shape.launch)
	}
	return job
}

func insertVerifierLaunch(t *testing.T, db *DB, jobID int64, token string, cleaned bool, createdAt string) {
	t.Helper()
	require.NoError(t, db.BeginCloudLaunch(jobID, verifierWorker, token, "https://example.com/acme/repo.git"))
	require.NoError(t, db.AttachCloudAgent(jobID, token, "bc_"+token))
	if cleaned {
		var id int64
		require.NoError(t, db.QueryRow(`SELECT id FROM review_job_cloud_launches WHERE launch_token = ?`, token).Scan(&id))
		require.NoError(t, db.MarkCloudLaunchCleaned(id))
	}
	if createdAt != "" {
		_, err := db.Exec(`UPDATE review_job_cloud_launches SET created_at = ? WHERE launch_token = ?`, createdAt, token)
		require.NoError(t, err)
	}
}

func assertVerifierFailed(t *testing.T, db *DB, jobID int64) {
	t.Helper()
	got, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusFailed, got.Status)
	var stored string
	require.NoError(t, db.QueryRow(`SELECT error FROM review_jobs WHERE id = ?`, jobID).Scan(&stored))
	assert.Equal(t, CloudLaunchInterruptedError, stored)
}

func assertPlannedUpdateRequeues(t *testing.T, db *DB, jobID int64) {
	t.Helper()
	failed, err := db.FailSentCloudLaunch(jobID, verifierWorker)
	require.NoError(t, err)
	assert.False(t, failed, "planned update must not fail this attempt")
	running, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, running.Status)

	requeued, err := db.RequeueUpdateInterruptedJob(jobID, verifierWorker)
	require.NoError(t, err)
	assert.True(t, requeued)
	got, err := db.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, got.Status)
}

func TestResetStaleJobsFailsUncleanedCloudLaunch(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, t.TempDir())
	launchJob := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "sha-launch").ID, "sha-launch")
	invoked := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "sha-invoked").ID, "sha-invoked")
	plain := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, "sha-plain").ID, "sha-plain")

	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`, "stale-worker", launchJob.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(launchJob.ID, "stale-worker", "tok-stale", "https://example.com/acme/repo.git"))
	_, err = db.Exec(
		`UPDATE review_jobs SET status = 'running', worker_id = ?, agent = ?, agent_invoked = 1 WHERE id = ?`,
		"stale-worker", "cursor-cloud", invoked.ID,
	)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = ? WHERE id = ?`, "stale-worker", plain.ID)
	require.NoError(t, err)

	require.NoError(t, db.ResetStaleJobs())

	assertVerifierFailed(t, db, launchJob.ID)
	assertVerifierFailed(t, db, invoked.ID)
	requeued, err := db.GetJobByID(plain.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, requeued.Status)
}

func runningLaunchJob(t *testing.T, db *DB, repo *Repo, sha string) *ReviewJob {
	t.Helper()
	job := enqueueJob(t, db, repo.ID, createCommit(t, db, repo.ID, sha).ID, sha)
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'w' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	job.RepoID = repo.ID
	return job
}

func TestBeginCloudLaunchWritesRowWithNote(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := createRepo(t, db, t.TempDir())
	job := runningLaunchJob(t, db, repo, "sha-begin")

	require.Error(t, db.BeginCloudLaunch(job.ID, "other-worker", "tok-miss", "https://example.com/acme/repo.git"))
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok", "https://example.com/acme/repo.git"))
	require.Error(t, db.BeginCloudLaunch(job.ID, "w", "tok", "https://example.com/acme/repo.git"), "duplicate token")

	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, job.ID, rows[0].JobID)
	assert.Equal(t, "tok", rows[0].Token)
	assert.Equal(t, "https://example.com/acme/repo.git", rows[0].RepoIdentity)
	assert.NotEmpty(t, rows[0].CreatedAt)

	require.Error(t, db.AttachCloudAgent(job.ID, "other-tok", "bc_x"))
	require.NoError(t, db.AttachCloudAgent(job.ID, "tok", "bc_1"))
	rows, err = db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	assert.Equal(t, "bc_1", rows[0].AgentID)
}

// The warning comes only from uncleaned launch rows. It is never written
// into review_jobs.error, so failing or retrying the job does not touch it
// and cleaning the row removes it.
func TestOrphanWarningComesFromUncleanedLaunchRows(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := createRepo(t, db, t.TempDir())
	job := runningLaunchJob(t, db, repo, "sha-warn")
	other := runningLaunchJob(t, db, repo, "sha-other")
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok-warn", "https://example.com/acme/repo.git"))

	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, CloudLaunchPendingNote, got.OrphanWarning)
	assert.Empty(t, got.Error)

	require.NoError(t, db.NoteCloudLaunchRisk(job.ID, "cursor-cloud orphan risk: delete failed"))
	_, err = db.FailJob(job.ID, "w", "agent failed")
	require.NoError(t, err)
	got, err = db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent failed", got.Error)
	assert.Equal(t, "cursor-cloud orphan risk: delete failed", got.OrphanWarning)

	jobs := []ReviewJob{{ID: job.ID}, {ID: other.ID}}
	require.NoError(t, db.AttachLaunchOrphanWarnings(jobs))
	assert.Equal(t, "cursor-cloud orphan risk: delete failed", jobs[0].OrphanWarning)
	assert.Empty(t, jobs[1].OrphanWarning)

	byID, err := db.GetJobsWithReviewsByIDs([]int64{job.ID, other.ID})
	require.NoError(t, err)
	assert.Equal(t, "cursor-cloud orphan risk: delete failed", byID[job.ID].Job.OrphanWarning)
	assert.Empty(t, byID[other.ID].Job.OrphanWarning)

	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NoError(t, db.MarkCloudLaunchCleaned(rows[0].ID))
	got, err = db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, got.OrphanWarning)
	assert.Equal(t, "agent failed", got.Error)
}

func TestMarkCloudLaunchCleanedByTokenIsScoped(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := createRepo(t, db, t.TempDir())
	job := runningLaunchJob(t, db, repo, "sha-a")
	other := runningLaunchJob(t, db, repo, "sha-b")
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok-a", "https://example.com/acme/repo.git"))
	require.NoError(t, db.BeginCloudLaunch(other.ID, "w", "tok-b", "https://example.com/acme/repo.git"))

	require.Error(t, db.MarkCloudLaunchCleanedByToken(job.ID, repo.ID, "tok-b"))
	require.Error(t, db.MarkCloudLaunchCleanedByToken(job.ID, repo.ID+1, "tok-a"))
	require.NoError(t, db.MarkCloudLaunchCleanedByToken(job.ID, repo.ID, "tok-a"))

	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "tok-b", rows[0].Token)
}

// Repo delete refuses while a launch is uncleaned. A deleted job keeps its
// launch row so recovery can still find the VM.
func TestCloudLaunchOutlivesJobAndBlocksRepoDelete(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	repo := createRepo(t, db, t.TempDir())
	job := runningLaunchJob(t, db, repo, "sha-del")
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "tok-del", "https://example.com/acme/repo.git"))

	require.ErrorIs(t, db.DeleteRepo(repo.ID, true), ErrRepoHasCloudLaunch)

	_, err := db.Exec(`DELETE FROM review_jobs WHERE id = ?`, job.ID)
	require.NoError(t, err)
	rows, err := db.ListUncleanedCloudLaunches()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, job.ID, rows[0].JobID)
	assert.Equal(t, "https://example.com/acme/repo.git", rows[0].RepoIdentity)
	assert.Empty(t, rows[0].RepoPath)

	require.NoError(t, db.MarkCloudLaunchCleaned(rows[0].ID))
	require.NoError(t, db.DeleteRepo(repo.ID, true))
}
