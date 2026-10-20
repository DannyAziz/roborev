package agent

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
)

// TestCursorCloudDelete5xxThenSucceeds is the in-process delete retry.
// The first DELETE is a 500 with Retry-After. The next one removes the VM.
// Reverting the retry leaves the 500 in place, or waits the old 100ms
// backoff instead of Retry-After.
func TestCursorCloudDelete5xxThenSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sha = "0123456789abcdef0123456789abcdef01234567"
		const repo = "https://example.com/acme/repo.git"
		const gap = 3 * time.Second
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
		script := &delete5xxThenOK{Fake: fake, gap: gap}
		text, err := (&CursorCloudAgent{Client: script, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", text)
		script.mu.Lock()
		at := append([]time.Time(nil), script.at...)
		script.mu.Unlock()
		require.GreaterOrEqual(t, len(at), 2)
		assert.Equal(t, gap, at[1].Sub(at[0]))
		listed, listErr := fake.ListAgents(context.Background())
		require.NoError(t, listErr)
		assert.Empty(t, listed)
	})
}

type delete5xxThenOK struct {
	*cursorcloudtest.Fake
	mu  sync.Mutex
	gap time.Duration
	at  []time.Time
}

func (c *delete5xxThenOK) DeleteAgent(ctx context.Context, id string) error {
	c.mu.Lock()
	c.at = append(c.at, time.Now())
	n := len(c.at)
	gap := c.gap
	c.mu.Unlock()
	if n == 1 {
		return &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusInternalServerError, Message: "unavailable",
			HasRetryAfter: true, RetryAfter: gap,
		}
	}
	return c.Fake.DeleteAgent(ctx, id)
}

// TestCursorCloudListTimeoutThenSucceeds retries a list deadline and then
// returns the agents. A single attempt leaves the deadline as the result.
func TestCursorCloudListTimeoutThenSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &timeoutThenList{Fake: &cursorcloudtest.Fake{}}
		agents, err := listCursorCloudAgents(context.Background(), client)
		require.NoError(t, err)
		require.Len(t, agents, 1)
		assert.Equal(t, "bc_timeout", agents[0].ID)
		assert.Equal(t, 2, client.calls)
	})
}

type timeoutThenList struct {
	*cursorcloudtest.Fake
	calls int
}

func (c *timeoutThenList) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	c.calls++
	if c.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	return []cursorcloud.Agent{{
		ID:               "bc_timeout",
		SourceRepository: "https://example.com/acme/repo.git",
	}}, nil
}

// TestCursorCloudRetryCancelStopsPromptly cancels during Retry-After.
// The wait ends without a second list or delete. A sleep that ignores
// the context is still blocked when the bubble is idle.
func TestCursorCloudRetryCancelStopsPromptly(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		assertRetryCancelStops(t, func(ctx context.Context, client *cancelAfter429) error {
			_, err := listCursorCloudAgents(ctx, client)
			return err
		})
	})
	t.Run("delete", func(t *testing.T) {
		assertRetryCancelStops(t, func(ctx context.Context, client *cancelAfter429) error {
			return deleteCursorCloudAgent(ctx, client, "bc_cancel", nil)
		})
	})
}

func assertRetryCancelStops(t *testing.T, run func(context.Context, *cancelAfter429) error) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		client := &cancelAfter429{Fake: &cursorcloudtest.Fake{}, started: make(chan struct{})}
		errCh := make(chan error, 1)
		go func() {
			errCh <- run(ctx, client)
		}()
		<-client.started
		cancel()
		synctest.Wait()
		var err error
		ready := false
		select {
		case err = <-errCh:
			ready = true
		default:
		}
		require.True(t, ready)
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, client.calls())
	})
}

type cancelAfter429 struct {
	*cursorcloudtest.Fake
	mu      sync.Mutex
	n       int
	once    sync.Once
	started chan struct{}
}

func (c *cancelAfter429) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *cancelAfter429) failOnce() error {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	c.once.Do(func() { close(c.started) })
	return &cursorcloud.APIError{
		Op: "cursor-cloud", StatusCode: http.StatusTooManyRequests, Message: "slow down",
		HasRetryAfter: true, RetryAfter: time.Hour,
	}
}

func (c *cancelAfter429) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	return nil, c.failOnce()
}

func (c *cancelAfter429) DeleteAgent(context.Context, string) error {
	return c.failOnce()
}
