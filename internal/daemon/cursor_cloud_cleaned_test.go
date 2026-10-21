package daemon

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const cursorCloudPassJSON = `{"schema_version":2,"summary":"No issues found.","verdict":"pass","findings":[]}`

// cloudCreateScript creates the requested agent, optional duplicates that
// carry the same prompt, then returns createErr. failDelete is the 1-based
// DeleteAgent call that refuses; 4xx other than 404 is not retried.
type cloudCreateScript struct {
	*cursorcloudtest.Fake
	createErr  error
	extra      int
	failDelete int
	deletes    int
}

func (c *cloudCreateScript) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	ag, err := c.Fake.CreateAgent(ctx, req)
	if err != nil {
		return ag, err
	}
	for range c.extra {
		if _, err := c.Fake.CreateAgent(ctx, req); err != nil {
			return ag, err
		}
	}
	if c.createErr != nil {
		return ag, c.createErr
	}
	return ag, nil
}

func (c *cloudCreateScript) DeleteAgent(ctx context.Context, id string) error {
	c.deletes++
	if c.failDelete > 0 && c.deletes == c.failDelete {
		return &cursorcloud.APIError{Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "refused"}
	}
	return c.Fake.DeleteAgent(ctx, id)
}

func newBoundCloud(t *testing.T, tc *workerTestContext, job *storage.ReviewJob, client cursorcloud.Client) agent.Agent {
	t.Helper()
	cloud := &agent.CursorCloudAgent{Client: client, RepoURL: cursorCloudRestartRepo}
	cloud.SetLaunchLimitsForTest(4, 20)
	return tc.Pool.bindCursorCloudLaunch(testWorkerID, job, cloud)
}

func assertLaunchCleaned(t *testing.T, tc *workerTestContext, jobID int64, fake *cursorcloudtest.Fake, goneID string) {
	t.Helper()
	n, err := tc.DB.CountUncleanedCloudLaunches(jobID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	_, err = fake.GetAgent(t.Context(), goneID)
	require.Error(t, err)
}

// TestCursorCloud429ReapMarksCleaned: a 429 whose reconcile finds and
// deletes the VM cleans the launch in-process.
func TestCursorCloud429ReapMarksCleaned(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{}
	script := &cloudCreateScript{
		Fake:      fake,
		createErr: &cursorcloud.APIError{Op: "create agent", StatusCode: http.StatusTooManyRequests, Message: "too many requests"},
	}
	_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	deleted := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
	require.NotEmpty(t, deleted)
	assertLaunchCleaned(t, tc, job.ID, fake, deleted[0])
}

// TestCursorCloudInFlightCreateReapMarksCleaned: the POST was sent, the
// response never arrived, and the VM is already listed. Reconcile deletes it
// and cleans the launch, whether the review context is live, canceled, or
// past its deadline.
func TestCursorCloudInFlightCreateReapMarksCleaned(t *testing.T) {
	t.Run("client deadline", func(t *testing.T) {
		assertInFlightCreateReaped(t, context.Background(), &cloudCreateScript{
			Fake:      &cursorcloudtest.Fake{},
			createErr: context.DeadlineExceeded,
		}, context.DeadlineExceeded)
	})
	t.Run("canceled ctx", func(t *testing.T) {
		ctx := newFinishCtx(context.Canceled)
		script := &finishDuringCreate{finish: ctx.finish}
		script.Fake = &cursorcloudtest.Fake{}
		assertInFlightCreateReaped(t, ctx, script, context.Canceled)
	})
	t.Run("expired deadline", func(t *testing.T) {
		ctx := newFinishCtx(context.DeadlineExceeded)
		script := &finishDuringCreate{finish: ctx.finish}
		script.Fake = &cursorcloudtest.Fake{}
		assertInFlightCreateReaped(t, ctx, script, context.DeadlineExceeded)
	})
	t.Run("500", func(t *testing.T) {
		apiErr := &cursorcloud.APIError{
			Op: "create agent", StatusCode: http.StatusInternalServerError, Message: "boom",
		}
		assertInFlightCreateReaped(t, context.Background(), &cloudCreateScript{
			Fake:      &cursorcloudtest.Fake{},
			createErr: apiErr,
		}, apiErr)
	})
}

func assertInFlightCreateReaped(t *testing.T, ctx context.Context, client cursorcloud.Client, want error) {
	t.Helper()
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	_, err := newBoundCloud(t, tc, job, client).Review(ctx, tc.TmpDir, sha, "prompt", nil)
	require.ErrorIs(t, err, want)
	fake := clientFake(client)
	deleted := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
	require.NotEmpty(t, deleted)
	assertLaunchCleaned(t, tc, job.ID, fake, deleted[0])
}

func clientFake(client cursorcloud.Client) *cursorcloudtest.Fake {
	switch c := client.(type) {
	case *cloudCreateScript:
		return c.Fake
	case *finishDuringCreate:
		return c.Fake
	default:
		return nil
	}
}

// finishCtx stays live until finish runs, then Err is the supplied context
// error. Deadline is reported so Review keeps this context instead of wrapping
// a fresh timeout. The pre-create check still sees a live context.
type finishCtx struct {
	done chan struct{}
	err  error
	once sync.Once
}

func newFinishCtx(err error) *finishCtx {
	return &finishCtx{done: make(chan struct{}), err: err}
}

func (f *finishCtx) finish() { f.once.Do(func() { close(f.done) }) }

func (f *finishCtx) Deadline() (time.Time, bool) {
	if errors.Is(f.err, context.DeadlineExceeded) {
		return time.Unix(0, 0), true
	}
	return time.Now().Add(time.Hour), true
}

func (f *finishCtx) Done() <-chan struct{} { return f.done }

func (f *finishCtx) Err() error {
	select {
	case <-f.done:
		return f.err
	default:
		return nil
	}
}

func (f *finishCtx) Value(any) any { return nil }

// finishDuringCreate stores the agent, then finishes the review context and
// returns that context's error. The VM is already listed when Review sees
// the cancel or the expired deadline.
type finishDuringCreate struct {
	cloudCreateScript
	finish func()
}

func (c *finishDuringCreate) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	ag, err := c.Fake.CreateAgent(context.Background(), req)
	if err != nil {
		return ag, err
	}
	if c.finish != nil {
		c.finish()
	}
	return ag, ctx.Err()
}

// TestCursorCloudSuccessDeleteMarksCleaned is the in-process success path.
// Nil and 404 both confirm the delete, and the marker list is empty, so the
// launch is cleaned without a restart.
func TestCursorCloudSuccessDeleteMarksCleaned(t *testing.T) {
	for _, mode := range []string{"nil", "404"} {
		t.Run(mode, func(t *testing.T) {
			tc, _ := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
			if mode == "404" {
				fake.MethodErrors = map[string]error{
					cursorcloudtest.MethodDelete: &cursorcloud.APIError{
						Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
					},
				}
			}
			text, err := newBoundCloud(t, tc, job, fake).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
			require.NoError(t, err)
			assert.Equal(t, "ok", text)
			n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
			require.NoError(t, err)
			assert.Equal(t, 0, n)
			if mode == "nil" {
				deleted := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
				require.NotEmpty(t, deleted)
				_, err = fake.GetAgent(t.Context(), deleted[0])
				require.Error(t, err)
			}
		})
	}
}

// TestCursorCloudDuplicateCreateBeforeCleaned is the provider-side duplicate.
// Both VMs must be deleted with nil or 404 before the launch is cleaned.
// A refused second delete leaves the row uncleaned and that VM in place.
func TestCursorCloudDuplicateCreateBeforeCleaned(t *testing.T) {
	t.Run("both deleted", func(t *testing.T) {
		tc, _ := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
		script := &cloudCreateScript{Fake: fake, extra: 1}
		text, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", text)
		for _, id := range []string{"bc_fake_1", "bc_fake_2"} {
			_, getErr := fake.GetAgent(t.Context(), id)
			require.Error(t, getErr, id)
		}
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})
	t.Run("second delete refused", func(t *testing.T) {
		tc, _ := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
		script := &cloudCreateScript{Fake: fake, extra: 1, failDelete: 2}
		text, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", text)
		_, err = fake.GetAgent(t.Context(), "bc_fake_1")
		require.Error(t, err)
		got, err := fake.GetAgent(t.Context(), "bc_fake_2")
		require.NoError(t, err)
		assert.Equal(t, "bc_fake_2", got.ID)
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})
}

// TestUpdateInterruptDiscardsSuccessfulResult pins worker.go's post-success
// update check. The discard log is emitted only on that return. The review
// row stays absent.
func TestUpdateInterruptDiscardsSuccessfulResult(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	const agentName = "update-discard-result"
	var jobID int64
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: agentName,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			tc.Pool.InterruptJobsForUpdate([]int64{jobID})
			return cursorCloudPassJSON, nil
		},
	})
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agentName)
	jobID = job.ID
	logs := cursorCloudCaptureLog(t)

	tc.Pool.processJob(testWorkerID, job)

	assert.Contains(t, logs.String(), "discarded successful result for update-interrupted job")
	stored := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, 0, stored.RetryCount)
	_, err := tc.DB.GetReviewByJobID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

// TestUpdateInterruptDiscardsSynthesisResult pins synthesis_worker.go's
// post-success update check. Removing that return hands the document back
// and leaves the attempt running.
func TestUpdateInterruptDiscardsSynthesisResult(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	const synthAgent = "synth-discard-result"
	var jobID int64
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: synthAgent,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			tc.Pool.InterruptJobsForUpdate([]int64{jobID})
			return cursorCloudPassJSON, nil
		},
	})
	_, _, synth := enqueuePanelRun(t, tc, "discard-synth", []memberSpec{{name: "m0", agent: "test"}})
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	_, err := tc.DB.Exec(
		`UPDATE review_jobs SET status = 'running', worker_id = ?, agent = ?, source = '', git_ref = ? WHERE id = ?`,
		testWorkerID, synthAgent, sha, synth.ID,
	)
	require.NoError(t, err)
	job, err := tc.DB.GetJobByID(synth.ID)
	require.NoError(t, err)
	jobID = job.ID

	doc, _, _, runErr := tc.Pool.runSynthesisAgent(context.Background(), testWorkerID, job, nil, "prompt")

	require.ErrorIs(t, runErr, errSynthesisCanceled)
	assert.Empty(t, doc.Summary)
	stored := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, 0, stored.RetryCount)
	_, err = tc.DB.GetReviewByJobID(job.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}
