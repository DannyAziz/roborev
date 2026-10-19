package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
)

func TestRunAgentReviewHostRoundTrip(t *testing.T) {
	dir := t.TempDir()
	host := &agent.HostAgent{Dir: dir, Timeout: 5 * time.Second, PollInterval: 5 * time.Millisecond}
	answer := `{"schema_version":2,"verdict":"fail","summary":"One bug.",
  "findings":[{"severity":"high","problem":"Nil map write.","fix":"Initialize the map.","location":"a.go:3"}]}`

	schemaCh := make(chan []byte, 1)
	go func() {
		for {
			matches, _ := filepath.Glob(filepath.Join(dir, "*", "instructions.md"))
			if len(matches) == 1 {
				jobDir := filepath.Dir(matches[0])
				schema, _ := os.ReadFile(filepath.Join(jobDir, "schema.json"))
				schemaCh <- schema
				tmp := filepath.Join(jobDir, "answer.md.tmp")
				_ = os.WriteFile(tmp, []byte(answer), 0o600)
				_ = os.Rename(tmp, filepath.Join(jobDir, "answer.md"))
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	var out strings.Builder
	got, err := RunAgentReview(context.Background(), host, t.TempDir(), "HEAD", "prompt", "", "", &out)
	require.NoError(t, err)
	require.NotNil(t, got.Structured)
	require.Len(t, got.Structured.Findings, 1)
	assert.Equal(t, "a.go:3", got.Structured.Findings[0].Location)
	assert.Equal(t, storage.VerdictFromPassed(false), got.Verdict)
	assert.Contains(t, out.String(), "Nil map write.")
	assert.JSONEq(t, string(CustomReviewSchema), string(<-schemaCh))
}

func TestRunAgentReviewHostTimeoutIsNonRetryable(t *testing.T) {
	host := &agent.HostAgent{Dir: t.TempDir(), Timeout: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond}
	_, err := RunAgentReview(context.Background(), host, t.TempDir(), "HEAD", "prompt", "", "", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host handoff timeout")
	assert.True(t, agent.IsUnavailable(err), "the worker fails over or fails without same-agent retries")
	assert.False(t, errors.Is(err, context.DeadlineExceeded), "the worker must not label it as the job timeout")
}
