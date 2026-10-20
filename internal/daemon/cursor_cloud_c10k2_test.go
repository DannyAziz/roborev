package daemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/testutil"
)

// TestNoUsableRepoPathDoesNotCleanByKey is the pin for token-only recovery
// when neither the stored identity nor the checkout remote can be listed.
// Cleaning that row by its token would drop a launch that was never swept.
func TestNoUsableRepoPathDoesNotCleanByKey(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-nousable-key"
	cursorCloudLegacyToken(t, tc, job.ID, token)
	tc.GitRepo.Run("remote", "remove", "origin")
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	live := cursorCloudSeedAgent(t, fake, sha, agent.CursorCloudLaunchMarker(machineID.String(), token)) //nolint:forbidigo // launch marker is prompt text

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	got, getErr := fake.GetAgent(t.Context(), live.ID)
	require.NoError(t, getErr)
	assert.Equal(t, live.ID, got.ID)
	assert.Empty(t, cursorCloudCallIDs(fake, cursorcloudtest.MethodList))
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, updated.Error, "orphan risk")
}

// TestCreate429LeavesUnconfirmedLaunchUncleaned is the pin for a create that
// returns 429 after the VM is stored. Cleaning on that status, before the
// reconcile list confirms a delete, drops a launch that is still up.
func TestCreate429LeavesUnconfirmedLaunchUncleaned(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{}
	script := &list429Script{Fake: fake, retryAfter: time.Millisecond, always: true}

	_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	script.mu.Lock()
	id := script.createdID
	script.mu.Unlock()
	got, getErr := fake.GetAgent(t.Context(), id)
	require.NoError(t, getErr)
	assert.Equal(t, id, got.ID)
}

// TestRestartMarkerListErrorDoesNotClean is the pin for the restart sweep's
// list. A 5xx or a deadline is not an empty list. The saved id can be
// deleted and the row still stays uncleaned while another VM for the token
// was never listed.
func TestRestartMarkerListErrorDoesNotClean(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "5xx",
			err: &cursorcloud.APIError{
				Op: "list agents", StatusCode: http.StatusInternalServerError, Message: "unavailable",
			},
		},
		{name: "deadline", err: context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtc, fake := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, wtc.TmpDir)
			job := wtc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			const token = "launch-token-list-clean"
			machineID, err := wtc.DB.GetMachineID()
			require.NoError(t, err)
			marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
			saved := cursorCloudSeedAgent(t, fake, sha, marker)
			dup := cursorCloudSeedAgent(t, fake, sha, marker)
			require.NoError(t, wtc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, cursorCloudRestartRepo))
			require.NoError(t, wtc.DB.AttachCloudAgent(job.ID, token, saved.ID))
			fake.MethodErrors = map[string]error{cursorcloudtest.MethodList: tc.err}

			err = recoverStaleJobs(context.Background(), wtc.DB, fake)
			require.Error(t, err)
			assert.Equal(t, 0, cursorCloudCleanedFlag(t, wtc, job.ID))
			_, getErr := fake.GetAgent(t.Context(), saved.ID)
			require.Error(t, getErr)
			got, getErr := fake.GetAgent(t.Context(), dup.ID)
			require.NoError(t, getErr)
			assert.Equal(t, dup.ID, got.ID)
		})
	}
}

// TestReconcileMatchErrorDoesNotMarkCleaned is the pin for a reconcile that
// found the launch and then failed to delete it. The cleaned callback stays
// unused, and the VM is still listed.
func TestReconcileMatchErrorDoesNotMarkCleaned(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{}
	script := &cloudCreateScript{
		Fake: fake,
		createErr: &cursorcloud.APIError{
			Op: "create agent", StatusCode: http.StatusInternalServerError, Message: "boom",
		},
		failDelete: 1,
	}

	_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.Error(t, err)
	assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	listed, err := fake.ListAgents(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 1)
	_, err = fake.GetAgent(t.Context(), listed[0].ID)
	require.NoError(t, err)
}

// listErrCloud stores the create, then every list returns err.
type listErrCloud struct {
	*cursorcloudtest.Fake
	createErr error
	listErr   error
}

func (c *listErrCloud) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	ag, err := c.Fake.CreateAgent(ctx, req)
	if err != nil {
		return ag, err
	}
	if c.createErr != nil {
		return ag, c.createErr
	}
	return ag, nil
}

func (c *listErrCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	return nil, c.listErr
}

// TestReconcileListErrorDoesNotMarkCleaned is the pin for a reconcile list
// that stays on 5xx or deadline. That is not a confirmed miss, so the
// cleaned callback does not run and the stored VM stays up.
func TestReconcileListErrorDoesNotMarkCleaned(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "5xx",
			err: &cursorcloud.APIError{
				Op: "list agents", StatusCode: http.StatusInternalServerError, Message: "unavailable",
			},
		},
		{name: "deadline", err: context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtc, _ := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, wtc.TmpDir)
			job := wtc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			fake := &cursorcloudtest.Fake{}
			client := &listErrCloud{
				Fake: fake,
				createErr: &cursorcloud.APIError{
					Op: "create agent", StatusCode: http.StatusInternalServerError, Message: "boom",
				},
				listErr: tc.err,
			}

			_, err := newBoundCloud(t, wtc, job, client).Review(context.Background(), wtc.TmpDir, sha, "prompt", nil)
			require.Error(t, err)
			require.ErrorContains(t, err, "orphan risk")
			assert.Equal(t, 0, cursorCloudCleanedFlag(t, wtc, job.ID))
			listed, listErr := fake.ListAgents(t.Context())
			require.NoError(t, listErr)
			require.Len(t, listed, 1)
		})
	}
}

// promptDeadlineCloud answers the review, then the sweep's conversation
// read returns a deadline.
type promptDeadlineCloud struct {
	*cursorcloudtest.Fake
	extra int
}

func (c *promptDeadlineCloud) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	ag, err := c.Fake.CreateAgent(ctx, req)
	if err != nil {
		return ag, err
	}
	for range c.extra {
		if _, err := c.Fake.CreateAgent(ctx, req); err != nil {
			return ag, err
		}
	}
	return ag, nil
}

func (c *promptDeadlineCloud) UserPrompt(context.Context, string) (string, error) {
	return "", context.DeadlineExceeded
}

// TestPostDeleteSweepDeadlineDoesNotClean is the pin for the sweep after a
// confirmed delete of the id from the create response. A deadline while
// reading another VM for the token leaves the row uncleaned.
func TestPostDeleteSweepDeadlineDoesNotClean(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	client := &promptDeadlineCloud{Fake: fake, extra: 1}

	text, err := newBoundCloud(t, tc, job, client).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", text)
	assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	_, getErr := fake.GetAgent(t.Context(), "bc_fake_1")
	require.Error(t, getErr)
	got, getErr := fake.GetAgent(t.Context(), "bc_fake_2")
	require.NoError(t, getErr)
	assert.Equal(t, "bc_fake_2", got.ID)
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, updated.Error, "orphan")
}

// cleanupListErrCloud finishes the review, then the cleanup list fails.
type cleanupListErrCloud struct {
	*cursorcloudtest.Fake
	listErr error
}

func (c *cleanupListErrCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	return nil, c.listErr
}

// TestPostDeleteCleanupListErrorDoesNotClean is the pin for the list that
// runs after the created id is deleted. A list error is not an empty sweep,
// so the row stays uncleaned.
func TestPostDeleteCleanupListErrorDoesNotClean(t *testing.T) {
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	client := &cleanupListErrCloud{
		Fake: fake,
		listErr: &cursorcloud.APIError{
			Op: "list agents", StatusCode: http.StatusInternalServerError, Message: "unavailable",
		},
	}

	text, err := newBoundCloud(t, tc, job, client).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", text)
	assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(t, updated.Error, "cleanup list")
	assert.Contains(t, updated.Error, "orphan risk")
}
