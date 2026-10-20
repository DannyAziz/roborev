package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
)

// TestMarkLaunchCleanedAfterDeleteHonorsFound is the post-delete gate.
// The sweep finds and deletes a same-token VM after the listing-lag window.
// Cleaning because the window has passed, and ignoring found, fails this test.
func TestMarkLaunchCleanedAfterDeleteHonorsFound(t *testing.T) {
	start := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
	restore := SetCursorCloudClockForTest(func() time.Time {
		return start.Add(cursorCloudReconcileWindow)
	})
	t.Cleanup(restore)

	const (
		repo  = "https://example.com/acme/repo.git"
		token = "found-pin"
		sha   = "0123456789abcdef0123456789abcdef01234567"
	)
	fake := &cursorcloudtest.Fake{}
	_, err := fake.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{
		Repo:   repo,
		Ref:    sha,
		Prompt: "review\n" + cursorCloudLaunchMarker + token + "\n",
	})
	require.NoError(t, err)
	cleaned := 0
	cloud := &CursorCloudAgent{
		RepoURL:    repo,
		launchedAt: start,
		MarkLaunchCleaned: func(string) error {
			cleaned++
			return nil
		},
	}

	require.NoError(t, cloud.markLaunchCleanedAfterDelete(fake, repo, token, token, nil))
	assert.Equal(t, 0, cleaned)
	_, err = fake.GetAgent(context.Background(), "bc_fake_1")
	require.Error(t, err)
}
