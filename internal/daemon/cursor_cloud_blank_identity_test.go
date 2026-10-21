package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/testutil"
)

// TestCursorCloudRestartBlankIdentityNeverCleans is the fail-closed pin for a
// launch row whose repo_identity is blank, as every row stored before that
// column has, or unusable. The saved id is deleted, an earlier pass listed
// the launch and noted the orphan, and the launch is past the delay. The
// marker list then runs against the checkout remote, which need not be the
// repository the launch was created on, so an empty list there must not
// clean the row. The stored-identity case is the control: the same setup
// cleans.
func TestCursorCloudRestartBlankIdentityNeverCleans(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		cleaned  int
	}{
		{name: "stored", identity: cursorCloudRestartRepo, cleaned: 1},
		{name: "blank", identity: "", cleaned: 0},
		{name: "unusable", identity: "file:///", cleaned: 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tc, fake := cursorCloudRestartFixture(t)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
			cursorCloudPersistAttempt(t, tc, fake, job.ID, "launch-token-identity-"+tt.name, true)
			_, err := tc.DB.Exec(`UPDATE review_job_cloud_launches SET repo_identity = ?, sweep_seen = 1 WHERE job_id = ?`, tt.identity, job.ID)
			require.NoError(t, err)
			_, err = tc.DB.Exec(`UPDATE review_jobs SET error = ? WHERE id = ?`, agent.CursorCloudPendingOrphanNote(), job.ID)
			require.NoError(t, err)
			cursorCloudAgeLaunch(t, tc.DB, job.ID)

			for range 2 {
				_ = recoverStaleJobs(context.Background(), tc.DB, fake)
				assert.Equal(t, tt.cleaned, cursorCloudCleanedFlag(t, tc, job.ID))
			}
			if tt.cleaned == 0 {
				var jobErr string
				require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
				assert.Contains(t, jobErr, "orphan risk")
			}
		})
	}
}
