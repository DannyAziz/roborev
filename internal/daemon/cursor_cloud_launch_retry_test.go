package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/storage"
)

const cursorCloudLaunchSHA = "0123456789abcdef0123456789abcdef01234567"

func TestCursorCloudLaunchHTTPErrorDoesNotRetryJob(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "5xx", status: http.StatusBadGateway, body: `{"message":"bad gateway"}`},
		{name: "429", status: http.StatusTooManyRequests, body: `{"message":"too many requests"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
					posts.Add(1)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)

			err := reviewCursorCloud(t, srv.URL, nil)
			require.Error(t, err)
			assert.Equal(t, int32(1), posts.Load())
			assertLaunchNotRetried(t, err)
		})
	}
}

func TestCursorCloudLaunchAmbiguousCreateDoesNotRetryJob(t *testing.T) {
	t.Run("error after send", func(t *testing.T) {
		var posts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
				posts.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":`)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		err := reviewCursorCloud(t, srv.URL, nil)
		require.Error(t, err)
		assert.Equal(t, int32(1), posts.Load())
		assertLaunchNotRetried(t, err)
	})

	t.Run("missing id", func(t *testing.T) {
		var posts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
				posts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"CREATING"}`)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		err := reviewCursorCloud(t, srv.URL, nil)
		require.Error(t, err)
		assert.Equal(t, int32(1), posts.Load())
		assertLaunchNotRetried(t, err)
	})

	t.Run("timeout after send", func(t *testing.T) {
		var posts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
				posts.Add(1)
				// The POST has been read. Hold the response longer than the
				// client timeout, then return so the server can shut down.
				time.Sleep(300 * time.Millisecond)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"id":"bc_late","status":"CREATING"}`)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		// Wall-clock wait: the injected HTTP client times out after the
		// request has been written. synctest cannot see that socket.
		hc := &http.Client{Timeout: 50 * time.Millisecond}
		err := reviewCursorCloud(t, srv.URL, hc)
		require.Error(t, err)
		assert.Equal(t, int32(1), posts.Load())
		assertLaunchNotRetried(t, err)
	})
}

func TestCursorCloudCapRefusalDoesNotRetryJob(t *testing.T) {
	var posts atomic.Int32
	hold := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
			posts.Add(1)
			entered <- struct{}{}
			<-hold
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"bc_held","name":"n","status":"CREATING"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() {
		release()
		srv.Close()
	})

	cloud := &agent.CursorCloudAgent{
		Client:  cursorcloud.NewHTTPClient("cursor-test-secret", cursorcloud.WithBaseURL(srv.URL)),
		RepoURL: "https://github.com/acme/widgets.git",
	}
	cloud.SetLaunchLimitsForTest(1, 100)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cloud.Review(context.Background(), t.TempDir(), cursorCloudLaunchSHA, "review the change", nil)
	}()
	<-entered
	held := posts.Load()

	err := reviewOn(t, cloud)
	require.Error(t, err)
	assert.Equal(t, held, posts.Load(), "a refused launch must not POST")
	// The cap refused before CreateAgent, so this is not a sent launch.
	require.NotErrorIs(t, err, agent.ErrCursorCloudNonRetryable)
	require.True(t, agent.IsUnavailable(err))

	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "cursor-cloud-cap", testWorkerID, agent.CursorCloudHost)
	job.RepoPath = tc.TmpDir
	tc.Pool.finishAgentError(context.Background(), testWorkerID, job, agent.CursorCloudHost, err, time.Minute)
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, agent.CursorCloudHost, updated.Agent)
	retryCount, retryErr := tc.DB.GetJobRetryCount(job.ID)
	require.NoError(t, retryErr)
	assert.Zero(t, retryCount)
	release()
	<-done
}

func reviewOn(t *testing.T, cloud *agent.CursorCloudAgent) error {
	t.Helper()
	_, err := cloud.Review(context.Background(), t.TempDir(), cursorCloudLaunchSHA, "review the change", nil)
	return err
}

func TestCursorCloudDeadlineExceededDoesNotRetryJob(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
			posts.Add(1)
			// The POST has been read. Hold the response longer than the
			// client timeout, then return so the server can shut down.
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"bc_late","status":"CREATING"}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	// Wall-clock wait: the injected HTTP client times out after the request
	// has been written. synctest cannot see that socket.
	err := reviewCursorCloud(t, srv.URL, &http.Client{Timeout: 50 * time.Millisecond})
	require.Error(t, err)
	assert.Equal(t, int32(1), posts.Load())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, agent.ErrCursorCloudNonRetryable)

	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "cursor-cloud-deadline", testWorkerID, agent.CursorCloudHost)
	job.RepoPath = tc.TmpDir
	tc.Pool.finishAgentError(context.Background(), testWorkerID, job, agent.CursorCloudHost, err, time.Minute)
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, "cursor-cloud: launch failed")
	retryCount, retryErr := tc.DB.GetJobRetryCount(job.ID)
	require.NoError(t, retryErr)
	assert.Zero(t, retryCount)
}

func TestDeadlineExceededStillRetriesOtherAgents(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "other-deadline", testWorkerID, "test")
	job.RepoPath = tc.TmpDir
	tc.Pool.finishAgentError(context.Background(), testWorkerID, job, "test", context.DeadlineExceeded, time.Minute)
	tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	retryCount, err := tc.DB.GetJobRetryCount(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, retryCount)
}

func TestCursorCloudSentLaunchMatchesWrappedSentinel(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	claimed := tc.createAndClaimJobWithAgent(t, "cursor-cloud-hidden", testWorkerID, agent.CursorCloudHost)
	claimed.RepoPath = tc.TmpDir

	hidden := hiddenCursorCloudErr{error: agent.ErrCursorCloudNonRetryable}
	require.ErrorIs(t, hidden, agent.ErrCursorCloudNonRetryable)
	assert.NotContains(t, hidden.Error(), agent.ErrCursorCloudNonRetryable.Error())
	tc.Pool.finishAgentError(context.Background(), testWorkerID, claimed, agent.CursorCloudHost, hidden, time.Minute)
	failed := tc.assertJobStatus(t, claimed.ID, storage.JobStatusFailed)
	assert.Equal(t, agent.CursorCloudHost, failed.Agent)
	retryCount, retryErr := tc.DB.GetJobRetryCount(claimed.ID)
	require.NoError(t, retryErr)
	assert.Zero(t, retryCount)

	textual := errors.New(agent.ErrCursorCloudNonRetryable.Error() + ": status 500")
	require.NotErrorIs(t, textual, agent.ErrCursorCloudNonRetryable)
	other := tc.createAndClaimJobWithAgent(t, "cursor-cloud-text", testWorkerID, agent.CursorCloudHost)
	other.RepoPath = tc.TmpDir
	tc.Pool.finishAgentError(context.Background(), testWorkerID, other, agent.CursorCloudHost, textual, time.Minute)
	retried := tc.assertJobStatus(t, other.ID, storage.JobStatusQueued)
	assert.Equal(t, agent.CursorCloudHost, retried.Agent)
	textRetries, textErr := tc.DB.GetJobRetryCount(other.ID)
	require.NoError(t, textErr)
	assert.Equal(t, 1, textRetries)
}

// hiddenCursorCloudErr unwraps to the launch sentinel without putting that
// sentinel's text in Error. Matching it requires errors.Is.
type hiddenCursorCloudErr struct{ error }

func (e hiddenCursorCloudErr) Error() string { return "provider refused" }

func (e hiddenCursorCloudErr) Unwrap() error { return e.error }

func TestCursorCloudLaunchDialErrorMayRetry(t *testing.T) {
	err := reviewCursorCloud(t, "http://127.0.0.1:1", nil)
	require.Error(t, err)
	assert.False(t, agent.IsUnavailable(err))

	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "cursor-cloud-dial", testWorkerID, agent.CursorCloudHost)
	job.RepoPath = tc.TmpDir
	tc.Pool.failOrRetryAgentExecutionContext(context.Background(), testWorkerID, job, agent.CursorCloudHost, err)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, agent.CursorCloudHost, updated.Agent)
	retryCount, retryErr := tc.DB.GetJobRetryCount(job.ID)
	require.NoError(t, retryErr)
	assert.Equal(t, 1, retryCount)
}

func reviewCursorCloud(t *testing.T, base string, hc *http.Client) error {
	t.Helper()
	opts := []cursorcloud.Option{cursorcloud.WithBaseURL(base)}
	if hc != nil {
		opts = append(opts, cursorcloud.WithHTTPClient(hc))
	}
	cloud := &agent.CursorCloudAgent{
		Client:  cursorcloud.NewHTTPClient("cursor-test-secret", opts...),
		RepoURL: "https://github.com/acme/widgets.git",
	}
	_, err := cloud.Review(context.Background(), t.TempDir(), cursorCloudLaunchSHA, "review the change", nil)
	return err
}

func assertLaunchNotRetried(t *testing.T, launchErr error) {
	t.Helper()
	require.ErrorIs(t, launchErr, agent.ErrCursorCloudNonRetryable)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJobWithAgent(t, "cursor-cloud-launch", testWorkerID, agent.CursorCloudHost)
	job.RepoPath = tc.TmpDir
	tc.Pool.failOrRetryAgentExecutionContext(
		context.Background(), testWorkerID, job, agent.CursorCloudHost, launchErr,
	)
	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Contains(t, updated.Error, "cursor-cloud: launch failed")
	retryCount, err := tc.DB.GetJobRetryCount(job.ID)
	require.NoError(t, err)
	assert.Zero(t, retryCount)
}
