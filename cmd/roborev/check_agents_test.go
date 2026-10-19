package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestCheckAgentsHonorsConfiguredCodexCommand(t *testing.T) {
	setupConfigEnv(t, "codex_cmd = 'codex-proxy'\n", "")
	t.Cleanup(testutil.MockExecutable(t, "codex", 1))
	t.Cleanup(testutil.MockExecutable(t, "codex-proxy", 1))

	cmd := checkAgentsCmd()
	cmd.SetArgs([]string{"--agent", "codex", "--timeout", "1"})
	output := captureOutput(t, cmd.Execute)

	assert.Regexp(t, `(?m)^  \? codex\s+codex-proxy \(`, output)
}

func TestCheckAgentsDiscoversConfiguredACPAgent(t *testing.T) {
	setupConfigEnv(t, "[acp.goose]\ncommand = 'missing-goose-acp'\n", "")

	cmd := checkAgentsCmd()
	cmd.SetArgs([]string{"--agent", "acp.goose", "--timeout", "1"})
	output := captureOutput(t, cmd.Execute)

	assert.Regexp(t, `(?m)^  - acp\.goose\s+agent "acp\.goose" command "missing-goose-acp" unavailable:`, output)
	assert.Contains(t, output, "1 skipped")
}

func TestCheckAgentsDiscoversRepoACPAgentFromNestedDirectory(t *testing.T) {
	env := setupConfigEnv(t, "", "[acp.goose]\ncommand = 'missing-nested-goose-acp'\n")
	nested := filepath.Join(env.RepoDir, "nested", "directory")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	t.Chdir(nested)

	cmd := checkAgentsCmd()
	cmd.SetArgs([]string{"--agent", "acp.goose", "--timeout", "1"})
	output := captureOutput(t, cmd.Execute)

	assert.Regexp(t, `(?m)^  - acp\.goose\s+agent "acp\.goose" command "missing-nested-goose-acp" unavailable:`, output)
	assert.Contains(t, output, "1 skipped")
}

func TestCheckAgentsNeverSmokeTestsHost(t *testing.T) {
	for _, tc := range []struct {
		marker, filter string
		detected       bool
		want, notWant  string
		wantErr        bool
	}{
		{marker: "1", want: `(?m)^  - host\s+not smoke-tested`},
		{marker: "1", filter: "host", want: `(?m)^  - host\s+not smoke-tested`},
		{detected: true, notWant: `(?m)^  . host\s`},
		{detected: true, filter: "host", wantErr: true, want: `(?m)^  x host\s+FAIL: agent "host" hands reviews to a host model, but none is attached: set ROBOREV_HOST_AGENT=1`},
		{marker: "0", notWant: `(?m)^  . host\s`},
		{marker: "0", filter: "host", wantErr: true, want: `(?m)^  x host\s+FAIL: agent "host" hands reviews to a host model, but none is attached: set ROBOREV_HOST_AGENT=1`},
	} {
		t.Run(fmt.Sprintf("marker=%s/detected=%v/filter=%s", tc.marker, tc.detected, tc.filter), func(t *testing.T) {
			setupConfigEnv(t, "", "")
			t.Setenv("ROBOREV_HOST_AGENT", tc.marker)
			t.Setenv("CURSOR_AGENT", "")
			if tc.detected {
				t.Setenv("CURSOR_AGENT", "1")
				t.Setenv("CLOUD_AGENT_TEST_DETECTION", "1")
			}
			hostDir := filepath.Join(t.TempDir(), "host")
			t.Setenv("ROBOREV_HOST_DIR", hostDir)
			t.Setenv("ROBOREV_HOST_TIMEOUT", "1h")
			t.Setenv("PATH", t.TempDir())

			cmd := checkAgentsCmd()
			args := []string{"--timeout", "1"}
			if tc.filter != "" {
				args = append(args, "--agent", tc.filter)
			}
			cmd.SetArgs(args)
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			var runErr error
			output := captureOutput(t, func() error { runErr = cmd.Execute(); return runErr })

			if tc.wantErr {
				assert.Error(t, runErr, "an unavailable host must exit nonzero")
			} else {
				assert.NoError(t, runErr)
			}
			if tc.want != "" {
				assert.Regexp(t, tc.want, output)
			}
			if tc.notWant != "" {
				assert.NotRegexp(t, tc.notWant, output)
			}
			assert.NotRegexp(t, `(?m)^  \? host`, output, "host is never smoke-tested")
			assert.NoDirExists(t, hostDir)
		})
	}
}
