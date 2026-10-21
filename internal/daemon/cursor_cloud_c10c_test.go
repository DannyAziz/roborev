package daemon

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestProcessJobBindsCursorCloudLaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "test-key")
	tc, fake := cursorCloudRestartFixture(t)
	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = `{"schema_version":2,"summary":"No issues found.","verdict":"pass","findings":[]}`
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)

	tc.Pool.processJob(testWorkerID, job)

	assert.Equal(t, 1, countCloudLaunchRows(t, tc.DB, job.ID))
}

func TestSynthesisBindsCursorCloudLaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "test-key")
	tc, fake := cursorCloudRestartFixture(t)
	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = `{"summary":"combined","findings":[]}`
	const memberAgent = "c10c-bind-synth-member"
	registerPassingAgent(t, memberAgent)
	runUUID, members, synth := enqueuePanelRun(t, tc, "c10c-bind-synth", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, agent.CursorCloudHost)
	completeMember(t, tc, members[0].ID, memberAgent, "Review #1\n\n## Review Findings\n\n- Severity: Medium\n- Location: a.go:1\n- Problem: A\n- Fix: Fix A")
	completeMember(t, tc, members[1].ID, memberAgent, "Review #2\n\n## Review Findings\n\n- Severity: Medium\n- Location: b.go:2\n- Problem: B\n- Fix: Fix B")
	claimed := releaseAndClaimSynthesis(t, tc, runUUID)

	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, claimed)

	assert.Equal(t, 1, countCloudLaunchRows(t, tc.DB, synth.ID))
}

func countCloudLaunchRows(t *testing.T, db *storage.DB, jobID int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM review_job_cloud_launches WHERE job_id = ?`, jobID).Scan(&n))
	return n
}

func TestSynthesisRunKeepsPriorLog(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	const memberAgent = "c10c-synth-log-member"
	registerPassingAgent(t, memberAgent)
	synthAgent := &synthesisEntrypointTestAgent{name: "c10c-synth-log"}
	agent.RegisterForTest(t, synthAgent)
	runUUID, members, synth := enqueuePanelRun(t, tc, "c10c-synth-log", []memberSpec{
		{name: "m0", agent: memberAgent},
		{name: "m1", agent: memberAgent},
	})
	setSynthesisAgent(t, tc, runUUID, synthAgent.name)
	completeMember(t, tc, members[0].ID, memberAgent, "Review #1\n\n## Review Findings\n\n- Severity: Medium\n- Location: a.go:1\n- Problem: A\n- Fix: Fix A")
	completeMember(t, tc, members[1].ID, memberAgent, "Review #2\n\n## Review Findings\n\n- Severity: Medium\n- Location: b.go:2\n- Problem: B\n- Fix: Fix B")
	require.NoError(t, os.MkdirAll(JobLogDir(), 0o700))
	const prior = "cursor-cloud: created agent bc_prior\n"
	require.NoError(t, os.WriteFile(JobLogPath(synth.ID), []byte(prior), 0o600))
	claimed := releaseAndClaimSynthesis(t, tc, runUUID)

	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, claimed)

	body, err := os.ReadFile(JobLogPath(synth.ID))
	require.NoError(t, err)
	assert.Contains(t, string(body), "bc_prior")
	assert.True(t, synthAgent.reviewCalled)
}

func TestDeleteRepoAPIRefusesUncleanedLaunch(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, "tok-api", cursorCloudRestartRepo))
	srv := &Server{db: tc.DB}
	input := &DeleteRepoInput{}
	input.Body.RepoID = tc.Repo.ID
	input.Body.Cascade = true

	_, err := srv.humaDeleteRepo(context.Background(), input)

	var status huma.StatusError
	require.ErrorAs(t, err, &status)
	assert.Equal(t, http.StatusConflict, status.GetStatus())
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestBatchCursorCloudLaunchIsRefusedWithKey(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "test-key")
	_, err := review.RunAgentReview(context.Background(), &agent.CursorCloudAgent{}, t.TempDir(),
		"0123456789abcdef0123456789abcdef01234567", "prompt", "", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launch binding")
}

func TestFixPlannerCursorCloudLaunchIsRefusedWithKey(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "test-key")
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	pb := prompt.NewBuilderWithConfig(nil, nil).ForRepo(tc.TmpDir, 0)
	_, _, err := tc.Pool.prepareFixPlan(context.Background(), 0, testWorkerID,
		&agent.CursorCloudAgent{}, job, tc.TmpDir, "plan the change", "implement", pb, tc.TmpDir, io.Discard, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launch binding")
}

func TestDaemonFreeSynthesisCursorCloudLaunchIsRefusedWithKey(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "test-key")
	tc := newWorkerTestContext(t, 1)
	_, err := review.RunSynthesisAgent(context.Background(), &agent.CursorCloudAgent{}, nil, "combine", "", nil, review.SynthesisHooks{
		Checkout: func() (review.SynthesisCheckout, error) {
			return review.SynthesisCheckout{RepoPath: tc.TmpDir, GitRef: "HEAD"}, nil
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launch binding")
}
