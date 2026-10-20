package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestProcessJobCursorCloudDoesNotRepost drives WorkerPool.processJob. A
// create 429, a deadline after the POST body was sent, and a wait-loop
// timeout must each leave exactly one POST /v0/agents and zero retries.
// The deadline still lists that launch and deletes it.
func TestProcessJobCursorCloudDoesNotRepost(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	for _, mode := range []string{"deadline", "wait"} {
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

			timeout := 400 * time.Millisecond
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

// TestCursorCloudDeleteForbiddenKeepsOrphanNoteThroughFailover is a 403
// delete. The backup still runs, and the job error keeps the orphan risk
// after failover and after that backup finishes.
func TestCursorCloudDeleteForbiddenKeepsOrphanNoteThroughFailover(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "test-key")
	tc, fake := cursorCloudRestartFixture(t)
	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = cursorCloudPassJSON
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodDelete: &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusForbidden, Message: "forbidden",
		},
	}
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	job.BackupAgent = "test"

	tc.Pool.processJob(testWorkerID, job)

	queued := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, "test", queued.Agent)
	assert.Contains(t, queued.Error, "orphan risk")
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)

	tc.Pool.processJob(testWorkerID, claimed)

	done := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	assert.Contains(t, done.Error, "orphan risk")
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

// A finished cloud review whose text is not a usable document must not
// launch again. One create for invalid text, and one for a document
// validateLiveDocument rejects.
func TestProcessJobCursorCloudDecodeDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	const repo = "https://example.com/acme/repo.git"
	for _, tc := range []struct {
		name  string
		final string
	}{
		{name: "invalid-text", final: `{"not_a_review":true}`},
		{name: "live-document", final: `{"schema_version":2,"summary":"blocked","verdict":"unable_to_review","findings":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: tc.final}
			prev, err := agent.Get(agent.CursorCloudHost)
			require.NoError(t, err)
			agent.Register(&agent.CursorCloudAgent{Client: fake, RepoURL: repo})
			t.Cleanup(func() { agent.Register(prev) })

			wtc := newWorkerTestContext(t, 1)
			sha := testutil.GetHeadSHA(t, wtc.TmpDir)
			job := wtc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			job.BackupAgent = "test"

			for range maxRetries + 2 {
				if job.Agent != agent.CursorCloudHost {
					break
				}
				wtc.Pool.processJob(testWorkerID, job)
				next, claimErr := wtc.DB.ClaimJob(testWorkerID)
				require.NoError(t, claimErr)
				if next == nil {
					break
				}
				job = next
			}
			assert.Equal(t, 1, createCountFromFake(fake))
			assert.NotEqual(t, agent.CursorCloudHost, job.Agent)
		})
	}
}

// A finished cloud review whose assistant text is not JSON must not launch
// again. Cleanup already deleted the VM before RunAgentReview decoded the
// text. Valid JSON of the wrong shape is TestProcessJobCursorCloudDecodeDoesNotRelaunch.
// This fails when the decode error is returned without sentCloudLaunch:
// RetryJob requeues cursor-cloud and the next claim POSTs a second agent.
func TestProcessJobCursorCloudMalformedJSONDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	const repo = "https://example.com/acme/repo.git"
	const malformed = `{"schema_version":2,`
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: malformed}
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	agent.Register(&agent.CursorCloudAgent{Client: fake, RepoURL: repo})
	t.Cleanup(func() { agent.Register(prev) })

	wtc := newWorkerTestContext(t, 1)
	// The pool default waits before a retry can be claimed. ClaimJob then
	// returns nil and this loop stops after one create.
	wtc.Pool.retryBackoff = 0
	sha := testutil.GetHeadSHA(t, wtc.TmpDir)
	job := wtc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	originalID := job.ID
	job.BackupAgent = "test"

	for range maxRetries + 2 {
		if job.Agent != agent.CursorCloudHost {
			break
		}
		wtc.Pool.processJob(testWorkerID, job)
		next, claimErr := wtc.DB.ClaimJob(testWorkerID)
		require.NoError(t, claimErr)
		if next == nil {
			break
		}
		job = next
	}
	assert.Equal(t, 1, createCountFromFake(fake))
	assert.NotEqual(t, agent.CursorCloudHost, job.Agent)
	stored, err := wtc.DB.GetJobByID(originalID)
	require.NoError(t, err)
	assert.Equal(t, 0, stored.RetryCount)
	assert.NotEqual(t, agent.CursorCloudHost, stored.Agent)
}

// A finished synthesis whose assistant text is not JSON must not launch again.
// decodeSynthesisResult marks that decode error sent, so RetryJob does not
// POST a second agent.
func TestProcessSynthesisCursorCloudMalformedJSONDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	const repo = "https://example.com/acme/repo.git"
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: `{"schema_version":2,`}
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	agent.Register(&agent.CursorCloudAgent{Client: fake, RepoURL: repo})
	t.Cleanup(func() { agent.Register(prev) })

	wtc := newWorkerTestContext(t, 1)
	wtc.Pool.retryBackoff = 0
	wtc.GitRepo.Run("remote", "add", "origin", repo)
	synth := claimSynthesis(t, wtc, "synth-malformed-json", agent.CursorCloudHost, "test")
	originalID := synth.ID

	for range maxRetries + 2 {
		if synth.Agent != agent.CursorCloudHost {
			break
		}
		wtc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
		next, claimErr := wtc.DB.ClaimJob(testWorkerID)
		require.NoError(t, claimErr)
		if next == nil {
			break
		}
		synth = next
	}
	assert.Equal(t, 1, createCountFromFake(fake))
	stored, err := wtc.DB.GetJobByID(originalID)
	require.NoError(t, err)
	assert.Equal(t, 0, stored.RetryCount)
	assert.NotEqual(t, agent.CursorCloudHost, stored.Agent)
}

// A no-verdict cloud review keeps the no-verdict prefix and output, and a
// restart does not launch another VM. The backup is cursor-cloud so a
// result that is not marked sent failsover into another CreateAgent.
func TestCursorCloudNoVerdictRestartDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	const repo = "https://example.com/acme/repo.git"
	const prose = "I am unable to read the diff file because it is ignored by configured ignore patterns. PIN-OUTPUT-7f3a"
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: prose}
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	agent.Register(&agent.CursorCloudAgent{Client: fake, RepoURL: repo})
	t.Cleanup(func() { agent.Register(prev) })

	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	originalID := job.ID
	job.BackupAgent = agent.CursorCloudHost
	var logs bytes.Buffer
	prevLog := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(prevLog)

	for range maxRetries + 2 {
		if job.Agent != agent.CursorCloudHost {
			break
		}
		tc.Pool.processJob(testWorkerID, job)
		next, claimErr := tc.DB.ClaimJob(testWorkerID)
		require.NoError(t, claimErr)
		if next == nil {
			break
		}
		job = next
	}
	assert.Equal(t, 1, createCountFromFake(fake))
	stored, err := tc.DB.GetJobByID(originalID)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(stored.Error, review.NoVerdictErrorPrefix), stored.Error)
	assert.Contains(t, stored.Error, "PIN-OUTPUT-7f3a")
	assert.Contains(t, logs.String(), "cursor-cloud launch will not be retried")

	const runUUID = "panel-run-no-verdict"
	const ghRepo = "acme/widgets"
	const head = "head-no-verdict"
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET panel_run_uuid = ?, panel_role = 'member', status = 'failed' WHERE id = ?`, runUUID, originalID)
	require.NoError(t, err)
	_, err = tc.DB.Exec(`INSERT INTO ci_pr_panels (github_repo, pr_number, head_sha, panel_run_uuid, outcome) VALUES (?, ?, ?, ?, ?)`,
		ghRepo, 7, head, runUUID, storage.PanelOutcomeNoReviewPosted)
	require.NoError(t, err)
	_, err = tc.DB.Exec(`INSERT INTO ci_pr_review_attempts (
		github_repo, pr_number, head_sha, attempt, first_attempt_at, last_error_class, state, updated_at
	) VALUES (?, ?, ?, 1, ?, 'genuine', 'done', ?)`, ghRepo, 7, head, now, now)
	require.NoError(t, err)
	rearmed, err := tc.DB.RearmFailedReviewAttempts(time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(0), rearmed)
	next, claimErr := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, claimErr)
	if next != nil && next.Agent == agent.CursorCloudHost {
		tc.Pool.processJob(testWorkerID, next)
	}
	assert.Equal(t, 1, createCountFromFake(fake))
}

func createCountFromFake(fake *cursorcloudtest.Fake) int {
	n := 0
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodCreate {
			n++
		}
	}
	return n
}

func TestProcessJobCursorCloudCreate429SkipsReconcile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
		const repo = "https://example.com/acme/repo.git"
		fake := &cursorcloudtest.Fake{
			MethodErrors: map[string]error{
				cursorcloudtest.MethodCreate: &cursorcloud.APIError{Op: "create agent", StatusCode: http.StatusTooManyRequests, Message: "slow down"},
			},
		}
		prev, err := agent.Get(agent.CursorCloudHost)
		require.NoError(t, err)
		agent.Register(&agent.CursorCloudAgent{Client: fake, RepoURL: repo})
		t.Cleanup(func() { agent.Register(prev) })

		tc := newWorkerTestContext(t, 1)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		job.BackupAgent = "test"
		tc.Pool.processJob(testWorkerID, job)

		assert.Equal(t, 1, createCountFromFake(fake))
		// The 429 was sent, so reconcile lists. The job still failovers
		// without a local retry.
		assert.Positive(t, methodCountFromFake(fake, cursorcloudtest.MethodList))
		updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
		assert.Equal(t, "test", updated.Agent)
		assert.Equal(t, 0, updated.RetryCount)
	})
}

func methodCountFromFake(fake *cursorcloudtest.Fake, method string) int {
	n := 0
	for _, call := range fake.Calls() {
		if call.Method == method {
			n++
		}
	}
	return n
}

// TestCursorCloudBadRefFailoversWithoutRetry is a ref that is not one commit.
// Client setup and the repo URL already succeeded, so this is the commit
// check. It fails over once and does not retry cursor-cloud.
func TestCursorCloudBadRefFailoversWithoutRetry(t *testing.T) {
	cloud := &agent.CursorCloudAgent{
		Client:  &cursorcloudtest.Fake{},
		RepoURL: "https://example.com/acme/repo.git",
	}
	_, err := cloud.Review(context.Background(), t.TempDir(), "dirty", "prompt", nil)
	require.Error(t, err)
	require.True(t, agent.IsUnavailable(err))
	require.NotErrorIs(t, err, agent.ErrCursorCloudNonRetryable)

	tc := newWorkerTestContext(t, 1)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	job.BackupAgent = "test"
	tc.Pool.failOrRetryAgentExecutionContext(context.Background(), testWorkerID, job, job.Agent, err)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, "test", updated.Agent)
	assert.Equal(t, 0, updated.RetryCount)
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
