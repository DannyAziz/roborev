package daemon

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestCursorCloudList429ThenReapsAndCleans is a reconcile list that answers
// 429 and then succeeds. The VM is deleted and the launch is cleaned. The
// gap between the lists is Retry-After.
func TestCursorCloudList429ThenReapsAndCleans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const gap = 2 * time.Second
		tc, _ := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		fake := &cursorcloudtest.Fake{}
		script := &list429Script{Fake: fake, retryAfter: gap}
		_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
		require.Error(t, err)

		script.mu.Lock()
		at := append([]time.Time(nil), script.at...)
		script.mu.Unlock()
		require.Len(t, at, 2)
		assert.Equal(t, gap, at[1].Sub(at[0]))
		deleted := cursorCloudCallIDs(fake, cursorcloudtest.MethodDelete)
		require.NotEmpty(t, deleted)
		assertLaunchCleaned(t, tc, job.ID, fake, deleted[0])
	})
}

// TestCursorCloudList429ExhaustedNotesOrphan keeps the launch uncleaned
// when every list is 429. The job error carries the orphan-risk note.
// The log is not the only copy. One list attempt fails the call-count check.
func TestCursorCloudList429ExhaustedNotesOrphan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
		var logs bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&logs)
		t.Cleanup(func() { log.SetOutput(prev) })

		tc, _ := cursorCloudRestartFixture(t)
		sha := testutil.GetHeadSHA(t, tc.TmpDir)
		job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
		fake := &cursorcloudtest.Fake{}
		script := &list429Script{Fake: fake, retryAfter: time.Second, always: true}
		prevAgent, err := agent.Get(agent.CursorCloudHost)
		require.NoError(t, err)
		cloud := &agent.CursorCloudAgent{Client: script, RepoURL: cursorCloudRestartRepo}
		cloud.SetLaunchLimitsForTest(4, 20)
		agent.Register(cloud)
		t.Cleanup(func() { agent.Register(prevAgent) })

		tc.Pool.processJob(testWorkerID, job)

		script.mu.Lock()
		lists := script.lists
		id := script.createdID
		script.mu.Unlock()
		assert.Equal(t, 3, lists)
		_, err = fake.GetAgent(t.Context(), id)
		require.NoError(t, err)
		n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		updated, err := tc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusFailed, updated.Status)
		assert.Contains(t, updated.Error, "orphan risk")
		assert.Contains(t, updated.Error, "pending cleanup")
		warning, err := tc.DB.LaunchOrphanWarning(job.ID)
		require.NoError(t, err)
		assert.Contains(t, warning, "orphan risk")
		assert.Contains(t, logs.String(), "orphan risk")
	})
}

type list429Script struct {
	*cursorcloudtest.Fake
	mu         sync.Mutex
	retryAfter time.Duration
	always     bool
	lists      int
	createdID  string
	at         []time.Time
}

func (c *list429Script) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	ag, err := c.Fake.CreateAgent(ctx, req)
	if err != nil {
		return ag, err
	}
	c.mu.Lock()
	c.createdID = ag.ID
	c.mu.Unlock()
	return ag, &cursorcloud.APIError{
		Op: "create agent", StatusCode: http.StatusTooManyRequests, Message: "too many requests",
	}
}

func (c *list429Script) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	c.mu.Lock()
	c.lists++
	n := c.lists
	c.at = append(c.at, time.Now())
	always := c.always
	wait := c.retryAfter
	c.mu.Unlock()
	if always || n == 1 {
		return nil, &cursorcloud.APIError{
			Op: "list agents", StatusCode: http.StatusTooManyRequests, Message: "slow down",
			HasRetryAfter: true, RetryAfter: wait,
		}
	}
	return c.Fake.ListAgents(ctx)
}
