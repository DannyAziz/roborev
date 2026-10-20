package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestProcessJobCursorCloudDoesNotRepost drives WorkerPool.processJob. A
// create 429, a deadline after the POST body was sent, and a wait-loop
// timeout must each leave exactly one POST /v0/agents and zero retries.
// The deadline still lists that launch and deletes it.
func TestProcessJobCursorCloudDoesNotRepost(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	for _, mode := range []string{"429", "deadline", "wait"} {
		t.Run(mode, func(t *testing.T) {
			var posts, lists, deletes atomic.Int32
			var mu sync.Mutex
			var prompt string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v0/agents":
					body, _ := io.ReadAll(r.Body)
					posts.Add(1)
					var payload struct {
						Prompt struct {
							Text string `json:"text"`
						} `json:"prompt"`
					}
					_ = json.Unmarshal(body, &payload)
					mu.Lock()
					prompt = payload.Prompt.Text
					mu.Unlock()
					switch mode {
					case "429":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, `{"message":"too many requests"}`)
					case "deadline":
						<-r.Context().Done()
					default:
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"bc_wait","status":"CREATING"}`)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/v0/agents":
					lists.Add(1)
					// A deadline whose create never returned an id is still
					// listed here so the first reconcile pass can delete it.
					// Other modes have nothing to clean up.
					mu.Lock()
					text := prompt
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if mode == "deadline" && text != "" {
						_ = json.NewEncoder(w).Encode(map[string]any{
							"agents": []map[string]any{{
								"id":     "bc_deadline",
								"status": "CREATING",
								"source": map[string]string{"repository": "https://example.com/acme/repo.git"},
							}},
						})
						return
					}
					_, _ = io.WriteString(w, `{"agents":[]}`)
				case r.Method == http.MethodGet && r.URL.Path == "/v0/agents/bc_wait":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"bc_wait","status":"RUNNING"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/v0/agents/bc_wait/conversation":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"messages":[{"type":"assistant_message","text":"review"}]}`)
				case r.Method == http.MethodPost && r.URL.Path == "/v0/agents/bc_wait/stop":
					_, _ = io.WriteString(w, `{"id":"bc_wait"}`)
				case r.Method == http.MethodDelete && r.URL.Path == "/v0/agents/bc_wait":
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/bc_deadline/conversation"):
					mu.Lock()
					text := prompt
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"messages": []map[string]string{{"type": "user_message", "text": text}},
					})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/bc_deadline/stop"):
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"bc_deadline"}`)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/bc_deadline"):
					deletes.Add(1)
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			timeout := time.Duration(0)
			if mode != "429" {
				timeout = 400 * time.Millisecond
			}
			prev, err := agent.Get(agent.CursorCloudHost)
			require.NoError(t, err)
			agent.Register(&agent.CursorCloudAgent{
				Client: cursorcloud.NewHTTPClient("cursor-test-secret",
					cursorcloud.WithBaseURL(srv.URL),
					cursorcloud.WithHTTPClient(&http.Client{}),
				),
				RepoURL:      "https://example.com/acme/repo.git",
				Timeout:      timeout,
				PollInterval: 20 * time.Millisecond,
			})
			t.Cleanup(func() { agent.Register(prev) })

			tc := newWorkerTestContext(t, 1)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			job.BackupAgent = "test"

			tc.Pool.processJob(testWorkerID, job)

			assert.Equal(t, int32(1), posts.Load())
			if mode == "deadline" {
				assert.GreaterOrEqual(t, lists.Load(), int32(1))
				assert.GreaterOrEqual(t, deletes.Load(), int32(1))
			}
			updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
			assert.Equal(t, "test", updated.Agent)
			assert.Equal(t, 0, updated.RetryCount)
			claimed, claimErr := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, claimErr)
			require.NotNil(t, claimed)
			assert.Equal(t, "test", claimed.Agent)
		})
	}
}

// TestSynthesisCursorCloudLaunchIsNotRetried pins the non-host synthesis
// path. Formatting that error with %v drops ErrCursorCloudNonRetryable, and
// the job is queued for another agent run.
func TestSynthesisCursorCloudLaunchIsNotRetried(t *testing.T) {
	noHostMarker(t)
	var calls atomic.Int32
	const synthAgent = "synth-cloud-once"
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: synthAgent,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			calls.Add(1)
			return "", fmt.Errorf("create agent: %w", agent.ErrCursorCloudNonRetryable)
		},
	})
	tc := newWorkerTestContext(t, 1)
	synth := claimSynthesis(t, tc, "synth-cloud-nr", synthAgent, "test")

	tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)

	updated := tc.assertJobStatus(t, synth.ID, storage.JobStatusQueued)
	assert.Equal(t, "test", updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
	assert.Equal(t, int32(1), calls.Load())
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, synth.ID, claimed.ID)
}

// TestFailOrRetryExecutionContextKeepsCursorCloudSentinel pins the check
// inside failOrRetryAgentExecutionContext. The processJob check is not on
// this path, so removing only that earlier check leaves this test green.
func TestFailOrRetryExecutionContextKeepsCursorCloudSentinel(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, "test")
	job.BackupAgent = "codex"

	tc.Pool.failOrRetryAgentExecutionContext(
		context.Background(), testWorkerID, job, job.Agent,
		fmt.Errorf("create agent: %w", agent.ErrCursorCloudNonRetryable),
	)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, "codex", updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, "codex", claimed.Agent)
}

// TestProcessJobCursorCloudDeadlineCheckStandsAlone pins the processJob
// check that runs before the deadline formatter. Removing only the
// execution-context check still fails the job here, because this error is
// both a deadline and the sentinel.
func TestProcessJobCursorCloudDeadlineCheckStandsAlone(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	agent.Register(&agent.FakeAgent{
		NameStr: agent.CursorCloudHost,
		ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
			return "", fmt.Errorf("wait: %w", errors.Join(context.DeadlineExceeded, agent.ErrCursorCloudNonRetryable))
		},
	})
	t.Cleanup(func() { agent.Register(prev) })

	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	job.BackupAgent = "test"

	tc.Pool.processJob(testWorkerID, job)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, "test", updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
	claimed, claimErr := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, claimErr)
	require.NotNil(t, claimed)
	assert.Equal(t, "test", claimed.Agent)
}

func TestCursorCloudPreLaunchValidationCanFailOver(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	job.BackupAgent = "test"

	tc.Pool.processJob(testWorkerID, job)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, "test", updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
}
