package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
)

// TestMarkLaunchCleanedAfterDelete cleans the row in-process once the
// marker list shows no VM left: with no duplicate, and after deleting one.
func TestMarkLaunchCleanedAfterDelete(t *testing.T) {
	const (
		repo  = "https://example.com/acme/repo.git"
		token = "dup-token"
		sha   = "0123456789abcdef0123456789abcdef01234567"
	)
	for _, duplicate := range []bool{false, true} {
		fake := &cursorcloudtest.Fake{}
		if duplicate {
			_, err := fake.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{
				Repo: repo, Ref: sha, Prompt: "review\n" + cursorCloudLaunchMarker + token + "\n",
			})
			require.NoError(t, err)
		}
		cleaned := 0
		cloud := &CursorCloudAgent{RepoURL: repo, MarkLaunchCleaned: func(string) error {
			cleaned++
			return nil
		}}
		require.NoError(t, cloud.markLaunchCleanedAfterDelete(fake, repo, token, token, nil))
		assert.Equal(t, 1, cleaned)
		agents, err := fake.ListAgents(context.Background())
		require.NoError(t, err)
		assert.Empty(t, agents)
	}
}
