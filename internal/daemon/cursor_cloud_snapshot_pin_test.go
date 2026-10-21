package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestHostileRepoSnapshotHandoffIsRefusedBeforeCreate enqueues a cursor-cloud
// task whose whole prompt is a snapshot handoff into a hostile repository.
// The handoff reaches a secret outside the repository by an absolute path,
// a '..' segment, or a symlinked .roborev. The launch is refused before
// CreateAgent and no request body or job error carries the secret.
func TestHostileRepoSnapshotHandoffIsRefusedBeforeCreate(t *testing.T) {
	const secret = "HOSTILE-REPO-HANDOFF-SECRET"
	for _, tc := range []struct {
		name string
		path func(t *testing.T, repo, secretPath string) string
	}{
		{name: "absolute path", path: func(t *testing.T, repo, secretPath string) string {
			return secretPath
		}},
		{name: "dot-dot out of .roborev", path: func(t *testing.T, repo, secretPath string) string {
			dir := filepath.Join(repo, ".roborev")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			rel, err := filepath.Rel(dir, secretPath)
			require.NoError(t, err)
			return dir + string(filepath.Separator) + rel
		}},
		{name: "symlinked .roborev", path: func(t *testing.T, repo, secretPath string) string {
			if runtime.GOOS == "windows" {
				t.Skip("skipping on Windows due to symlink semantics")
			}
			require.NoError(t, os.Symlink(filepath.Dir(secretPath), filepath.Join(repo, ".roborev")))
			return filepath.Join(repo, ".roborev", filepath.Base(secretPath))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, fake := cursorCloudPinServer(t, nil)
			repo := testutil.InitTestGitRepo(t, filepath.Join(t.TempDir(), "repo"))
			repo.CommitFile("a.txt", "one\n", "one")
			secretPath := filepath.Join(t.TempDir(), "prompt.md")
			require.NoError(t, os.WriteFile(secretPath, []byte(secret), 0o600))
			created := enqueueViaHTTP(t, server, EnqueueRequest{
				RepoPath:     repo.Path(),
				GitRef:       "HEAD",
				Agent:        agent.CursorCloudHost,
				CustomPrompt: prompt.SnapshotHandoffPrompt(tc.path(t, repo.Path(), secretPath)),
			})
			require.Equal(t, agent.CursorCloudHost, created.Agent)
			claimed, err := server.db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, created.ID, claimed.ID)

			server.workerPool.processJob(testWorkerID, claimed)

			assert.Zero(t, createCountFromFake(fake))
			for _, call := range fake.Calls() {
				raw, marshalErr := json.Marshal(call.Request)
				require.NoError(t, marshalErr)
				assert.NotContains(t, string(raw), secret)
			}
			job, err := server.db.GetJobByID(created.ID)
			require.NoError(t, err)
			assert.NotEqual(t, storage.JobStatusDone, job.Status)
			assert.NotContains(t, job.Error, secret)
		})
	}
}

// TestOversizedReviewSendsPreparedPrompt keeps the legitimate handoff working
// for the default and a configured snapshot_dir.
func TestOversizedReviewSendsPreparedPrompt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
	}{
		{name: "default snapshot dir"},
		{name: "custom snapshot dir", config: "max_prompt_size = 64\nsnapshot_dir = \"review-snapshots\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const marker = "OVERSIZED-REVIEW-SNAPSHOT-MARKER"
			cfg := config.DefaultConfig()
			cfg.MaxWorkers = 0
			cfg.DefaultMaxPromptSize = 64
			server, fake := cursorCloudPinServer(t, cfg)
			repo := testutil.InitTestGitRepo(t, filepath.Join(t.TempDir(), "repo"))
			if tc.config != "" {
				require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte(tc.config), 0o644))
			}
			repo.CommitFile("a.txt", marker+"\n"+strings.Repeat("line\n", 40), "marker")
			created := enqueueViaHTTP(t, server, EnqueueRequest{
				RepoPath: repo.Path(),
				GitRef:   "HEAD",
				Host:     agent.CursorCloudHost,
			})
			require.Equal(t, agent.CursorCloudHost, created.Agent)
			claimed, err := server.db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, created.ID, claimed.ID)

			server.workerPool.processJob(testWorkerID, claimed)

			require.Equal(t, 1, createCountFromFake(fake))
			for _, call := range fake.Calls() {
				if call.Method == cursorcloudtest.MethodCreate {
					assert.Contains(t, call.Request.Prompt, marker)
					assert.NotContains(t, call.Request.Prompt, "Read the complete task prompt from")
				}
			}
		})
	}
}

func cursorCloudPinServer(t *testing.T, cfg *config.Config) (*Server, *cursorcloudtest.Fake) {
	t.Helper()
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	t.Setenv(agent.EnvDefaultHost, "")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	db, _ := testutil.OpenTestDBWithDir(t)
	if cfg == nil {
		cfg = config.DefaultConfig()
		cfg.MaxWorkers = 0
	}
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: cursorCloudPassJSON}
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	cloud := &agent.CursorCloudAgent{Client: fake, RepoURL: "https://github.com/acme/api.git"}
	cloud.SetLaunchLimitsForTest(4, 20)
	agent.Register(cloud)
	t.Cleanup(func() { agent.Register(prev) })
	return server, fake
}
