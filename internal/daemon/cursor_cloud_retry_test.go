package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
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
func TestProcessJobCursorCloudDoesNotRepost(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	for _, mode := range []string{"429", "deadline", "wait"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v0/agents":
					_, _ = io.Copy(io.Discard, r.Body)
					posts.Add(1)
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
					w.Header().Set("Content-Type", "application/json")
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
			updated := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
			assert.Equal(t, agent.CursorCloudHost, updated.Agent)
			assert.Equal(t, 0, updated.RetryCount)
			claimed, claimErr := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, claimErr)
			assert.Nil(t, claimed)
		})
	}
}
