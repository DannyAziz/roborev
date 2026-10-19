package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// setHostMarker forces the explicit host marker on, or leaves no marker of
// either kind: ROBOREV_HOST_AGENT unset and no Cursor cloud detection.
func setHostMarker(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv(HostMarkerEnv, "1")
		return
	}
	unsetForTest(t, HostMarkerEnv)
	unsetForTest(t, "CURSOR_AGENT")
	clearCloudAgentEnv(t)
}

// setDetectedHostOnly simulates a Cursor cloud agent shell with no explicit
// ROBOREV_HOST_AGENT.
func setDetectedHostOnly(t *testing.T) {
	t.Helper()
	unsetForTest(t, HostMarkerEnv)
	t.Setenv("CURSOR_AGENT", "1")
	t.Setenv("CLOUD_AGENT_TEST_DETECTION", "1")
}

type hostMode struct {
	name     string
	set      func(t *testing.T)
	explicit bool
	detected bool
}

// named reports whether host is accepted when chosen by name in this mode.
func (m hostMode) named() bool { return m.explicit || m.detected }

// listed reports whether host is offered in listings in this mode.
func (m hostMode) listed() bool { return m.explicit }

var hostModes = []hostMode{
	{name: "no marker", set: func(t *testing.T) { setHostMarker(t, false) }},
	{name: "marker 0 inside a Cursor cloud agent", set: func(t *testing.T) {
		setDetectedHostOnly(t)
		t.Setenv(HostMarkerEnv, "0")
	}},
	{name: "Cursor cloud agent shell", set: setDetectedHostOnly},
	{name: "explicit marker", set: func(t *testing.T) { setHostMarker(t, true) }, explicit: true},
}

func eachHostMode(t *testing.T, fn func(t *testing.T, mode hostMode)) {
	t.Helper()
	for _, mode := range hostModes {
		t.Run(mode.name, func(t *testing.T) {
			mode.set(t)
			fn(t, mode)
		})
	}
}

// unsetForTest removes key for the duration of t; t.Setenv restores it.
func unsetForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	require.NoError(t, os.Unsetenv(key))
}

func clearCloudAgentEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if key, _, _ := strings.Cut(kv, "="); strings.HasPrefix(key, "CLOUD_AGENT_") {
			unsetForTest(t, key)
		}
	}
}

func stubPath(t *testing.T, commands ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, c := range commands {
		require.NoError(t, os.WriteFile(filepath.Join(dir, c), []byte("#!/bin/sh\n"), 0o755))
	}
	t.Setenv("PATH", dir)
}

func requireHostUnavailable(t *testing.T, label string, a Agent, err error) {
	t.Helper()
	require.Error(t, err, label)
	assert.Nil(t, a, label)
	assert.True(t, IsHostUnavailable(err), "%s: %v", label, err)
	assert.Contains(t, err.Error(), HostMarkerEnv, label)
}

func TestHostUnavailableSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("review: %w", fmt.Errorf("agent: %w", &HostUnavailableError{}))
	assert.True(t, IsHostUnavailable(err))
	var target *HostUnavailableError
	require.ErrorAs(t, err, &target)
	assert.Contains(t, err.Error(), HostMarkerEnv)
	assert.False(t, IsHostUnavailable(errors.New("agent \"host\" unavailable")), "only the typed error counts")
	assert.False(t, IsHostUnavailable(nil))
}

func TestReadHostMarker(t *testing.T) {
	setHostMarker(t, false)
	assert.Equal(t, HostMarker{}, ReadHostMarker())

	setDetectedHostOnly(t)
	assert.Equal(t, HostMarker{}, ReadHostMarker(), "a Cursor cloud agent shell alone attaches nothing")

	for _, v := range []string{"", "  ", "0", "false", "OFF", "no", "bogus"} {
		t.Setenv(HostMarkerEnv, v)
		assert.Equal(t, HostMarker{}, ReadHostMarker(), "%s=%q", HostMarkerEnv, v)
	}
	for _, v := range []string{"1", "true", "TRUE", " Yes ", "on"} {
		t.Setenv(HostMarkerEnv, v)
		m := ReadHostMarker()
		assert.True(t, m.Explicit, "%s=%q", HostMarkerEnv, v)
		assert.Contains(t, m.Why, HostMarkerEnv)
	}
}

// The ruling lives in HostMarker.Allows: every use of host needs
// ROBOREV_HOST_AGENT=1.
func TestHostMarkerAllows(t *testing.T) {
	for _, tc := range []struct {
		marker              HostMarker
		named, listed, auto bool
	}{
		{marker: HostMarker{}},
		{marker: HostMarker{Explicit: true}, named: true, listed: true, auto: true},
	} {
		assert.Equal(t, tc.named, tc.marker.Allows(HostNamed), "%+v named", tc.marker)
		assert.Equal(t, tc.listed, tc.marker.Allows(HostListed), "%+v listed", tc.marker)
		assert.Equal(t, tc.auto, tc.marker.Allows(HostAuto), "%+v automatic pick", tc.marker)
	}
	eachHostMode(t, func(t *testing.T, mode hostMode) {
		assert.Equal(t, mode.named(), HostAllowed(HostNamed))
		assert.Equal(t, mode.listed(), HostAllowed(HostListed))
		assert.Equal(t, mode.explicit, HostAllowed(HostAuto))
	})
}

func TestHostAvailabilityIsMarkerGated(t *testing.T) {
	eachHostMode(t, func(t *testing.T, mode hostMode) {
		assert.Equal(t, mode.named(), IsAvailable("host"))
		assert.Equal(t, mode.named(), isAvailableWithConfig("host", &config.Config{}))
		assert.Equal(t, mode.named(), isAvailableWithConfigFromConfig("host", nil, &config.Config{}))
	})
}

// R1: an explicitly chosen primary host with no marker is a hard error on
// every resolver, and never falls back to another agent.
func TestExplicitHostWithoutMarkerErrors(t *testing.T) {
	setHostMarker(t, false)
	hostDir := filepath.Join(t.TempDir(), "handoff")
	t.Setenv("ROBOREV_HOST_DIR", hostDir)
	stubPath(t, "codex", "claude")
	cfg := &config.Config{}

	a, err := GetPreferredOrBackupWithConfigFromConfig(nil, "host", cfg)
	requireHostUnavailable(t, "strict", a, err)
	a, err = GetPreferredOrBackupWithConfigFromConfig(nil, " host ", cfg, "claude")
	requireHostUnavailable(t, "strict with backup", a, err)
	a, err = GetAvailableWithConfigFromConfig(nil, "host", cfg)
	requireHostUnavailable(t, "non-strict", a, err)
	a, err = GetAvailableWithConfigFromConfig(nil, "host", cfg, "claude")
	requireHostUnavailable(t, "non-strict with backup", a, err)
	a, err = GetAvailableWithConfigFromConfig(nil, "host", nil)
	requireHostUnavailable(t, "non-strict nil config", a, err)
	a, err = GetAvailable("host")
	requireHostUnavailable(t, "GetAvailable", a, err)
	a, err = GetAvailable("host", "codex")
	requireHostUnavailable(t, "GetAvailable with backup", a, err)
	a, err = GetAvailableExactWithConfigFromConfig(nil, "host", cfg)
	requireHostUnavailable(t, "exact", a, err)

	assert.NoDirExists(t, hostDir, "resolution never creates a handoff dir")
}

// An explicitly named host resolves in every mode that allows naming it.
func TestExplicitHostAgent(t *testing.T) {
	eachHostMode(t, func(t *testing.T, mode hostMode) {
		if !mode.named() {
			return
		}
		stubPath(t, "claude")
		cfg := &config.Config{}
		for label, lookup := range map[string]func() (Agent, error){
			"strict":       func() (Agent, error) { return GetPreferredOrBackupWithConfigFromConfig(nil, "host", cfg) },
			"non-strict":   func() (Agent, error) { return GetAvailableWithConfigFromConfig(nil, "host", cfg) },
			"GetAvailable": func() (Agent, error) { return GetAvailable("host") },
			"exact":        func() (Agent, error) { return GetAvailableExactWithConfigFromConfig(nil, "host", cfg) },
		} {
			a, err := lookup()
			require.NoError(t, err, label)
			assert.Equal(t, "host", a.Name(), label)
		}
	})
}

// R4: a strict lookup for a named agent never answers with host unless host
// itself was named, whatever the marker.
func TestStrictNamedAgentNeverReturnsHost(t *testing.T) {
	eachHostMode(t, func(t *testing.T, mode hostMode) {
		stubPath(t)
		cfg := &config.Config{ACP: config.ACPAgentConfigs{"remote": {Command: "missing-acp"}}}

		for _, tc := range []struct {
			name      string
			preferred string
			backups   []string
		}{
			{name: "named cli", preferred: "codex"},
			{name: "named cli with backup", preferred: "codex", backups: []string{"claude"}},
			{name: "named ACP", preferred: "acp.remote"},
			{name: "named ACP with backup", preferred: "acp.remote", backups: []string{"gemini"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, err := GetPreferredOrBackupWithConfigFromConfig(nil, tc.preferred, cfg, tc.backups...)
				require.Error(t, err)
				assert.Nil(t, a)
				assert.Contains(t, err.Error(), "no configured agent available")
			})
		}

		stubPath(t, "claude")
		a, err := GetPreferredOrBackupWithConfigFromConfig(nil, "codex", cfg, "claude")
		require.NoError(t, err)
		assert.Equal(t, "claude-code", a.Name(), "a named backup still wins over host")
	})
}

// R3: host is never picked by auto-selection and is listed only with
// ROBOREV_HOST_AGENT=1, not on Cursor cloud detection alone. Every
// UnknownAgentError known-list follows the same rule.
func TestHostHiddenFromSelectionAndListings(t *testing.T) {
	eachHostMode(t, func(t *testing.T, mode hostMode) {
		marker := mode.listed()
		stubPath(t, "claude")
		cfg := &config.Config{}
		a, err := GetAvailableWithConfigFromConfig(nil, "", cfg)
		require.NoError(t, err)
		assert.Equal(t, "claude-code", a.Name())
		a, err = GetAvailable("")
		require.NoError(t, err)
		assert.Equal(t, "claude-code", a.Name())
		a, err = GetAvailableWithConfigFromConfig(nil, "codex", cfg)
		require.NoError(t, err)
		assert.Equal(t, "claude-code", a.Name())

		assert.Equal(t, marker, contains(AvailableNamesFromConfig(nil, cfg), "host"))
		for label, lookup := range map[string]func() error{
			"GetAvailable": func() error { _, err := GetAvailable("bogus"); return err },
			"non-strict":   func() error { _, err := GetAvailableWithConfigFromConfig(nil, "bogus", cfg); return err },
			"strict":       func() error { _, err := GetPreferredOrBackupWithConfigFromConfig(nil, "bogus", cfg); return err },
			"exact":        func() error { _, err := GetAvailableExactWithConfigFromConfig(nil, "bogus", cfg); return err },
		} {
			var unknown *UnknownAgentError
			require.ErrorAs(t, lookup(), &unknown, label)
			assert.Equal(t, marker, contains(unknown.Known, "host"), "%s known list: %v", label, unknown.Known)
			assert.Contains(t, unknown.Known, "claude-code", label)
		}
	})
}

func contains(names []string, want string) bool {
	return slices.Contains(names, want)
}

func hostTestAgent(dir string, timeout time.Duration) *HostAgent {
	return &HostAgent{Dir: dir, Timeout: timeout, PollInterval: 5 * time.Millisecond}
}

// handoffSeen is what a test host read from one handoff directory.
type handoffSeen struct {
	dir, prompt, schema, instructions string
}

// fakeHost answers every handoff that appears under dir until the test ends.
// It follows the host contract: instructions.md is published last, so once it
// exists the handoff is complete; the answer is written to answer.md.tmp and
// then renamed into place. Each answered handoff is sent on the channel.
func fakeHost(t *testing.T, dir string, answer func(handoffSeen) string) <-chan handoffSeen {
	t.Helper()
	seen := make(chan handoffSeen, 16)
	done := make(chan struct{})
	stopped := make(chan struct{})
	t.Cleanup(func() { close(done); <-stopped })
	go func() {
		defer close(stopped)
		answered := map[string]bool{}
		for {
			matches, _ := filepath.Glob(filepath.Join(dir, "*", "instructions.md"))
			for _, m := range matches {
				d := filepath.Dir(m)
				if answered[d] {
					continue
				}
				answered[d] = true
				h := handoffSeen{dir: d}
				h.instructions = readFileString(m)
				h.prompt = readFileString(filepath.Join(d, "prompt.md"))
				h.schema = readFileString(filepath.Join(d, "schema.json"))
				tmp := filepath.Join(d, "answer.md.tmp")
				if os.WriteFile(tmp, []byte(answer(h)), 0o600) == nil {
					_ = os.Rename(tmp, filepath.Join(d, "answer.md"))
				}
				seen <- h
			}
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond): //nolint:kennlint // polls for handoff files the agent under test writes; not a deadline assertion
			}
		}
	}()
	return seen
}

func readFileString(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func answerWith(answer string) func(handoffSeen) string {
	return func(handoffSeen) string { return answer }
}

func TestHostAgentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := hostTestAgent(dir, 5*time.Second)
	schema := jsontext.Value(`{"type":"object"}`)
	answer := `{"schema_version":2,"verdict":"pass","summary":"ok","findings":[]}`

	handoffs := fakeHost(t, dir, answerWith("```json\n"+answer+"\n```\n"))

	var out strings.Builder
	raw, err := a.ReviewWithSchema(context.Background(), "/work/repo", "abc..def", "REVIEW PROMPT", schema, &out)
	require.NoError(t, err)
	assert.JSONEq(t, answer, string(raw))

	seen := <-handoffs
	jobDir := seen.dir
	assert.True(t, strings.HasPrefix(filepath.Base(jobDir), "abc..def-"), jobDir)
	assert.Equal(t, "REVIEW PROMPT", seen.prompt)
	assert.JSONEq(t, string(schema), seen.schema)
	assert.Contains(t, seen.instructions, "Repository: /work/repo\n")
	assert.Contains(t, seen.instructions, "Git ref: abc..def\n")
	assert.Contains(t, seen.instructions, "schema.json")
	assert.Contains(t, seen.instructions, "host handoff timeout")
	assert.Contains(t, out.String(), "waiting up to 5s")
	assert.NoDirExists(t, jobDir, "handoff dir is removed after success")
}

func TestHostAgentPlainReviewPassesRepoPath(t *testing.T) {
	dir := t.TempDir()
	a := hostTestAgent(dir, 5*time.Second)
	repo := t.TempDir()
	handoffs := fakeHost(t, dir, answerWith("LGTM\n"))
	got, err := a.Review(context.Background(), repo, "HEAD", "p", nil)
	require.NoError(t, err)
	assert.Equal(t, "LGTM", got)
	instr := (<-handoffs).instructions
	assert.Contains(t, instr, "Repository: "+repo+"\n")
	assert.NotContains(t, instr, "schema.json")
}

func TestHostAgentTimeout(t *testing.T) {
	dir := t.TempDir()
	a := hostTestAgent(dir, 40*time.Millisecond)
	start := time.Now()
	_, err := a.Review(context.Background(), "/repo", "HEAD", "p", nil)
	require.Error(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)

	var timeoutErr *HostTimeoutError
	require.ErrorAs(t, err, &timeoutErr)
	assert.Equal(t, 40*time.Millisecond, timeoutErr.Timeout)
	assert.Contains(t, err.Error(), "host handoff timeout")
	assert.Contains(t, err.Error(), "after 40ms")
	assert.True(t, IsUnavailable(err), "host timeout is non-retryable")
	require.NotErrorIs(t, err, context.DeadlineExceeded, "not reported as the job timeout")

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "handoff dir is removed after failure")
}

// The handoff timeout bounds the wait even when the poll interval is longer,
// and an answer that lands before the deadline is still read.
func TestHostAgentPollBoundaries(t *testing.T) {
	dir := t.TempDir()
	a := &HostAgent{Dir: dir, Timeout: 60 * time.Millisecond, PollInterval: time.Hour}
	fakeHost(t, dir, answerWith("late but in time"))
	got, err := a.Review(context.Background(), "/repo", "HEAD", "p", nil)
	require.NoError(t, err)
	assert.Equal(t, "late but in time", got)

	start := time.Now()
	_, err = (&HostAgent{Dir: t.TempDir(), Timeout: 30 * time.Millisecond, PollInterval: time.Hour}).Review(context.Background(), "/repo", "HEAD", "p", nil)
	require.ErrorAs(t, err, new(*HostTimeoutError))
	assert.Less(t, time.Since(start), 5*time.Second)

	assert.Equal(t, defaultHostPollInterval, (&HostAgent{}).pollInterval())
	assert.Equal(t, defaultHostPollInterval, (&HostAgent{PollInterval: -1}).pollInterval())
	assert.Equal(t, time.Millisecond, (&HostAgent{PollInterval: time.Millisecond}).pollInterval())
}

func TestHostAgentTimeoutFromEnv(t *testing.T) {
	unsetForTest(t, hostTimeoutEnv)
	d, err := (&HostAgent{}).timeout()
	require.NoError(t, err)
	assert.Equal(t, defaultHostTimeout, d)

	t.Setenv(hostTimeoutEnv, " 90s ")
	d, err = (&HostAgent{}).timeout()
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, d)
	d, err = (&HostAgent{Timeout: time.Second}).timeout()
	require.NoError(t, err)
	assert.Equal(t, time.Second, d, "explicit timeout wins over the environment")

	for _, bad := range []string{"bogus", "0", "0s", "-5s", "10"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv(hostTimeoutEnv, bad)
			_, err := (&HostAgent{}).timeout()
			require.Error(t, err)
			assert.Contains(t, err.Error(), hostTimeoutEnv)

			dir := filepath.Join(t.TempDir(), "handoff")
			_, err = (&HostAgent{Dir: dir}).Review(context.Background(), "/repo", "HEAD", "p", nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid "+hostTimeoutEnv)
			assert.NoDirExists(t, dir, "invalid timeout fails before any handoff dir exists")
		})
	}
}

func TestHostAgentIgnoresTempAndRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	answerPath := filepath.Join(dir, "answer.md")
	require.NoError(t, os.WriteFile(answerPath+".tmp", []byte("partial"), 0o600))
	_, ready, err := readHostAnswer(answerPath)
	require.NoError(t, err)
	assert.False(t, ready, "answer.md.tmp is never read")

	require.NoError(t, os.WriteFile(answerPath, []byte("  \n"), 0o600))
	_, _, err = readHostAnswer(answerPath)
	require.Error(t, err)
	assert.True(t, strings.HasSuffix(err.Error(), "is empty"))

	require.NoError(t, os.Remove(answerPath))
	require.NoError(t, os.Mkdir(answerPath, 0o700))
	_, _, err = readHostAnswer(answerPath)
	require.ErrorContains(t, err, "not a regular file")

	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	require.NoError(t, os.Remove(answerPath))
	target := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(target, []byte("do not read"), 0o600))
	require.NoError(t, os.Symlink(target, answerPath))
	_, _, err = readHostAnswer(answerPath)
	require.ErrorContains(t, err, "not a regular file")

	_, err = waitForHostAnswer(context.Background(), answerPath, time.Second, time.Millisecond)
	require.ErrorContains(t, err, "not a regular file")
	assert.True(t, IsUnavailable(err))
}

// A file swapped in between the Lstat and the open is refused.
func TestHostAnswerReplacedBeforeOpenIsRefused(t *testing.T) {
	dir := t.TempDir()
	answerPath := filepath.Join(dir, "answer.md")
	require.NoError(t, os.WriteFile(answerPath, []byte("first"), 0o600))
	prev := beforeHostAnswerOpen
	t.Cleanup(func() { beforeHostAnswerOpen = prev })
	beforeHostAnswerOpen = func(path string) {
		other := path + ".other"
		require.NoError(t, os.WriteFile(other, []byte("second"), 0o600))
		require.NoError(t, os.Rename(other, path))
	}
	_, ready, err := readHostAnswer(answerPath)
	require.ErrorContains(t, err, "replaced while reading")
	assert.False(t, ready)
}

// A FIFO at answer.md never blocks the reader: it is refused on open.
func TestHostAnswerFIFOIsRefusedWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on Windows")
	}
	fifo := filepath.Join(t.TempDir(), "answer.md")
	require.NoError(t, mkfifo(fifo))
	done := make(chan error, 1)
	go func() {
		f, _, err := openRegularFile(fifo)
		if f != nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("opening a FIFO answer blocked")
	}

	prev := beforeHostAnswerOpen
	t.Cleanup(func() { beforeHostAnswerOpen = prev })
	answerPath := filepath.Join(t.TempDir(), "answer.md")
	require.NoError(t, os.WriteFile(answerPath, []byte("x"), 0o600))
	beforeHostAnswerOpen = func(path string) {
		require.NoError(t, os.Remove(path))
		require.NoError(t, mkfifo(path))
	}
	go func() {
		_, _, err := readHostAnswer(answerPath)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "not a regular file")
	case <-time.After(5 * time.Second):
		t.Fatal("a FIFO swapped in after the Lstat blocked the reader")
	}
}

// A read error during polling fails the wait at once instead of waiting out
// the handoff timeout.
func TestHostAnswerErrorFailsBeforeDeadline(t *testing.T) {
	answerPath := filepath.Join(t.TempDir(), "answer.md")
	require.NoError(t, os.Mkdir(answerPath, 0o700))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := waitForHostAnswer(ctx, answerPath, time.Hour, time.Millisecond)
	require.ErrorContains(t, err, "not a regular file")
	assert.True(t, IsUnavailable(err))
}

// A read error found by the final check at the deadline is reported, not
// masked as a timeout.
func TestHostAnswerErrorAtDeadlineIsReported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	answerPath := filepath.Join(t.TempDir(), "answer.md")
	target := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(target, []byte("do not read"), 0o600))
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.Symlink(target, answerPath)
	}()
	_, err := waitForHostAnswer(context.Background(), answerPath, 200*time.Millisecond, time.Hour)
	require.ErrorContains(t, err, "not a regular file")
	require.NotErrorAs(t, err, new(*HostTimeoutError), "reported as the read error, not a timeout")
	assert.True(t, IsUnavailable(err))
}

// Handoff files appear complete or not at all, and no temp file is left.
func TestHandoffFilesArePublishedAtomically(t *testing.T) {
	data := []byte(strings.Repeat("x", 4<<20))
	for i := range 20 {
		dir := t.TempDir()
		name := fmt.Sprintf("prompt-%d.md", i)
		stop := make(chan struct{})
		partial := make(chan int, 1)
		go func() {
			for {
				select {
				case <-stop:
					close(partial)
					return
				default:
				}
				if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil && len(b) != len(data) {
					partial <- len(b)
					return
				}
			}
		}()
		require.NoError(t, writeHandoffFile(dir, name, data))
		close(stop)
		if n, ok := <-partial; ok {
			t.Fatalf("reader saw a partial %s (%d of %d bytes)", name, n, len(data))
		}
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Len(t, entries, 1, "only the published file remains")
		assert.Equal(t, name, entries[0].Name())
	}
}

func TestHostAgentRefusesPlantedHandoffFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(target, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(target, filepath.Join(dir, "prompt.md")))
	err := writeHandoffFile(dir, "prompt.md", []byte("overwrite"))
	require.Error(t, err)
	got, _ := os.ReadFile(target)
	assert.Equal(t, "keep", string(got))
	info, err := os.Lstat(filepath.Join(dir, "prompt.md"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the planted symlink is not replaced")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "schema.json"), []byte("planted"), 0o600))
	require.Error(t, writeHandoffFile(dir, "schema.json", []byte("{}")))
	assert.Equal(t, "planted", readFileString(filepath.Join(dir, "schema.json")))
}

func TestHostAgentCancel(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := hostTestAgent(dir, time.Minute).Review(ctx, "/repo", "HEAD", "p", nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, IsUnavailable(err), "cancellation is not a host failure")
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries)
}

func TestHostJobLabel(t *testing.T) {
	for in, want := range map[string]string{
		"abc123":                "abc123",
		"abc..def":              "abc..def",
		"../../etc/passwd":      "_.._etc_passwd",
		"..":                    "review",
		"":                      "review",
		"feature/x y":           "feature_x_y",
		`C:\windows\system`:     "C_windows_system",
		".hidden":               "hidden",
		strings.Repeat("a", 60): strings.Repeat("a", maxHostJobLabel),
	} {
		assert.Equal(t, want, hostJobLabel(in), "label for %q", in)
	}
}

func TestHostHandoffDirsAreUniqueAndContained(t *testing.T) {
	dir := t.TempDir()
	const n = 4
	handoffs := fakeHost(t, dir, answerWith("ok"))
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			out, err := hostTestAgent(dir, 5*time.Second).Review(context.Background(), "/repo", "../../escape", "p", nil)
			require.NoError(t, err)
			assert.Equal(t, "ok", out)
		})
	}
	wg.Wait()
	got := map[string]bool{}
	for range n {
		got[(<-handoffs).dir] = true
	}
	assert.Len(t, got, n, "same-ref handoffs never share a dir")
	for d := range got {
		assert.Equal(t, dir, filepath.Dir(d), "job dir stays inside the handoff dir")
	}
	parent, _ := os.ReadDir(filepath.Dir(dir))
	for _, e := range parent {
		assert.NotEqual(t, "escape", e.Name())
	}
}
