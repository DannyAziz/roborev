package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/testutil"
)

func TestAgentHookTracksHistoricalRepoIdentity(t *testing.T) {
	db := testutil.OpenTestDB(t)
	repo := testutil.NewTestRepo(t)
	const historical = "https://github.com/User/Repo.git"
	repo.AddRemote("origin", historical)
	mainRoot, err := gitrepo.MainRoot(context.Background(), repo.Root)
	require.NoError(t, err)
	stored, err := db.GetOrCreateRepo(mainRoot, historical)
	require.NoError(t, err)
	require.Equal(t, historical, stored.Identity)

	resolved, ok := daemonAgentHookSource{db: db}.ResolveTrackedRepo(
		context.Background(), repo.Root, "main",
	)
	require.True(t, ok)
	assert.True(t, resolved.Tracked)
	assert.Equal(t, historical, resolved.Identity)
}
