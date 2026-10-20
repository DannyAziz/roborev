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
	// The row is the identity ResolveRepoIdentity stored at bb99a09a:
	// case and the .git suffix kept. It is not computed by the function
	// under test.
	const historical = "https://github.com/Acme/Widgets.git"
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

func TestAgentHookTracksMainFormIdentity(t *testing.T) {
	// These are the strings main's stripURLCredentials stores. They are
	// written literally, not computed by the function under test.
	for _, historical := range []string{
		"deploy@git.example.internal:org/repo.git",
		"https://host.example/org/repo@v2.git",
	} {
		t.Run(historical, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			repo := testutil.NewTestRepo(t)
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
		})
	}
}

func TestAgentHookTracksSSHAndSCPIdentities(t *testing.T) {
	cases := []struct{ remote, stored string }{
		{"ssh://git@host/org/repo", "ssh://host/org/repo"},
		{"deploy@host:org/repo.git", "deploy@host:org/repo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.remote, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			repo := testutil.NewTestRepo(t)
			repo.AddRemote("origin", tc.remote)
			mainRoot, err := gitrepo.MainRoot(context.Background(), repo.Root)
			require.NoError(t, err)
			_, err = db.GetOrCreateRepo(mainRoot, tc.stored)
			require.NoError(t, err)
			resolved, ok := daemonAgentHookSource{db: db}.ResolveTrackedRepo(context.Background(), repo.Root, "main")
			require.True(t, ok)
			assert.True(t, resolved.Tracked)
			assert.Equal(t, tc.stored, resolved.Identity)
		})
	}
}

func TestCanonicalIdentityStillTrackedThenScrubbed(t *testing.T) {
	db := testutil.OpenTestDB(t)
	repo := testutil.NewTestRepo(t)
	const stored = "https://github.com/Acme/Widgets.git"
	const canonical = "https://github.com/acme/widgets"
	repo.AddRemote("origin", stored)
	mainRoot, err := gitrepo.MainRoot(context.Background(), repo.Root)
	require.NoError(t, err)
	row, err := db.GetOrCreateRepo(mainRoot, canonical)
	require.NoError(t, err)
	require.NoError(t, db.SetRepoIdentity(row.ID, canonical))

	resolved, ok := daemonAgentHookSource{db: db}.ResolveTrackedRepo(context.Background(), repo.Root, "main")
	require.True(t, ok)
	assert.True(t, resolved.Tracked)

	n, err := db.ScrubCanonicalRepoIdentities()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	again, err := db.GetRepoByPath(mainRoot)
	require.NoError(t, err)
	assert.Equal(t, stored, again.Identity)

	other := testutil.NewTestRepo(t)
	const upper = "https://git.example.internal/Org/Repo"
	const lower = "https://git.example.internal/org/repo"
	other.AddRemote("origin", upper)
	otherRoot, err := gitrepo.MainRoot(context.Background(), other.Root)
	require.NoError(t, err)
	otherRow, err := db.GetOrCreateRepo(otherRoot, lower)
	require.NoError(t, err)
	require.NoError(t, db.SetRepoIdentity(otherRow.ID, lower))
	miss, ok := daemonAgentHookSource{db: db}.ResolveTrackedRepo(context.Background(), other.Root, "main")
	require.True(t, ok)
	assert.False(t, miss.Tracked)
	n, err = db.ScrubCanonicalRepoIdentities()
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	kept, err := db.GetRepoByPath(otherRoot)
	require.NoError(t, err)
	assert.Equal(t, lower, kept.Identity)
}
