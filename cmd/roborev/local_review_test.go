package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// reviewHarness encapsulates a test git repo, cobra command, and output buffer.
type reviewHarness struct {
	t   *testing.T
	Dir string
	Cmd *cobra.Command
	Out *bytes.Buffer
}

// newReviewHarness creates a harness with the real CLI command structure.
func newReviewHarness(t *testing.T) *reviewHarness {
	t.Helper()
	repo := testutil.NewTestRepoWithCommit(t)
	cmd := reviewCmd() // Use real factory
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	return &reviewHarness{t: t, Dir: repo.Root, Cmd: cmd, Out: &out}
}

// writeConfig writes a .roborev.toml in the repo directory.
func (h *reviewHarness) writeConfig(content string) {
	h.t.Helper()
	path := filepath.Join(h.Dir, ".roborev.toml")
	err := os.WriteFile(path, []byte(content), 0o644)
	require.NoError(h.t, err)
}

// runCmd executes the command with the given arguments.
func (h *reviewHarness) runCmd(args ...string) error {
	h.t.Helper()
	h.Cmd.SetArgs(args)
	return h.Cmd.Execute()
}

// assertOutputContains checks if the output contains the expected string.
func (h *reviewHarness) assertOutputContains(want string) {
	h.t.Helper()
	got := h.Out.String()
	assert.Contains(h.t, got, want, "Output missing %q. Got:\n%s", want, got)
}

// assertOutputNotContains checks if the output does not contain the forbidden string.
func (h *reviewHarness) assertOutputNotContains(want string) {
	h.t.Helper()
	got := h.Out.String()
	assert.NotContains(h.t, got, want, "Output should not contain %q. Got:\n%s", want, got)
}

// assertErrorContains checks if the error contains the expected string.
func (h *reviewHarness) assertErrorContains(err error, want string) {
	h.t.Helper()
	require.Error(h.t, err, "Expected error containing %q", want)
	assert.Contains(h.t, err.Error(), want, "Error missing %q. Got: %v", want, err)
}

// runOpts holds optional parameters for run, mapping to CLI flags.
type runOpts struct {
	Revision   string
	Agent      string
	Model      string
	Reasoning  string
	ReviewType string
	Quiet      bool
	Dirty      bool
}

// run converts opts to CLI flags and executes the command.
func (h *reviewHarness) run(opts runOpts) error {
	h.t.Helper()
	// Always enforce --local and --repo to ensure we target the test repo and don't need daemon
	args := []string{"--local", "--repo", h.Dir}

	if opts.Revision != "" {
		args = append(args, opts.Revision)
	}
	if opts.Dirty {
		args = append(args, "--dirty")
	}
	if opts.Agent != "" {
		args = append(args, "--agent", opts.Agent)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Reasoning != "" {
		args = append(args, "--reasoning", opts.Reasoning)
	}
	if opts.ReviewType != "" {
		args = append(args, "--type", opts.ReviewType)
	}
	if opts.Quiet {
		args = append(args, "--quiet")
	}

	return h.runCmd(args...)
}

func TestLocalReviewRefusesCursorCloudHost(t *testing.T) {
	var httpCalls atomic.Int32
	var launchRows atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		httpCalls.Add(1)
	}))
	t.Cleanup(srv.Close)

	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	cloud := &agent.CursorCloudAgent{
		Client:  cursorcloud.NewHTTPClient("cursor-test-secret-not-real", cursorcloud.WithBaseURL(srv.URL)),
		RepoURL: "https://github.com/example/repo.git",
	}
	if field := reflect.ValueOf(cloud).Elem().FieldByName("BindLaunch"); field.IsValid() && field.CanSet() {
		field.Set(reflect.MakeFunc(field.Type(), func([]reflect.Value) []reflect.Value {
			launchRows.Add(1)
			return []reflect.Value{reflect.Zero(field.Type().Out(0))}
		}))
	}
	agent.Register(cloud)
	t.Cleanup(func() { agent.Register(prev) })

	for _, tc := range []struct {
		name string
		args []string
		cfg  string
		env  bool
	}{
		{name: "--host", args: []string{"--host", agent.CursorCloudHost}},
		{name: ".roborev.toml host", cfg: "host = \"cursor-cloud\"\n"},
		{name: "ROBOREV_DEFAULT_HOST", env: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			httpCalls.Store(0)
			launchRows.Store(0)
			t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
			t.Setenv(agent.EnvDefaultHost, "")
			t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
			if tc.env {
				t.Setenv(agent.EnvDefaultHost, agent.CursorCloudHost)
			}
			h := newReviewHarness(t)
			if tc.cfg != "" {
				h.writeConfig(tc.cfg)
			}
			err := h.runCmd(append([]string{"--local", "--repo", h.Dir, "--quiet"}, tc.args...)...)
			require.ErrorContains(t, err, "--local cannot use a cursor-cloud host")
			assert.Equal(t, int32(0), httpCalls.Load())
			assert.Equal(t, int32(0), launchRows.Load())
		})
	}
}

// cloudCreateFails is a cursor-cloud endpoint that fails the test if create
// is called. The registered agent points at it.
func cloudCreateFails(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("cursor-cloud create was called: %s %s", r.Method, r.URL.Path)
		http.Error(w, "create called", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	agent.Register(&agent.CursorCloudAgent{
		Client:  cursorcloud.NewHTTPClient("cursor-test-secret-not-real", cursorcloud.WithBaseURL(srv.URL)),
		RepoURL: "https://github.com/example/repo.git",
	})
	t.Cleanup(func() { agent.Register(prev) })
}

func seedStaleRoborevHook(t *testing.T, repoDir string) (string, []byte) {
	t.Helper()
	hooks := filepath.Join(repoDir, ".git", "hooks")
	require.NoError(t, os.MkdirAll(hooks, 0o755))
	path := filepath.Join(hooks, "post-commit")
	body := []byte("#!/bin/sh\n# roborev stale-hook-canary\necho stale-hook-canary\n")
	require.NoError(t, os.WriteFile(path, body, 0o755))
	return path, body
}

func TestLocalReviewRefusesLevelCursorCloudBeforeHooks(t *testing.T) {
	cloudCreateFails(t)
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv(agent.EnvDefaultHost, "")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	h := newReviewHarness(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.Dir, "dirty.txt"), []byte("dirty-local-diff\n"), 0o644))
	h.writeConfig("review_agent_thorough = \"cursor-cloud\"\n")
	hookPath, hookBody := seedStaleRoborevHook(t, h.Dir)
	err := h.runCmd("--local", "--repo", h.Dir, "--dirty")
	require.ErrorContains(t, err, "--local cannot use a cursor-cloud host")
	got, readErr := os.ReadFile(hookPath)
	require.NoError(t, readErr)
	assert.Equal(t, hookBody, got)
}

func TestRunLocalReviewNameGuardRefusesCursorCloud(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
			posts.Add(1)
		}
		http.Error(w, "create called", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	prev, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	agent.Register(&agent.CursorCloudAgent{
		Client:  cursorcloud.NewHTTPClient("cursor-test-secret-not-real", cursorcloud.WithBaseURL(srv.URL)),
		RepoURL: "https://github.com/example/repo.git",
	})
	t.Cleanup(func() { agent.Register(prev) })
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv(agent.EnvDefaultHost, "")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	h := newReviewHarness(t)
	h.writeConfig("review_agent_thorough = \"cursor-cloud\"\n")
	h.Cmd.SetContext(context.Background())
	err = runLocalReview(h.Cmd, h.Dir, "HEAD", "DIRTY-DIFF-CANARY", []string{"dirty.txt"}, "", "", "", "", "", false, "")
	require.ErrorContains(t, err, "--local cannot use a cursor-cloud host")
	assert.Equal(t, int32(0), posts.Load())
}

func TestLocalReviewRefusesCursorCloudBeforeHooks(t *testing.T) {
	cloudCreateFails(t)
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv(agent.EnvDefaultHost, "")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	h := newReviewHarness(t)
	err := h.runCmd("--local", "--repo", h.Dir, "--host", agent.CursorCloudHost)
	require.ErrorContains(t, err, "--local cannot use a cursor-cloud host")
	_, statErr := os.Stat(filepath.Join(h.Dir, ".git", "hooks", "post-commit"))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestLocalReviewRefusesCursorCloudAgentRoutes(t *testing.T) {
	cloudCreateFails(t)
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv(agent.EnvDefaultHost, "")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	for _, tc := range []struct {
		name   string
		args   []string
		repo   string
		global string
	}{
		{name: "--agent cursor-cloud", args: []string{"--agent", agent.CursorCloudHost}},
		{name: "workflow agent", repo: "review_agent = \"cursor-cloud\"\n"},
		{
			name:   "unavailable preferred with cursor-cloud backup",
			repo:   "agent = \"codex\"\nbackup_agent = \"cursor-cloud\"\n",
			global: "codex_cmd = \"/nonexistent/codex-for-local-refusal\"\n",
		},
		{name: "--host cursor-cloud", args: []string{"--host", agent.CursorCloudHost}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.global != "" {
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(tc.global), 0o600))
			} else {
				_ = os.Remove(config.GlobalConfigPath())
			}
			h := newReviewHarness(t)
			if tc.repo != "" {
				h.writeConfig(tc.repo)
			}
			err := h.runCmd(append([]string{"--local", "--repo", h.Dir, "--quiet"}, tc.args...)...)
			require.ErrorContains(t, err, "--local cannot use a cursor-cloud host")
		})
	}
}

func TestGoalLocalReviewRefusesCursorCloudHostSources(t *testing.T) {
	cloudCreateFails(t)
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv(agent.EnvDefaultHost, "")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	for _, tc := range []struct {
		name   string
		args   []string
		repo   string
		global string
		env    bool
	}{
		{name: "--host", args: []string{"--host", agent.CursorCloudHost}},
		{name: "repo config", repo: "host = \"cursor-cloud\"\n"},
		{name: "global config", global: "host = \"cursor-cloud\"\n"},
		{name: "ROBOREV_DEFAULT_HOST", env: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(agent.EnvDefaultHost, "")
			if tc.global != "" {
				require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte(tc.global), 0o600))
			} else {
				_ = os.Remove(config.GlobalConfigPath())
			}
			if tc.env {
				t.Setenv(agent.EnvDefaultHost, agent.CursorCloudHost)
			}
			h := newReviewHarness(t)
			if tc.repo != "" {
				h.writeConfig(tc.repo)
			}
			err := h.runCmd(append([]string{"--type", "goal", "--local", "--repo", h.Dir, "--quiet"}, tc.args...)...)
			require.ErrorContains(t, err, "--local cannot use a cursor-cloud host")
		})
	}
}

// blockCursorCloudDials replaces the transport the cursor-cloud client clones.
// Any dial fails the test, so a launch that gets past the agent's binding
// check is caught before a request leaves the process.
func blockCursorCloudDials(t *testing.T) {
	t.Helper()
	prev := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
			t.Errorf("cursor-cloud dial: %s %s", network, addr)
			return nil, errors.New("cursor-cloud dial blocked in test")
		},
	}
	t.Cleanup(func() { http.DefaultTransport = prev })
}

// TestLocalReviewNeverReachesCursorCloud pins that no --local route launches
// cursor-cloud. Every route that selects cursor-cloud, from an agent setting
// or a host source, is refused before a launch is bound or a dial is made.
// Each cell goes red if a route is added that dials, binds a launch, or sends
// a host source to cursor-cloud.
func TestLocalReviewNeverReachesCursorCloud(t *testing.T) {
	const localRefusal = "--local cannot use a cursor-cloud host"
	cloud, err := agent.Get(agent.CursorCloudHost)
	require.NoError(t, err)
	registry, ok := cloud.(*agent.CursorCloudAgent)
	require.True(t, ok)
	require.Nil(t, registry.Client)
	require.Nil(t, registry.BindLaunch)
	blockCursorCloudDials(t)

	// Only git stays on PATH, so no local CLI agent is available to pick.
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	require.NoError(t, os.Symlink(realGit, filepath.Join(binDir, "git")))

	for _, tc := range []struct {
		name   string
		args   []string
		repo   string
		global string
		env    bool
		// want is the error; empty means the local test agent ran.
		want string
	}{
		{name: "--agent cursor-cloud", args: []string{"--agent", agent.CursorCloudHost}, want: localRefusal},
		{name: "--agent cursor-cloud --dirty", args: []string{"--agent", agent.CursorCloudHost, "--dirty"}, want: localRefusal},
		{name: "repo agent", repo: "agent = \"cursor-cloud\"\n", want: localRefusal},
		{name: "repo workflow agent", repo: "review_agent = \"cursor-cloud\"\n", want: localRefusal},
		{name: "repo level agent", repo: "review_agent_thorough = \"cursor-cloud\"\n", want: localRefusal},
		{name: "global default agent", global: "default_agent = \"cursor-cloud\"\n", want: localRefusal},
		{name: "global workflow agent", global: "review_agent = \"cursor-cloud\"\n", want: localRefusal},
		{name: "repo backup agent", repo: "agent = \"codex\"\nbackup_agent = \"cursor-cloud\"\n", want: localRefusal},
		{name: "global backup agent", repo: "agent = \"codex\"\n", global: "default_backup_agent = \"cursor-cloud\"\n", want: localRefusal},
		{name: "CURSOR_API_KEY only", want: "no configured agent available"},
		{name: "--host", args: []string{"--host", agent.CursorCloudHost}, repo: "agent = \"test\"\n", want: localRefusal},
		{name: "repo host", repo: "agent = \"test\"\nhost = \"cursor-cloud\"\n", want: localRefusal},
		{name: "global host", repo: "agent = \"test\"\n", global: "host = \"cursor-cloud\"\n", want: localRefusal},
		{name: "ROBOREV_DEFAULT_HOST", repo: "agent = \"test\"\n", env: true, want: localRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
			t.Setenv(agent.EnvDefaultHost, "")
			if tc.env {
				t.Setenv(agent.EnvDefaultHost, agent.CursorCloudHost)
			}
			t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
			dataDir := testenv.SetDataDir(t)
			if tc.global != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(tc.global), 0o600))
			}
			h := newReviewHarness(t)
			// A remote lets a launch past the binding check reach the dial.
			out, err := exec.Command("git", "-C", h.Dir, "remote", "add", "origin", "https://github.com/example/repo.git").CombinedOutput()
			require.NoError(t, err, "%s", out)
			require.NoError(t, os.WriteFile(filepath.Join(h.Dir, "dirty.txt"), []byte("dirty-local-diff\n"), 0o644))
			if tc.repo != "" {
				h.writeConfig(tc.repo)
			}
			t.Setenv("PATH", binDir)
			err = h.runCmd(append([]string{"--local", "--repo", h.Dir, "--quiet"}, tc.args...)...)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}

	t.Run("goal --agent cursor-cloud", func(t *testing.T) {
		t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
		t.Setenv(agent.EnvDefaultHost, "")
		t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
		testenv.SetDataDir(t)
		h := newReviewHarness(t)
		spec := "docs/superpowers/specs/feature-design.md"
		require.NoError(t, os.MkdirAll(filepath.Join(h.Dir, "docs", "superpowers", "specs"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(h.Dir, spec), []byte("# Feature\n"), 0o600))
		t.Setenv("PATH", binDir)
		err := h.runCmd("--type", "goal", "--local", "--repo", h.Dir, "--quiet", "--agent", agent.CursorCloudHost, "--spec", spec)
		require.ErrorContains(t, err, "goal review requires a tool-disabled schema agent")
	})
}

func TestLocalReviewFlag(t *testing.T) {
	h := newReviewHarness(t)
	// Passing --help to cobra usually returns nil error but prints usage.

	err := h.runCmd("--local", "--help")
	require.NoError(t, err, "Expected no error, got: %v")

	h.assertOutputContains("--local")
	h.assertOutputContains("run review locally without daemon")
}

func TestLocalReviewRequiresAgent(t *testing.T) {
	h := newReviewHarness(t)

	err := h.run(runOpts{Agent: "test", Reasoning: "fast"})
	require.NoError(t, err, "Expected no error with test agent, got: %v")

	h.assertOutputContains("Running test review")
}

func TestLocalReviewWithDirtyDiff(t *testing.T) {
	h := newReviewHarness(t)

	// Create a new file to make the repo dirty
	newFile := filepath.Join(h.Dir, "newfile.go")
	err := os.WriteFile(newFile, []byte("package main\nfunc test() {}"), 0o644)
	require.NoError(t, err)

	err = h.run(runOpts{Dirty: true, Agent: "test", Reasoning: "fast"})
	require.NoError(t, err, "Expected no error, got: %v")

	// We don't check output for diff content because agent output is mocked.
	h.assertOutputContains("No issues found.")
}

func TestLocalReviewAgentResolution(t *testing.T) {
	h := newReviewHarness(t)
	h.writeConfig(`agent = "test"`)

	err := h.run(runOpts{Reasoning: "fast"})
	require.NoError(t, err, "Expected no error, got: %v")

	h.assertOutputContains("Running test review")
}

func TestLocalReviewModelResolution(t *testing.T) {
	h := newReviewHarness(t)
	h.writeConfig(`
agent = "test"
model = "test-model"
`)

	err := h.run(runOpts{Reasoning: "fast"})
	require.NoError(t, err, "Expected no error, got: %v")

	h.assertOutputContains("model: test-model")
}

func TestLocalReviewValidation(t *testing.T) {
	tests := []struct {
		name       string
		opts       runOpts
		wantErr    string
		wantOutput string
	}{
		{
			name:    "Invalid Reasoning",
			opts:    runOpts{Agent: "test", Reasoning: "invalid-reasoning"},
			wantErr: "invalid reasoning",
		},
		{
			name:    "Invalid Type",
			opts:    runOpts{Agent: "test", Reasoning: "fast", ReviewType: "bogus"},
			wantErr: "invalid --type",
		},
		{
			name:    "Valid Security Type",
			opts:    runOpts{Agent: "test", Reasoning: "fast", ReviewType: "security", Dirty: true, Quiet: true},
			wantErr: "no uncommitted changes",
		},
		{
			name:    "Valid Design Type",
			opts:    runOpts{Agent: "test", Reasoning: "fast", ReviewType: "design", Dirty: true, Quiet: true},
			wantErr: "no uncommitted changes",
		},
		{
			name:    "Valid Lookahead Type",
			opts:    runOpts{Agent: "test", Reasoning: "fast", ReviewType: "lookahead", Dirty: true, Quiet: true},
			wantErr: "no uncommitted changes",
		},
		{
			name:    "Empty Type Accepted",
			opts:    runOpts{Agent: "test", Reasoning: "fast", ReviewType: "", Dirty: true, Quiet: true},
			wantErr: "no uncommitted changes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newReviewHarness(t)
			err := h.run(tc.opts)
			if tc.wantErr != "" {
				h.assertErrorContains(err, tc.wantErr)
			} else {
				require.NoError(t, err, "Expected no error, got: %v")
			}
			if tc.wantOutput != "" {
				h.assertOutputContains(tc.wantOutput)
			}
		})
	}
}

func TestLocalReviewWorkflowSpecificAgent(t *testing.T) {
	h := newReviewHarness(t)
	h.writeConfig(`
agent = "codex"
review_agent_fast = "test"
`)

	err := h.run(runOpts{Reasoning: "fast"})
	require.NoError(t, err, "Expected no error, got: %v")

	h.assertOutputContains("Running test review")
}

func TestLocalReviewWorkflowSpecificModel(t *testing.T) {
	h := newReviewHarness(t)
	h.writeConfig(`
agent = "test"
model = "default-model"
review_model_thorough = "thorough-model"
`)

	err := h.run(runOpts{})
	require.NoError(t, err, "Expected no error, got: %v")

	h.assertOutputContains("model: thorough-model")
}

func TestLocalReviewReasoningFromConfig(t *testing.T) {
	h := newReviewHarness(t)
	h.writeConfig(`
agent = "test"
review_reasoning = "fast"
`)

	err := h.run(runOpts{})
	require.NoError(t, err, "Expected no error, got: %v")

	h.assertOutputContains("reasoning: fast")
}

func TestLocalReviewQuietMode(t *testing.T) {
	h := newReviewHarness(t)

	err := h.run(runOpts{Agent: "test", Reasoning: "fast", Quiet: true})
	require.NoError(t, err, "Expected no error, got: %v")

	h.assertOutputNotContains("Running")
}

func TestLocalReviewSkipsDaemon(t *testing.T) {
	h := newReviewHarness(t)

	t.Setenv("HOME", t.TempDir())

	err := h.run(runOpts{Agent: "test", Reasoning: "fast"})
	require.NoError(t, err, "Expected --local to work without daemon, got: %v")
}

func TestLocalReviewProjectReasoning(t *testing.T) {
	for _, explicit := range []string{"", "high"} {
		t.Run(explicit, func(t *testing.T) {
			h := newReviewHarness(t)
			gitCmd := exec.Command("git", "-C", h.Dir, "remote", "add", "origin", "https://example.com/team/project-a.git")
			gitOutput, err := gitCmd.CombinedOutput()
			require.NoError(t, err, "%s", gitOutput)
			dataDir := testenv.SetDataDir(t)
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
default_agent = "test"
review_model_medium = "medium-model"
review_model_high = "high-model"
[projects."example.com/team/project-a"]
review_reasoning = "medium"
`), 0o600))
			require.NoError(t, h.run(runOpts{Agent: "test", Reasoning: explicit}))
			want := "medium"
			if explicit != "" {
				want = explicit
			}
			h.assertOutputContains("model: " + want + "-model, reasoning: " + want)
		})
	}
}
