package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestRestartClientWritesOrphanNote(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	job := cursorCloudBareUncleaned(t, tc, "client-note-tok")
	err := recoverCloudLaunches(context.Background(), tc.DB, nil, nil)
	require.Error(t, err)
	assertRestartNoteWritten(t, tc, job.ID, "restart client returned without writing the orphan note")
}

func TestRestartListWritesOrphanNote(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	job := cursorCloudBareUncleaned(t, tc, "list-note-tok")
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodList: &cursorcloud.APIError{
			Op: "list agents", StatusCode: http.StatusInternalServerError, Message: "unavailable",
		},
	}
	err := recoverCloudLaunches(context.Background(), tc.DB, fake, nil)
	require.Error(t, err)
	assertRestartNoteWritten(t, tc, job.ID, "restart list returned without writing the orphan note")
}

func TestRestartSweepWritesOrphanNote(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := cursorCloudBareUncleaned(t, tc, "sweep-note-tok")
	created := cursorCloudShowToken(t, tc, fake, sha, "sweep-note-tok")
	err := recoverCloudLaunches(context.Background(), tc.DB, &cursorCloudRefuseDelete{Fake: fake, id: created.ID}, nil)
	require.Error(t, err)
	assertRestartNoteWritten(t, tc, job.ID, "restart sweep returned without writing the orphan note")
}

func TestRestartNoteShapeMatrix(t *testing.T) {
	paths := []struct {
		name string
		red  string
		run  func(t *testing.T, tc *workerTestContext, job *storage.ReviewJob)
	}{
		{name: "client", red: "restart client returned without writing the orphan note", run: runRestartClient},
		{name: "list", red: "restart list returned without writing the orphan note", run: runRestartList},
		{name: "sweep", red: "restart sweep returned without writing the orphan note", run: runRestartSweep},
	}
	for _, path := range paths {
		for _, noteSet := range []bool{true, false} {
			for _, cleaned := range []bool{true, false} {
				t.Run(path.name+"/note="+boolName(noteSet)+"/cleaned="+boolName(cleaned), func(t *testing.T) {
					tc, _ := cursorCloudRestartFixture(t)
					job := cursorCloudShapeLaunch(t, tc, path.name, noteSet, cleaned)
					path.run(t, tc, job)
					stored, note, rowCleaned := cursorCloudNoteState(t, tc, job.ID)
					if cleaned {
						assert.Equal(t, 1, rowCleaned)
						if noteSet {
							assert.Contains(t, stored, "cursor-cloud orphan risk: keep-this-token")
						} else {
							assert.NotContains(t, stored, "orphan risk")
						}
						assert.Empty(t, note)
						return
					}
					assert.Equal(t, 0, rowCleaned)
					assert.Contains(t, stored, "orphan risk", path.red)
					assert.Contains(t, note, "orphan risk", path.red)
					if noteSet {
						assert.Contains(t, stored, "cursor-cloud orphan risk: keep-this-token", path.red)
						assert.Equal(t, "cursor-cloud orphan risk: keep-this-token", note, path.red)
					}
				})
			}
		}
	}
}

func TestBackupAgentFailureKeepsOrphanNote(t *testing.T) {
	const backup = "backup-fail-note"
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: backup,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			return "", errors.New("backup blew up")
		},
	})
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, "backup-note-tok", cursorCloudRestartRepo))
	require.NoError(t, tc.DB.NoteCloudLaunchRisk(job.ID, "cursor-cloud orphan risk: keep-this-token"))
	ok, err := tc.DB.FailoverJob(job.ID, testWorkerID, backup, "")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET retry_count = 3 WHERE id = ?`, job.ID)
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	failed := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, backup, failed.Agent)
	var stored string
	require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&stored))
	assert.Contains(t, stored, "cursor-cloud orphan risk: keep-this-token",
		"backup-agent failure cleared orphan note while launch uncleaned")
	assert.Contains(t, stored, "backup blew up")
}

func cursorCloudBareUncleaned(t *testing.T, tc *workerTestContext, token string) *storage.ReviewJob {
	t.Helper()
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	cursorCloudInsertBareLaunch(t, tc, job.ID, token, "")
	return job
}

func cursorCloudShapeLaunch(t *testing.T, tc *workerTestContext, path string, noteSet, cleaned bool) *storage.ReviewJob {
	t.Helper()
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	token := path + "-shape-tok"
	note := ""
	errText := ""
	if noteSet {
		note = "cursor-cloud orphan risk: keep-this-token"
		errText = note
	}
	cleanedBit := 0
	if cleaned {
		cleanedBit = 1
		note = ""
	}
	_, err := tc.DB.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, repo_identity, orphan_note, cleaned, created_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		job.ID, token, cursorCloudRestartRepo, note, cleanedBit)
	require.NoError(t, err)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET error = NULLIF(?, '') WHERE id = ?`, errText, job.ID)
	require.NoError(t, err)
	return job
}

func runRestartClient(t *testing.T, tc *workerTestContext, job *storage.ReviewJob) {
	t.Helper()
	_ = recoverCloudLaunches(context.Background(), tc.DB, nil, nil)
	_ = job
}

func runRestartList(t *testing.T, tc *workerTestContext, job *storage.ReviewJob) {
	t.Helper()
	fake := &cursorcloudtest.Fake{}
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodList: &cursorcloud.APIError{
			Op: "list agents", StatusCode: http.StatusInternalServerError, Message: "unavailable",
		},
	}
	_ = recoverCloudLaunches(context.Background(), tc.DB, fake, nil)
	_ = job
}

func runRestartSweep(t *testing.T, tc *workerTestContext, job *storage.ReviewJob) {
	t.Helper()
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	fake := &cursorcloudtest.Fake{}
	var token string
	require.NoError(t, tc.DB.QueryRow(`SELECT launch_token FROM review_job_cloud_launches WHERE job_id = ?`, job.ID).Scan(&token))
	created := cursorCloudShowToken(t, tc, fake, sha, token)
	_ = recoverCloudLaunches(context.Background(), tc.DB, &cursorCloudRefuseDelete{Fake: fake, id: created.ID}, nil)
}

func assertRestartNoteWritten(t *testing.T, tc *workerTestContext, jobID int64, red string) {
	t.Helper()
	stored, note, cleaned := cursorCloudNoteState(t, tc, jobID)
	assert.Equal(t, 0, cleaned)
	assert.Contains(t, stored, "orphan risk", red)
	assert.Contains(t, note, "orphan risk", red)
}

func cursorCloudNoteState(t *testing.T, tc *workerTestContext, jobID int64) (stored, note string, cleaned int) {
	t.Helper()
	require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, jobID).Scan(&stored))
	require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(orphan_note, ''), cleaned FROM review_job_cloud_launches WHERE job_id = ?`, jobID).Scan(&note, &cleaned))
	return stored, note, cleaned
}

func boolName(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
