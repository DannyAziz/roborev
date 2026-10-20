package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestBeginCloudLaunchNoteFailureDoesNotPost(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	_, err := tc.DB.Exec(`CREATE TRIGGER roborev_fail_orphan_note
		BEFORE UPDATE OF orphan_note ON review_job_cloud_launches
		BEGIN
			SELECT RAISE(ABORT, 'note write failed');
		END`)
	require.NoError(t, err)

	posts := 0
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	cloud := &agent.CursorCloudAgent{
		Client:  &countingCreate{Fake: fake, posts: &posts},
		RepoURL: cursorCloudRestartRepo,
	}
	cloud.SetLaunchLimitsForTest(4, 20)
	cloud.BindLaunch = func(token, repoURL string) error {
		return tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, repoURL)
	}
	_, err = cloud.Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	assert.Equal(t, 0, posts)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

type countingCreate struct {
	*cursorcloudtest.Fake
	posts *int
}

func (c *countingCreate) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	*c.posts++
	return c.Fake.CreateAgent(ctx, req)
}

func TestKnownIDDeleteCleansOnlyAfterNilOr404(t *testing.T) {
	t.Run("delete error", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		job, id := cursorCloudEmptyTokenAgent(t, tc, fake, true)
		fake.MethodErrors = map[string]error{
			cursorcloudtest.MethodDelete: &cursorcloud.APIError{
				Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "no",
			},
		}
		require.Error(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Equal(t, id, cursorCloudLaunchAgentID(t, tc.DB, job.ID))
	})
	t.Run("nil delete", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		job, _ := cursorCloudEmptyTokenAgent(t, tc, fake, true)
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
		assert.Empty(t, cursorCloudLaunchNote(t, tc.DB, job.ID))
	})
	t.Run("404 delete", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		job, _ := cursorCloudEmptyTokenAgent(t, tc, fake, true)
		fake.MethodErrors = map[string]error{
			cursorcloudtest.MethodDelete: &cursorcloud.APIError{
				Op: "delete agent", StatusCode: http.StatusNotFound, Message: "gone",
			},
		}
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
}

func TestEmptyTokenCleansOnlyOnConfirmedEmptyList(t *testing.T) {
	t.Run("non-empty list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudSeedAgent(t, fake, sha, "someone else")
		cursorCloudInsertEmptyToken(t, tc.DB, job.ID, "")
		require.Error(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})
	t.Run("empty list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudInsertEmptyToken(t, tc.DB, job.ID, "")
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
}

func TestInProcessKnownIDDeleteClearsNoteOnlyOnSuccess(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		assertInProcessClear(t, nil, true)
	})
	t.Run("404", func(t *testing.T) {
		assertInProcessClear(t, &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusNotFound, Message: "gone",
		}, true)
	})
	t.Run("delete error", func(t *testing.T) {
		assertInProcessClear(t, &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "no",
		}, false)
	})
}

func assertInProcessClear(t *testing.T, deleteErr error, cleaned bool) {
	t.Helper()
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = "ok"
	if deleteErr != nil {
		fake.MethodErrors = map[string]error{cursorcloudtest.MethodDelete: deleteErr}
	}
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	bound := tc.Pool.bindCursorCloudLaunch(testWorkerID, job, cloud)
	_, err := bound.Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	n, nerr := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, nerr)
	if cleaned {
		require.NoError(t, err)
		assert.Equal(t, 0, n)
		return
	}
	require.Error(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, cursorCloudLaunchNote(t, tc.DB, job.ID), "orphan risk")
}

func TestAPIJobStatusKeepsLaunchOrphanNote(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	srv := newServerWithLogs(tc.DB, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = srv.Close() })
	sha := testutil.GetHeadSHA(t, tc.TmpDir)

	t.Run("fail", func(t *testing.T) {
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, "fail-tok", cursorCloudRestartRepo))
		updated, err := tc.DB.FailJob(job.ID, testWorkerID, "disk failed")
		require.NoError(t, err)
		require.True(t, updated)
		cursorCloudAssertAPIWarning(t, srv, job.ID, true)
		stored, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.NotContains(t, stored.Error, "orphan risk")
		cursorCloudCleanToken(t, tc.DB, "fail-tok")
		cursorCloudAssertAPIWarning(t, srv, job.ID, false)
	})
	t.Run("failover", func(t *testing.T) {
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, "fail-over-tok", cursorCloudRestartRepo))
		ok, err := tc.DB.FailoverJob(job.ID, testWorkerID, "test", "")
		require.NoError(t, err)
		require.True(t, ok)
		cursorCloudAssertAPIWarning(t, srv, job.ID, true)
		stored, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Empty(t, stored.Error)
		assert.Equal(t, storage.JobStatusQueued, stored.Status)
		cursorCloudCleanToken(t, tc.DB, "fail-over-tok")
		cursorCloudAssertAPIWarning(t, srv, job.ID, false)
		_, err = tc.DB.Exec(`UPDATE review_jobs SET status = 'failed' WHERE id = ?`, job.ID)
		require.NoError(t, err)
	})
	t.Run("cancel", func(t *testing.T) {
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, "cancel-tok", cursorCloudRestartRepo))
		require.NoError(t, tc.DB.CancelJobWithError(job.ID, "timeout cancel"))
		cursorCloudAssertAPIWarning(t, srv, job.ID, true)
		stored, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, "timeout cancel", stored.Error)
		cursorCloudCleanToken(t, tc.DB, "cancel-tok")
		cursorCloudAssertAPIWarning(t, srv, job.ID, false)
	})
	t.Run("sync replay", func(t *testing.T) {
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, "sync-tok", cursorCloudRestartRepo))
		require.NotNil(t, job.UUID)
		require.NoError(t, tc.DB.UpsertPulledJob(storage.PulledJob{
			UUID:            *job.UUID,
			GitRef:          job.GitRef,
			Agent:           job.Agent,
			Status:          "failed",
			Error:           "origin replay",
			EnqueuedAt:      job.EnqueuedAt,
			UpdatedAt:       time.Now().Add(time.Hour),
			SourceMachineID: *job.SourceMachineID,
		}, job.RepoID, job.CommitID))
		cursorCloudAssertAPIWarning(t, srv, job.ID, true)
		stored, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, "origin replay", stored.Error)
		assert.NotContains(t, stored.Error, "orphan risk")
		cursorCloudCleanToken(t, tc.DB, "sync-tok")
		cursorCloudAssertAPIWarning(t, srv, job.ID, false)
	})
}

func cursorCloudAssertAPIWarning(t *testing.T, srv *Server, jobID int64, want bool) {
	t.Helper()
	rr := serveHuma(t, srv, http.MethodGet, fmt.Sprintf("/api/jobs?id=%d", jobID), nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var body struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.Len(t, body.Jobs, 1)
	if want {
		assert.Contains(t, body.Jobs[0].OrphanWarning, "orphan risk")
		assert.Contains(t, body.Jobs[0].Error, "orphan risk")
		return
	}
	assert.Empty(t, body.Jobs[0].OrphanWarning)
	assert.NotContains(t, body.Jobs[0].Error, "orphan risk")
}

func cursorCloudEmptyTokenAgent(t *testing.T, tc *workerTestContext, fake *cursorcloudtest.Fake, seed bool) (*storage.ReviewJob, string) {
	t.Helper()
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	id := "bc_missing"
	if seed {
		id = cursorCloudSeedAgent(t, fake, sha, "review").ID
	}
	cursorCloudInsertEmptyToken(t, tc.DB, job.ID, id)
	return job, id
}

func cursorCloudInsertEmptyToken(t *testing.T, db *storage.DB, jobID int64, agentID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, agent_id, repo_identity, orphan_note)
		VALUES (?, '', ?, ?, ?)`, jobID, agentID, cursorCloudRestartRepo, storage.CloudLaunchPendingNote)
	require.NoError(t, err)
}

func cursorCloudLaunchNote(t *testing.T, db *storage.DB, jobID int64) string {
	t.Helper()
	var note string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(orphan_note, '') FROM review_job_cloud_launches WHERE job_id = ?`, jobID).Scan(&note))
	return note
}

func cursorCloudLaunchAgentID(t *testing.T, db *storage.DB, jobID int64) string {
	t.Helper()
	var id string
	require.NoError(t, db.QueryRow(`SELECT agent_id FROM review_job_cloud_launches WHERE job_id = ?`, jobID).Scan(&id))
	return id
}

func cursorCloudCleanToken(t *testing.T, db *storage.DB, token string) {
	t.Helper()
	require.NoError(t, db.MarkCloudLaunchCleanedByToken(token))
}
