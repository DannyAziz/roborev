package daemon

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/testutil"
)

// cursorCloudUnconfirmedCells is a 5xx and a deadline. Both are transient
// list or match failures. They are separate from the 429 pins: a rate limit
// already has its own tests, and treating these two as a confirmed delete
// marks the launch cleaned while the VM is still unknown.
func cursorCloudUnconfirmedCells(op string) []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{
			name: "5xx",
			err: &cursorcloud.APIError{
				Op: op, StatusCode: http.StatusInternalServerError, Message: "unavailable",
			},
		},
		{name: "deadline", err: context.DeadlineExceeded},
	}
}

func cursorCloudSentCreateErr() error {
	return &cursorcloud.APIError{
		Op: "create agent", StatusCode: http.StatusInternalServerError, Message: "boom",
	}
}

// TestCursorCloudRestartMarkerListErrorStaysUncleaned is the restart marker
// list. DeleteCursorCloudLaunchByMarker asks ListAgents once. A 5xx or a
// deadline is not an empty list: the saved id may already be gone while
// another VM for the token is still up. Ignoring that error marks the row
// cleaned. The assertion is the row, not how many times the list was called.
func TestCursorCloudRestartMarkerListErrorStaysUncleaned(t *testing.T) {
	for _, cell := range cursorCloudUnconfirmedCells("list agents") {
		t.Run(cell.name, func(t *testing.T) {
			tc, fake := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			_, dup := cursorCloudSeedMarkedPair(t, tc, fake, sha, job.ID, "launch-token-marker-list-"+cell.name)
			fake.MethodErrors = map[string]error{cursorcloudtest.MethodList: cell.err}

			err := recoverStaleJobs(context.Background(), tc.DB, fake)
			assert := assert.New(t)
			assert.Equal(0, cursorCloudCleanedFlag(t, tc, job.ID))
			require.ErrorContains(t, err, "cleanup list")
			got, getErr := fake.GetAgent(t.Context(), dup.ID)
			require.NoError(t, getErr)
			assert.Equal(dup.ID, got.ID)
		})
	}
}

// TestCursorCloudReconcileMatchErrorStaysUncleaned is a reconcile match
// error before any VM for the token is confirmed. The conversation read
// fails, so found is still false. The delete-failure pin does not cover
// that shape: there the match is already known and the delete itself fails.
// Firing the cleaned callback on this error marks the row cleaned.
func TestCursorCloudReconcileMatchErrorStaysUncleaned(t *testing.T) {
	for _, cell := range cursorCloudUnconfirmedCells("conversation") {
		t.Run(cell.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tc, _ := cursorCloudRestartFixture(t)
				sha := testutil.GetHeadSHA(t, tc.TmpDir)
				job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
				fake := &cursorcloudtest.Fake{
					MethodErrors: map[string]error{cursorcloudtest.MethodPrompt: cell.err},
				}
				script := &cloudCreateScript{Fake: fake, createErr: cursorCloudSentCreateErr()}

				_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
				assert.Equal(t, 0, cursorCloudCleanedFlag(t, tc, job.ID))
				require.Error(t, err)
				fake.MethodErrors = nil
				listed, listErr := fake.ListAgents(t.Context())
				require.NoError(t, listErr)
				require.NotEmpty(t, listed)
			})
		})
	}
}

// TestCursorCloudReconcileListErrorStaysUncleaned is a reconcile list that
// stays 5xx or deadline after the in-process retries. A 429 list already
// leaves the row uncleaned and stores the orphan note. These two do the
// same. Treating either as a finished sweep runs the cleaned callback.
func TestCursorCloudReconcileListErrorStaysUncleaned(t *testing.T) {
	for _, cell := range cursorCloudUnconfirmedCells("list agents") {
		t.Run(cell.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tc, _ := cursorCloudRestartFixture(t)
				sha := testutil.GetHeadSHA(t, tc.TmpDir)
				job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
				fake := &cursorcloudtest.Fake{
					MethodErrors: map[string]error{cursorcloudtest.MethodList: cell.err},
				}
				script := &cloudCreateScript{Fake: fake, createErr: cursorCloudSentCreateErr()}

				_, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
				assert := assert.New(t)
				assert.Equal(0, cursorCloudCleanedFlag(t, tc, job.ID))
				assert.Contains(cursorCloudStoredError(t, tc.DB, job.ID), "orphan risk")
				require.Error(t, err)
				fake.MethodErrors = nil
				listed, listErr := fake.ListAgents(t.Context())
				require.NoError(t, listErr)
				require.NotEmpty(t, listed)
			})
		})
	}
}

// TestCursorCloudPostDeleteSweepErrorStaysUncleaned is the sweep after the
// id from this response was deleted. A second VM shares the marker. Its
// conversation read fails with a 5xx or a deadline, so the sweep did not
// confirm that VM is gone. A deadline or a 5xx from that read must not mark
// the row cleaned. The job error keeps the orphan-agent note.
func TestCursorCloudPostDeleteSweepErrorStaysUncleaned(t *testing.T) {
	for _, cell := range cursorCloudUnconfirmedCells("conversation") {
		t.Run(cell.name, func(t *testing.T) {
			tc, _ := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			fake := &cursorcloudtest.Fake{
				CreateStatus: cursorcloud.StatusFinished,
				Final:        "ok",
				MethodErrors: map[string]error{cursorcloudtest.MethodPrompt: cell.err},
			}
			script := &cloudCreateScript{Fake: fake, extra: 1}

			text, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
			require.NoError(t, err)
			assert.Equal(t, "ok", text)
			assert := assert.New(t)
			assert.Equal(0, cursorCloudCleanedFlag(t, tc, job.ID))
			assert.Contains(cursorCloudStoredError(t, tc.DB, job.ID), "orphan agent")
			fake.MethodErrors = nil
			listed, listErr := fake.ListAgents(t.Context())
			require.NoError(t, listErr)
			require.NotEmpty(t, listed)
		})
	}
}

// TestCursorCloudPostDeleteListErrorStaysUncleaned is the cleanup list after
// the known id was deleted. The list is retried, then a 5xx or a deadline
// still hides any provider-side duplicate. Treating that error as an empty
// sweep marks the row cleaned. The job error carries the orphan-risk note.
func TestCursorCloudPostDeleteListErrorStaysUncleaned(t *testing.T) {
	for _, cell := range cursorCloudUnconfirmedCells("list agents") {
		t.Run(cell.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tc, _ := cursorCloudRestartFixture(t)
				sha := testutil.GetHeadSHA(t, tc.TmpDir)
				job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
				fake := &cursorcloudtest.Fake{
					CreateStatus: cursorcloud.StatusFinished,
					Final:        "ok",
					MethodErrors: map[string]error{cursorcloudtest.MethodList: cell.err},
				}
				script := &cloudCreateScript{Fake: fake, extra: 1}

				text, err := newBoundCloud(t, tc, job, script).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
				require.NoError(t, err)
				assert.Equal(t, "ok", text)
				assert := assert.New(t)
				assert.Equal(0, cursorCloudCleanedFlag(t, tc, job.ID))
				assert.Contains(cursorCloudStoredError(t, tc.DB, job.ID), "orphan risk")
				fake.MethodErrors = nil
				listed, listErr := fake.ListAgents(t.Context())
				require.NoError(t, listErr)
				require.NotEmpty(t, listed)
			})
		})
	}
}
