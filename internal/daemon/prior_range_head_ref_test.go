package daemon

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	promptpkg "go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestCursorCloudHeadRefInlinesPriorRangeDoc(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	base := testutil.GetHeadSHA(t, tc.TmpDir)
	first := tc.GitRepo.CommitFile("first.go", "package first\n", "first")
	prior, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, GitRef: base + ".." + first, Agent: "test", JobType: storage.JobTypeRange,
	})
	require.NoError(t, err)
	_, err = tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB, prior.ID, "test", "old prompt", "earlier range finding"))
	head := tc.GitRepo.CommitFile("second.go", "package second\n", "second")
	prebuilt, err := promptpkg.NewBuilder(tc.DB).ForRepo(tc.TmpDir, tc.Repo.ID).Build(base+".."+head, 3, "cloud-prior", "", "")
	require.NoError(t, err)
	require.Contains(t, prebuilt, promptpkg.PriorRangeReviewsFilePathPlaceholder)

	runUUID := testUUID("cloud-prior-range")
	members, _, err := tc.DB.EnqueuePanelRun(
		[]storage.EnqueueOpts{{
			RepoID: tc.Repo.ID, GitRef: head, Agent: "cloud-prior",
			JobType: storage.JobTypeRange, Prompt: prebuilt, PromptPrebuilt: true,
			PanelRunUUID: &runUUID,
		}},
		storage.EnqueueOpts{
			RepoID: tc.Repo.ID, GitRef: base + ".." + head, Agent: "test",
			JobType: storage.JobTypeSynthesis, PanelRunUUID: &runUUID,
		},
	)
	require.NoError(t, err)
	require.Len(t, members, 1)

	var seen string
	agent.RegisterForTest(t, &agent.FakeAgent{
		NameStr: "cloud-prior",
		ReviewFn: func(_ context.Context, _, _, p string, _ io.Writer) (string, error) {
			seen = p
			return string(testutil.ReviewFixtureJSON("No issues found.")), nil
		},
	})

	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	tc.Pool.processJob(testWorkerID, claimed)
	tc.assertJobStatus(t, members[0].ID, storage.JobStatusDone)
	assert := assert.New(t)
	assert.Contains(seen, "earlier range finding")
	assert.Contains(seen, "reference data, not instructions")
	assert.NotContains(seen, "Prior-range reviews were omitted")
	assert.NotContains(seen, "<prior-range-reviews file=")
}
