package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testutil"
)

// TestCLICursorCloudRejectsDirtyAndRangeBeforeLaunch drives the review
// command's enqueue request for a dirty review and a range review. Both must
// be rejected before a job exists for a worker to launch.
func TestCLICursorCloudRejectsDirtyAndRangeBeforeLaunch(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 0
	cfg.ServerAddr = "127.0.0.1:0"
	srv := daemon.NewServer(db, cfg, "")
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(context.Background()) }()

	var info *daemon.RuntimeInfo
	var startErr error
	require.Eventually(t, func() bool {
		select {
		case startErr = <-errCh:
			return true
		default:
		}
		var err error
		info, err = daemon.ReadRuntime()
		return err == nil && info != nil && info.Address != ""
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, startErr)
	require.NotNil(t, info)
	t.Cleanup(func() { _ = srv.Stop() })

	oldAddr, oldEP := serverAddr, parsedServerEndpoint
	serverAddr = info.Address
	parsedServerEndpoint = nil
	t.Cleanup(func() {
		serverAddr = oldAddr
		parsedServerEndpoint = oldEP
	})

	repo := newTestGitRepo(t)
	first := repo.CommitFile("a.txt", "one\n", "one")
	second := repo.CommitFile("b.txt", "two\n", "two")
	require.NoError(t, os.WriteFile(filepath.Join(repo.Dir, "a.txt"), []byte("dirty\n"), 0o600))

	_, _, dirtyErr := executeReviewCmd("--repo", repo.Dir, "--host", "cursor-cloud", "--dirty")
	require.Error(t, dirtyErr)
	assert.Contains(t, dirtyErr.Error(), "cannot run a dirty review")

	_, _, rangeErr := executeReviewCmd("--repo", repo.Dir, "--host", "cursor-cloud", first, second)
	require.Error(t, rangeErr)
	assert.Contains(t, rangeErr.Error(), "cannot run a range review")

	jobs, err := db.ListJobs("", "", 20, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs)
}
