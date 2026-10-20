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

// TestKnownIDDeleteCleansOnlyAfterNilOr404 still deletes a saved id. An
// empty token is not marked cleaned after that delete: there is no marker
// for any other VM, so the row stays uncleaned.
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
		job, id := cursorCloudEmptyTokenAgent(t, tc, fake, true)
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		_, err := fake.GetAgent(t.Context(), id)
		require.Error(t, err)
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		updated, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Contains(t, updated.Error, "orphan risk")
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
		assert.Equal(t, 1, n)
	})
}

// TestEmptyTokenCleansOnlyOnConfirmedEmptyList no longer lists. An empty
// token stays uncleaned whether or not other agents are present.
func TestEmptyTokenCleansOnlyOnConfirmedEmptyList(t *testing.T) {
	t.Run("non-empty list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudSeedAgent(t, fake, sha, "someone else")
		cursorCloudInsertEmptyToken(t, tc.DB, job.ID, "")
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodList))
	})
	t.Run("empty list", func(t *testing.T) {
		tc, fake := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		cursorCloudInsertEmptyToken(t, tc.DB, job.ID, "")
		require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodList))
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
		assert.NotContains(t, cursorCloudStoredError(t, tc.DB, job.ID), "orphan risk")
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
		assert.Empty(t, cursorCloudStoredError(t, tc.DB, job.ID))
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
		assert.Equal(t, "timeout cancel", cursorCloudStoredError(t, tc.DB, job.ID))
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
		storedErr := cursorCloudStoredError(t, tc.DB, job.ID)
		assert.Equal(t, "origin replay", storedErr)
		assert.NotContains(t, storedErr, "orphan risk")
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

func cursorCloudStoredError(t *testing.T, db *storage.DB, jobID int64) string {
	t.Helper()
	var text string
	require.NoError(t, db.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, jobID).Scan(&text))
	return text
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

func TestBatchJobStatusShowsLaunchOrphanWarning(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	srv := newServerWithLogs(tc.DB, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = srv.Close() })
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	quiet := tc.createJobWithAgent(t, sha, "test")
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, "batch-reader-tok", cursorCloudRestartRepo))

	body, err := json.Marshal(map[string][]int64{"job_ids": {job.ID, quiet.ID}})
	require.NoError(t, err)
	rr := serveHuma(t, srv, http.MethodPost, "/api/jobs/batch", body)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp struct {
		Results map[int64]storage.JobWithReview `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Contains(t, resp.Results[job.ID].Job.OrphanWarning, "orphan risk")
	assert.Contains(t, resp.Results[job.ID].Job.Error, "orphan risk")
	assert.Empty(t, resp.Results[quiet.ID].Job.OrphanWarning)
}

func TestJobStatusReadersSurfaceLaunchLookupError(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	srv := newServerWithLogs(tc.DB, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = srv.Close() })
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	_, err := tc.DB.Exec(`ALTER TABLE review_job_cloud_launches RENAME TO review_job_cloud_launches_src`)
	require.NoError(t, err)
	_, err = tc.DB.Exec(`CREATE VIEW review_job_cloud_launches AS
		SELECT id, job_id, launch_token, agent_id, cleaned, repo_identity, created_at
		FROM review_job_cloud_launches_src`)
	require.NoError(t, err)

	direct := serveHuma(t, srv, http.MethodGet, fmt.Sprintf("/api/jobs?id=%d", job.ID), nil)
	assert.Equal(t, http.StatusInternalServerError, direct.Code, "direct: %s", direct.Body.String())
	assert.Contains(t, direct.Body.String(), "orphan_note")

	list := serveHuma(t, srv, http.MethodGet, "/api/jobs", nil)
	assert.Equal(t, http.StatusInternalServerError, list.Code, "list: %s", list.Body.String())
	assert.Contains(t, list.Body.String(), "orphan_note")

	body, err := json.Marshal(map[string][]int64{"job_ids": {job.ID}})
	require.NoError(t, err)
	batch := serveHuma(t, srv, http.MethodPost, "/api/jobs/batch", body)
	assert.Equal(t, http.StatusInternalServerError, batch.Code, "batch: %s", batch.Body.String())
	assert.Contains(t, batch.Body.String(), "orphan_note")
}
