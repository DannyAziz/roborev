package daemon

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestAPIReviewKeepsLaunchOrphanNote(t *testing.T) {
	srv, db, dir := newTestServer(t)
	_, jobs := seedRepoWithJobs(t, db, filepath.Join(dir, "read-repo"), 1, "read")
	job := jobs[0]
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', branch = 'main' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "prompt", "kept"))
	insertReadLaunch(t, db, job.ID, "api-review-tok")

	rr := serveHuma(t, srv, http.MethodGet, "/api/review?job_id="+url.QueryEscape(stringID(job.ID)), nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "orphan risk", "api review dropped the orphan warning")
}

func TestAPIReviewSurfacesOrphanLookupFault(t *testing.T) {
	srv, db, dir := newTestServer(t)
	_, jobs := seedRepoWithJobs(t, db, filepath.Join(dir, "fault-repo"), 1, "fault")
	job := jobs[0]
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "prompt", "kept"))
	_, err = db.Exec(`ALTER TABLE review_job_cloud_launches RENAME TO review_job_cloud_launches_src`)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE VIEW review_job_cloud_launches AS
		SELECT id, job_id, launch_token, agent_id, cleaned, repo_identity, created_at
		FROM review_job_cloud_launches_src`)
	require.NoError(t, err)

	rr := serveHuma(t, srv, http.MethodGet, "/api/review?job_id="+url.QueryEscape(stringID(job.ID)), nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code, "api review returned success on orphan lookup fault: %s", rr.Body.String())
	assert.Contains(t, rr.Body.String(), "orphan_note", "api review returned success on orphan lookup fault")
}

func TestReviewProjectionKeepsLaunchOrphanNote(t *testing.T) {
	srv, db, dir := newTestServer(t)
	_, jobs := seedRepoWithJobs(t, db, filepath.Join(dir, "proj-repo"), 1, "proj")
	job := jobs[0]
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', branch = 'main' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "prompt", "kept"))
	insertReadLaunch(t, db, job.ID, "proj-tok")

	rr := serveHuma(t, srv, http.MethodGet, "/api/ui/review-projection?job_id="+url.QueryEscape(stringID(job.ID)), nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var projection ReviewProjection
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &projection))
	assert.Contains(t, projection.Job.OrphanWarning, "orphan risk", "web projection dropped the orphan warning")
	assert.Contains(t, projection.Job.Error, "orphan risk", "web projection dropped the orphan warning")
}

func TestReviewProjectionContextualKeepsLaunchOrphanNote(t *testing.T) {
	srv, db, dir := newTestServer(t)
	repo, jobs := seedRepoWithJobs(t, db, filepath.Join(dir, "ctx-repo"), 1, "ctx")
	job := jobs[0]
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', branch = 'feature' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	insertReadLaunch(t, db, job.ID, "ctx-tok")

	rr := serveHuma(t, srv, http.MethodGet,
		"/api/ui/review-projection?repo="+url.QueryEscape(repo.RootPath)+"&branch=feature", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var projection ReviewProjection
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &projection))
	assert.Equal(t, job.ID, projection.Job.ID)
	assert.Contains(t, projection.Job.OrphanWarning, "orphan risk", "contextual projection dropped the orphan warning")
	assert.Contains(t, projection.Job.Error, "orphan risk", "contextual projection dropped the orphan warning")
}

func TestReviewProjectionPanelKeepsLaunchOrphanNote(t *testing.T) {
	srv, db, dir := newTestServer(t)
	_, jobs := seedRepoWithJobs(t, db, filepath.Join(dir, "panel-repo"), 2, "panel")
	parent, member := jobs[0], jobs[1]
	const run = "11111111-1111-4111-8111-111111111111"
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', branch = 'main', panel_run_uuid = ?, panel_role = 'synthesis' WHERE id = ?`, run, parent.ID)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET panel_run_uuid = ?, panel_role = 'member', panel_member_name = 'reader', panel_member_index = 0 WHERE id = ?`, run, member.ID)
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(db, parent.ID, "test", "prompt", "kept"))
	insertReadLaunch(t, db, member.ID, "panel-tok")

	rr := serveHuma(t, srv, http.MethodGet, "/api/ui/review-projection?job_id="+url.QueryEscape(stringID(parent.ID)), nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var projection ReviewProjection
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &projection))
	require.Len(t, projection.PanelMembers, 1)
	assert.Equal(t, member.ID, projection.PanelMembers[0].ID)
	assert.Contains(t, projection.PanelMembers[0].OrphanWarning, "orphan risk", "panel projection dropped the orphan warning")
	assert.Contains(t, projection.PanelMembers[0].Error, "orphan risk", "panel projection dropped the orphan warning")
}

func TestBrowserProjectionKeepsLaunchOrphanNote(t *testing.T) {
	job := storage.ReviewJob{
		Status:        storage.JobStatusDone,
		Error:         "cursor-cloud orphan risk: keep-this-token",
		OrphanWarning: "cursor-cloud orphan risk: keep-this-token",
	}
	projected := projectBrowserReviewJob(job)
	assert.Contains(t, projected.OrphanWarning, "orphan risk", "browser projection dropped the orphan warning")
	assert.Contains(t, projected.Error, "orphan risk", "browser projection dropped the orphan warning")
}

func insertReadLaunch(t *testing.T, db *storage.DB, jobID int64, token string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, ?, ?)`,
		jobID, token, storage.CloudLaunchPendingNote)
	require.NoError(t, err)
}
