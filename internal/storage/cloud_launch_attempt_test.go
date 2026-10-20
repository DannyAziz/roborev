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
