package daemon

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/storage"
)

const cursorCloudLaunchSHA = "0123456789abcdef0123456789abcdef01234567"

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
