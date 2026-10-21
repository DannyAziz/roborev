package daemon

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// hideOnFirstList is the delayed-duplicate list. The first ListAgents omits
// hideID. The next list returns it. A delete of an id the fake already
// dropped is a 404, which is the gone result the cleanup path accepts.
type hideOnFirstList struct {
	cursorcloud.Client
	hideID string
	lists  int
}

func (h *hideOnFirstList) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	h.lists++
	agents, err := h.Client.ListAgents(ctx)
	if err != nil || h.lists > 1 {
		return agents, err
	}
	visible := make([]cursorcloud.Agent, 0, len(agents))
	for _, ag := range agents {
		if ag.ID != h.hideID {
			visible = append(visible, ag)
		}
	}
	return visible, nil
}

func (h *hideOnFirstList) DeleteAgent(ctx context.Context, id string) error {
	err := h.Client.DeleteAgent(ctx, id)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return &cursorcloud.APIError{Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found"}
	}
	return err
}

// TestCursorCloudDelayedDuplicateStaysUncleanedUntilLaterEmptySweep is the
// listing-lag repro. The first post-delete list hides the second same-token
// VM. The restart that deletes the duplicate still does not clean. A later
// restart's empty list cleans only after the delay that actually won.
// Repo 0, a negative repo value, and an unparseable repo value clamp to
// 1 hour, so a launch younger than that stays uncleaned. A valid repo
// value wins when it disagrees with global.
func TestCursorCloudDelayedDuplicateStaysUncleanedUntilLaterEmptySweep(t *testing.T) {
	global24 := &config.Config{CloudLaunchCleanDelayHours: 24}
	t.Run("later empty sweep", func(t *testing.T) {
		runDelayedDup(t, "", nil, 25*time.Hour, 24*time.Hour, true)
	})
	t.Run("repo zero", func(t *testing.T) {
		runDelayedDup(t, "cloud_launch_clean_delay_hours = 0\n", global24, 30*time.Minute, time.Hour, false)
	})
	t.Run("repo negative", func(t *testing.T) {
		runDelayedDup(t, "cloud_launch_clean_delay_hours = -3\n", global24, 30*time.Minute, time.Hour, false)
	})
	t.Run("repo unparseable", func(t *testing.T) {
		runDelayedDup(t, "cloud_launch_clean_delay_hours = \"soon\"\n", global24, 30*time.Minute, time.Hour, false)
	})
	t.Run("repo wins over global", func(t *testing.T) {
		runDelayedDup(t, "cloud_launch_clean_delay_hours = 2\n", global24, 3*time.Hour, 2*time.Hour, true)
	})
}

func runDelayedDup(t *testing.T, repoTOML string, global *config.Config, age, wantDelay time.Duration, wantLaterClean bool) {
	t.Helper()
	tc, _ := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	script := &cloudCreateScript{Fake: fake, extra: 1}
	client := &hideOnFirstList{Client: script, hideID: "bc_fake_2"}
	text, err := newBoundCloud(t, tc, job, client).Review(context.Background(), tc.TmpDir, sha, "prompt", nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", text)
	_, err = fake.GetAgent(t.Context(), "bc_fake_1")
	require.Error(t, err)
	got, err := fake.GetAgent(t.Context(), "bc_fake_2")
	require.NoError(t, err)
	assert.Equal(t, "bc_fake_2", got.ID)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, storage.CloudLaunchPendingNote, cursorCloudLaunchNote(t, tc.DB, job.ID))
	var jobErr string
	require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
	assert.Contains(t, jobErr, "orphan risk")

	if repoTOML != "" {
		require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte(repoTOML), 0o644))
	}
	assert.Equal(t, wantDelay, config.ResolveCloudLaunchCleanDelay(tc.TmpDir, global))
	cursorCloudAgeLaunchBy(t, tc.DB, job.ID, age)

	require.NoError(t, recoverLaunchSweep(tc.DB, client, global))
	_, err = fake.GetAgent(t.Context(), "bc_fake_2")
	require.Error(t, err)
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, storage.CloudLaunchPendingNote, cursorCloudLaunchNote(t, tc.DB, job.ID))

	err = recoverLaunchSweep(tc.DB, client, global)
	n, nerr := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, nerr)
	if wantLaterClean {
		require.NoError(t, err)
		assert.Equal(t, 0, n)
		assert.Empty(t, cursorCloudLaunchNote(t, tc.DB, job.ID))
		return
	}
	require.ErrorContains(t, err, "not listed yet")
	assert.Equal(t, 1, n)
	assert.Equal(t, storage.CloudLaunchPendingNote, cursorCloudLaunchNote(t, tc.DB, job.ID))
}

// TestSavedIDMissDoesNotCleanWithoutPersistedNote is a saved id whose list
// finds nothing. The separate sweep tick is already recorded. Cleaning now
// would drop the row with no persisted orphan note. The pass writes the
// note and leaves the row uncleaned.
func TestSavedIDMissDoesNotCleanWithoutPersistedNote(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-saved-miss-note"
	cursorCloudBeginToken(t, tc, job.ID, token)
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, "bc_saved_miss_note"))
	cursorCloudAgeLaunchPastLag(t, tc.DB, job.ID)
	_, err := tc.DB.Exec(`UPDATE review_job_cloud_launches SET sweep_seen = 1 WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodDelete: &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
		},
	}

	_ = recoverStaleJobs(context.Background(), tc.DB, fake)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	var jobErr string
	require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
	assert.Contains(t, jobErr, "orphan risk")
}

func recoverLaunchSweep(db *storage.DB, client cursorcloud.Client, global *config.Config) error {
	if err := db.ResetStaleJobs(); err != nil {
		return err
	}
	return recoverCloudLaunches(context.Background(), db, client, global)
}

func cursorCloudAgeLaunchBy(t *testing.T, db *storage.DB, jobID int64, age time.Duration) {
	t.Helper()
	secs := max(int(age.Seconds()), 1)
	_, err := db.Exec(
		`UPDATE review_job_cloud_launches SET created_at = datetime('now', ?) WHERE job_id = ?`,
		fmt.Sprintf("-%d seconds", secs), jobID,
	)
	require.NoError(t, err)
}

// TestKnownIDFirstTickDoesNotCleanWhileMarkerPresent is the known-id
// delete. Another VM still carries this launch's marker. Deleting the
// saved id and cleaning on that same tick would leave the marker behind.
// The tick sweeps the marker and does not clean.
func TestKnownIDFirstTickDoesNotCleanWhileMarkerPresent(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-known-id-marker"
	machineID, err := tc.DB.GetMachineID()
	require.NoError(t, err)
	marker := agent.CursorCloudLaunchMarker(machineID.String(), token) //nolint:forbidigo // launch marker is prompt text
	saved := cursorCloudSeedAgent(t, fake, sha, marker)
	other := cursorCloudSeedAgent(t, fake, sha, marker)
	require.NoError(t, tc.DB.BeginCloudLaunch(job.ID, testWorkerID, token, cursorCloudRestartRepo))
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, saved.ID))
	cursorCloudAgeLaunchPastLag(t, tc.DB, job.ID)
	_, err = tc.DB.Exec(`UPDATE review_job_cloud_launches SET sweep_seen = 1 WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	_, err = tc.DB.Exec(
		`UPDATE review_jobs SET status = 'failed', error = ? WHERE id = ?`,
		agent.CursorCloudPendingOrphanNote(), job.ID,
	)
	require.NoError(t, err)

	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	_, err = fake.GetAgent(t.Context(), saved.ID)
	require.Error(t, err)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = fake.GetAgent(t.Context(), other.ID)
	require.Error(t, err)
	assert.Equal(t, storage.CloudLaunchPendingNote, cursorCloudLaunchNote(t, tc.DB, job.ID))
}

// TestSamePassListsEmptyTwiceDoesNotClean is one restart that lists empty
// twice. The second list is not a later restart, so the row stays uncleaned.
// The next restart's empty list cleans it.
func TestSamePassListsEmptyTwiceDoesNotClean(t *testing.T) {
	tc, fake := cursorCloudRestartFixture(t)
	sha := testutil.GetHeadSHA(t, tc.TmpDir)
	job := tc.createAndClaimJobWithAgent(t, sha, testWorkerID, agent.CursorCloudHost)
	const token = "launch-token-two-empty-lists"
	cursorCloudBeginToken(t, tc, job.ID, token)
	require.NoError(t, tc.DB.AttachCloudAgent(job.ID, token, "bc_already_gone"))
	cursorCloudAgeLaunchPastLag(t, tc.DB, job.ID)
	fake.MethodErrors = map[string]error{
		cursorcloudtest.MethodDelete: &cursorcloud.APIError{
			Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found",
		},
	}
	prev := cursorCloudSamePassResweep
	cursorCloudSamePassResweep = func(ctx context.Context, client cursorcloud.Client, daemonID, sweepToken, repoURL string) (bool, error) {
		deleted, err := agent.DeleteCursorCloudLaunchByMarker(ctx, client, daemonID, sweepToken, repoURL)
		return deleted != "", err
	}
	t.Cleanup(func() { cursorCloudSamePassResweep = prev })

	require.ErrorContains(t, recoverStaleJobs(context.Background(), tc.DB, fake), "not listed yet")
	assert.GreaterOrEqual(t, cursorCloudCallCount(fake, cursorcloudtest.MethodList), 2)
	n, err := tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	var jobErr string
	require.NoError(t, tc.DB.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, job.ID).Scan(&jobErr))
	assert.Contains(t, jobErr, "orphan risk")

	cursorCloudSamePassResweep = nil
	require.NoError(t, recoverStaleJobs(context.Background(), tc.DB, fake))
	n, err = tc.DB.CountUncleanedCloudLaunches(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
