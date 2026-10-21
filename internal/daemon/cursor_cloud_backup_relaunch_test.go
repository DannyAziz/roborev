package daemon

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const backupCloudSynthesisFinal = `{"schema_version":2,"summary":"Combined.","verdict":"pass","findings":[{"severity":"medium","problem":"shared defect","fix":"fix it","location":"file.go:1","sources":[1,2]}]}`

// TestReviewBackupCloudCleanupCrashDoesNotRelaunch is the review crash pin.
// cursor-cloud is the backup, so the stored agent stays the primary. The
// launch is the review worker's bound Review: cleanup marks the row cleaned,
// then the process dies before CompleteJob. Restart must not queue another
// VM. Reverting the sent-launch gate requeues this job and the drain posts
// a second create.
func TestReviewBackupCloudCleanupCrashDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	tc, fake := cursorCloudRestartFixture(t)
	const primary = "review-backup-cloud-primary"
	agent.RegisterForTest(t, &unavailableSynthesisCommandAgent{
		name:    primary,
		command: "roborev-test-missing-review-primary",
	})
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, primary)
	_, err := tc.DB.Exec(`UPDATE review_jobs SET backup_agent = ? WHERE id = ?`, agent.CursorCloudHost, job.ID)
	require.NoError(t, err)
	job, err = tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, primary, job.Agent)
	require.Equal(t, agent.CursorCloudHost, job.BackupAgent)

	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = cursorCloudPassJSON
	cloud := newBoundCloud(t, tc, job, fake)
	tc.Pool.markAgentInvoked(testWorkerID, job, cloud)
	text, err := cloud.Review(context.Background(), tc.TmpDir, sha, "review the commit", nil)
	require.NoError(t, err)
	require.NotEmpty(t, text)
	running, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusRunning, running.Status)
	var invoked int
	require.NoError(t, tc.DB.QueryRow(`SELECT agent_invoked FROM review_jobs WHERE id = ?`, job.ID).Scan(&invoked))
	require.Equal(t, 1, invoked)
	assertBackupLaunchCleaned(t, tc, job.ID, primary)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.Equal(t, 1, creates)
	// The in-process delete already removed the VM. This start still has
	// the uncleaned row, so it does not queue another create.
	_ = cursorCloudRecover(t, tc, fake)
	drainRecoveredJob(t, tc, job.ID, false)

	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, storage.CloudLaunchInterruptedError)
	assert.Equal(t, primary, updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
}

// TestSynthesisBackupCloudCleanupCrashDoesNotRelaunch is the synthesis crash
// pin. The primary is unavailable, so the worker runs the cursor-cloud backup
// without rewriting job.Agent. Cleanup marks the launch cleaned, then an
// update interruption lands before CompleteJob. Recovery must fail the job
// instead of posting another VM. Reverting the sent-launch gate requeues it
// and the next synthesis run creates again.
func TestSynthesisBackupCloudCleanupCrashDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	tc, fake := cursorCloudRestartFixture(t)
	const primary = "synth-backup-cloud-primary"
	agent.RegisterForTest(t, &unavailableSynthesisCommandAgent{
		name:    primary,
		command: "roborev-test-missing-synth-primary",
	})
	const member = "synth-backup-cloud-member"
	registerPassingAgent(t, member)
	runUUID, members, _ := enqueuePanelRun(t, tc, "backup-cloud-panel", []memberSpec{
		{name: "m0", agent: member},
		{name: "m1", agent: member},
	})
	setSynthesisAgent(t, tc, runUUID, primary)
	setSynthesisBackup(t, tc, runUUID, agent.CursorCloudHost)
	for _, m := range members {
		completeDefectMember(t, tc, m.ID, member)
	}
	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	require.Equal(t, primary, synth.Agent)
	require.Equal(t, agent.CursorCloudHost, synth.BackupAgent)

	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = backupCloudSynthesisFinal
	tc.Pool.InterruptJobsForUpdate([]int64{synth.ID})
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
	assertBackupLaunchCleaned(t, tc, synth.ID, primary)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.Equal(t, 1, creates)
	drainRecoveredJob(t, tc, synth.ID, true)

	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	updated := tc.assertJobStatus(t, synth.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, storage.CloudLaunchInterruptedError)
	assert.Equal(t, primary, updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
}

// TestCIMemberCloudCleanupCrashDoesNotRelaunch is the CI member crash pin.
// cursor-cloud is the backup, cleanup marks the launch cleaned, and the
// process dies before CompleteJob. Restart must not queue another VM.
func TestCIMemberCloudCleanupCrashDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	tc, fake := cursorCloudRestartFixture(t)
	const primary = "ci-member-cloud-primary"
	agent.RegisterForTest(t, &unavailableSynthesisCommandAgent{
		name:    primary,
		command: "roborev-test-missing-ci-member-primary",
	})
	_, members, _ := enqueuePanelRun(t, tc, "ci-member-cloud", []memberSpec{
		{name: "m0", agent: primary},
	})
	_, err := tc.DB.Exec(
		`UPDATE review_jobs SET source = ?, backup_agent = ?, ci_base_branch = ? WHERE id = ?`,
		storage.JobSourceCI, agent.CursorCloudHost, "main", members[0].ID,
	)
	require.NoError(t, err)
	job, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, members[0].ID, job.ID)
	require.Equal(t, primary, job.Agent)
	require.True(t, job.IsCIReview())
	require.Equal(t, storage.PanelRoleMember, job.PanelRole)

	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = cursorCloudPassJSON
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	cloud := newBoundCloud(t, tc, job, fake)
	tc.Pool.markAgentInvoked(testWorkerID, job, cloud)
	text, err := cloud.Review(context.Background(), tc.TmpDir, sha, "review the commit", nil)
	require.NoError(t, err)
	require.NotEmpty(t, text)
	assertBackupLaunchCleaned(t, tc, job.ID, primary)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.Equal(t, 1, creates)
	// The review already deleted the VM. Restart sees that 404 as gone
	// and does not relaunch.
	_ = cursorCloudRecover(t, tc, fake)
	drainRecoveredJob(t, tc, job.ID, false)

	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, storage.CloudLaunchInterruptedError)
	assert.Equal(t, primary, updated.Agent)
	assert.Equal(t, storage.JobSourceCI, updated.Source)
	assert.Equal(t, 0, updated.RetryCount)
}

// TestCISynthesisCloudCleanupCrashDoesNotRelaunch is the CI synthesis crash
// pin. The primary is unavailable, so the worker runs the cursor-cloud backup
// without rewriting job.Agent. Cleanup marks the launch cleaned, then an
// update interruption lands before CompleteJob. Recovery must not post
// another VM.
func TestCISynthesisCloudCleanupCrashDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	tc, fake := cursorCloudRestartFixture(t)
	const primary = "ci-synth-backup-cloud-primary"
	agent.RegisterForTest(t, &unavailableSynthesisCommandAgent{
		name:    primary,
		command: "roborev-test-missing-ci-synth-primary",
	})
	const member = "ci-synth-backup-cloud-member"
	registerPassingAgent(t, member)
	runUUID, members, _ := enqueuePanelRun(t, tc, "ci-backup-cloud-panel", []memberSpec{
		{name: "m0", agent: member},
		{name: "m1", agent: member},
	})
	setSynthesisAgent(t, tc, runUUID, primary)
	setSynthesisBackup(t, tc, runUUID, agent.CursorCloudHost)
	_, err := tc.DB.Exec(
		`UPDATE review_jobs SET source = ?, ci_base_branch = ? WHERE panel_run_uuid = ? AND panel_role = 'synthesis'`,
		storage.JobSourceCI, "main", runUUID,
	)
	require.NoError(t, err)
	for _, m := range members {
		completeDefectMember(t, tc, m.ID, member)
	}
	synth := releaseAndClaimSynthesis(t, tc, runUUID)
	require.Equal(t, primary, synth.Agent)
	require.Equal(t, agent.CursorCloudHost, synth.BackupAgent)
	require.True(t, synth.IsCIReview())

	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = backupCloudSynthesisFinal
	tc.Pool.InterruptJobsForUpdate([]int64{synth.ID})
	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
	assertBackupLaunchCleaned(t, tc, synth.ID, primary)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.Equal(t, 1, creates)
	drainRecoveredJob(t, tc, synth.ID, true)

	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	updated := tc.assertJobStatus(t, synth.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, storage.CloudLaunchInterruptedError)
	assert.Equal(t, primary, updated.Agent)
	assert.Equal(t, storage.JobSourceCI, updated.Source)
	assert.Equal(t, 0, updated.RetryCount)
}

// TestClassifyToDesignCloudCleanupCrashDoesNotRelaunch is the design review
// a classifier promoted in place. cursor-cloud is the backup, cleanup marks
// the launch cleaned, and the process dies before CompleteJob. Restart must
// not queue another VM.
func TestClassifyToDesignCloudCleanupCrashDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	tc, fake := cursorCloudRestartFixture(t)
	const primary = "classify-design-cloud-primary"
	agent.RegisterForTest(t, &unavailableSynthesisCommandAgent{
		name:    primary,
		command: "roborev-test-missing-classify-design-primary",
	})
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	classify := tc.createAndClaimClassifyJob(t, sha, "feat: new package", "+lots of new code\n")
	require.NoError(t, tc.DB.PromoteClassifyToDesignReview(classify.ID, testWorkerID, primary, ""))
	_, err := tc.DB.Exec(`UPDATE review_jobs SET backup_agent = ? WHERE id = ?`, agent.CursorCloudHost, classify.ID)
	require.NoError(t, err)
	job, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, classify.ID, job.ID)
	require.Equal(t, "review", job.JobType)
	require.Equal(t, "design", job.ReviewType)
	require.Equal(t, "auto_design", job.Source)
	require.Equal(t, primary, job.Agent)

	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = cursorCloudPassJSON
	cloud := newBoundCloud(t, tc, job, fake)
	tc.Pool.markAgentInvoked(testWorkerID, job, cloud)
	text, err := cloud.Review(context.Background(), tc.TmpDir, sha, "review the commit", nil)
	require.NoError(t, err)
	require.NotEmpty(t, text)
	assertBackupLaunchCleaned(t, tc, job.ID, primary)

	creates := cursorCloudCallCount(fake, cursorcloudtest.MethodCreate)
	require.Equal(t, 1, creates)
	_ = cursorCloudRecover(t, tc, fake)
	drainRecoveredJob(t, tc, job.ID, false)

	assert.Equal(t, creates, cursorCloudCallCount(fake, cursorcloudtest.MethodCreate))
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, storage.CloudLaunchInterruptedError)
	assert.Equal(t, primary, updated.Agent)
	assert.Equal(t, "design", updated.ReviewType)
	assert.Equal(t, "auto_design", updated.Source)
	assert.Equal(t, 0, updated.RetryCount)
}

func assertBackupLaunchCleaned(t *testing.T, tc *workerTestContext, jobID int64, primary string) {
	t.Helper()
	job, err := tc.DB.GetJobByID(jobID)
	require.NoError(t, err)
	assert.Equal(t, primary, job.Agent)
	assert.Equal(t, agent.CursorCloudHost, job.BackupAgent)
	uncleaned, err := tc.DB.CountUncleanedCloudLaunches(jobID)
	require.NoError(t, err)
	assert.Equal(t, 0, uncleaned)
	var rows int
	require.NoError(t, tc.DB.QueryRow(
		`SELECT COUNT(*) FROM review_job_cloud_launches WHERE job_id = ?`, jobID,
	).Scan(&rows))
	assert.Equal(t, 1, rows)
}

func completeDefectMember(t *testing.T, tc *workerTestContext, jobID int64, ag string) {
	t.Helper()
	structured := jsontext.Value(`{
		"schema_version": 2,
		"summary": "One defect.",
		"verdict": "fail",
		"findings": [
			{"severity":"medium","problem":"shared defect","fix":"fix it","location":"file.go:1"}
		]
	}`)
	markMemberRunning(t, tc, jobID)
	require.NoError(t, tc.DB.CompleteJobResult(jobID, ag, "", storage.ReviewCompletion{
		Output:           "shared defect",
		Verdict:          storage.VerdictFail,
		StructuredOutput: structured,
	}))
}

func drainRecoveredJob(t *testing.T, tc *workerTestContext, jobID int64, synthesis bool) {
	t.Helper()
	workerID := testWorkerID + "-recover"
	for range maxRetries + 4 {
		claimed, err := tc.DB.ClaimJob(workerID)
		require.NoError(t, err)
		if claimed == nil || claimed.ID != jobID {
			return
		}
		if synthesis {
			tc.Pool.processSynthesisJob(context.Background(), workerID, claimed)
			continue
		}
		tc.Pool.processJob(workerID, claimed)
	}
}
