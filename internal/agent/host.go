package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"go.kenn.io/kit/atomicfile"

	"go.kenn.io/roborev/internal/config"
)

const (
	hostAgentName = "host"
	// HostMarkerEnv attaches a host model; see HostMarker.Allows.
	HostMarkerEnv           = "ROBOREV_HOST_AGENT"
	hostTimeoutEnv          = "ROBOREV_HOST_TIMEOUT"
	defaultHostTimeout      = 10 * time.Minute
	defaultHostPollInterval = 500 * time.Millisecond
	maxHostJobLabel         = 40
)

// HostAgent hands the review to the model driving the current shell through
// prompt.md, schema.json and instructions.md in a fresh job directory, then
// waits for the host to rename its answer into answer.md.
type HostAgent struct {
	Dir          string
	Timeout      time.Duration
	PollInterval time.Duration
}

func (a *HostAgent) Name() string                       { return hostAgentName }
func (a *HostAgent) WithReasoning(ReasoningLevel) Agent { return a }
func (a *HostAgent) WithAgentic(bool) Agent             { return a }
func (a *HostAgent) WithModel(string) Agent             { return a }
func (a *HostAgent) CommandLine() string                { return "host (file handoff in " + a.baseDir() + ")" }

func (a *HostAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return a.handoff(ctx, repoPath, commitSHA, prompt, nil, output)
}

func (a *HostAgent) ReviewWithSchema(ctx context.Context, repoPath, gitRef, prompt string, schema jsontext.Value, output io.Writer) (jsontext.Value, error) {
	raw, err := a.handoff(ctx, repoPath, gitRef, prompt, schema, output)
	if err != nil {
		return nil, err
	}
	return jsontext.Value(raw), nil
}

func (a *HostAgent) baseDir() string {
	if a.Dir != "" {
		return a.Dir
	}
	if dir := os.Getenv("ROBOREV_HOST_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(config.DataDir(), "host")
}

func (a *HostAgent) timeout() (time.Duration, error) {
	if a.Timeout > 0 {
		return a.Timeout, nil
	}
	raw := strings.TrimSpace(os.Getenv(hostTimeoutEnv))
	if raw == "" {
		return defaultHostTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("host agent: invalid %s %q: want a positive duration such as 10m", hostTimeoutEnv, raw)
	}
	return d, nil
}

func (a *HostAgent) pollInterval() time.Duration {
	if a.PollInterval > 0 {
		return a.PollInterval
	}
	return defaultHostPollInterval
}

var unsafeJobChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// hostJobLabel turns a git ref into a short directory-name prefix that cannot
// escape the handoff directory or hide the job.
func hostJobLabel(gitRef string) string {
	label := unsafeJobChars.ReplaceAllString(gitRef, "_")
	if len(label) > maxHostJobLabel {
		label = label[:maxHostJobLabel]
	}
	label = strings.TrimLeft(label, ".")
	if label == "" {
		return "review"
	}
	return label
}

func (a *HostAgent) handoff(ctx context.Context, repoPath, gitRef, prompt string, schema jsontext.Value, output io.Writer) (string, error) {
	timeout, err := a.timeout()
	if err != nil {
		return "", err
	}
	base := a.baseDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("host agent: create handoff dir: %w", err)
	}
	jobDir, err := os.MkdirTemp(base, hostJobLabel(gitRef)+"-")
	if err != nil {
		return "", fmt.Errorf("host agent: create job dir: %w", err)
	}
	defer os.RemoveAll(jobDir)

	if err := writeHandoffFile(jobDir, "prompt.md", []byte(prompt)); err != nil {
		return "", err
	}
	format := "Plain-text review in the format requested at the end of prompt.md."
	if len(schema) > 0 {
		if err := writeHandoffFile(jobDir, "schema.json", schema); err != nil {
			return "", err
		}
		format = "Exactly one JSON document that validates against schema.json " +
			"(no prose, no code fences). Keep schema_version as given in the schema; " +
			"a fail verdict needs at least one finding."
	}
	instructions := fmt.Sprintf(`# roborev host review handoff

Repository: %s
Git ref: %s

1. Read prompt.md here and review the change it describes, using the repository above.
2. Output format: %s
3. Write the answer to answer.md.tmp, then rename it to answer.md (only a regular answer.md is read).
4. roborev waits %s (the host handoff timeout, %s) for answer.md, then fails and removes this directory.
`, repoPath, gitRef, format, timeout, hostTimeoutEnv)
	if err := writeHandoffFile(jobDir, "instructions.md", []byte(instructions)); err != nil {
		return "", err
	}
	answerPath := filepath.Join(jobDir, "answer.md")
	if output != nil {
		fmt.Fprintf(output, "host agent: handoff written to %s\nhost agent: waiting up to %s for %s\n", jobDir, timeout, answerPath)
	}
	return waitForHostAnswer(ctx, answerPath, timeout, a.pollInterval())
}

// writeHandoffFile publishes name atomically (synced temp file, then rename),
// so the host never reads a partial file. A planted file at name is refused.
func writeHandoffFile(dir, name string, data []byte) error {
	final := filepath.Join(dir, name)
	err := fmt.Errorf("already exists")
	if _, statErr := os.Lstat(final); errors.Is(statErr, os.ErrNotExist) {
		err = writeSyncedTemp(dir, name, data, final)
	}
	if err != nil {
		return fmt.Errorf("host agent: write %s: %w", name, err)
	}
	return nil
}

func writeSyncedTemp(dir, name string, data []byte, final string) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	err = errors.Join(err, f.Sync(), f.Close())
	if err == nil {
		err = atomicfile.RenameNoReplace(f.Name(), final)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// HostTimeoutError is the host handoff timeout, distinct from the job timeout.
type HostTimeoutError struct {
	AnswerPath string
	Timeout    time.Duration
}

func (e *HostTimeoutError) Error() string {
	return fmt.Sprintf("host agent: host handoff timeout: no answer at %s after %s (%s)", e.AnswerPath, e.Timeout, hostTimeoutEnv)
}

func waitForHostAnswer(ctx context.Context, answerPath string, timeout, poll time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		answer, ready, err := readHostAnswer(answerPath)
		if err != nil {
			return "", MarkUnavailable(err)
		}
		if ready {
			return answer, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("host agent: waiting for %s: %w", answerPath, ctx.Err())
		case <-deadline.C:
			if answer, ready, err := readHostAnswer(answerPath); err != nil || ready {
				return answer, MarkUnavailable(err)
			}
			return "", MarkUnavailable(&HostTimeoutError{AnswerPath: answerPath, Timeout: timeout})
		case <-ticker.C:
		}
	}
}

// readHostAnswer reads answer.md once renamed into place; temp files are never
// read, and a symlink or other non-regular file is refused, not followed.
func readHostAnswer(answerPath string) (string, bool, error) {
	info, err := os.Lstat(answerPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err == nil {
		err = notRegular(answerPath, info)
	}
	if err != nil {
		return "", false, err
	}
	beforeHostAnswerOpen(answerPath)
	f, opened, err := openRegularFile(answerPath)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	if !os.SameFile(info, opened) {
		return "", false, fmt.Errorf("host agent: refusing %s: replaced while reading", answerPath)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", false, fmt.Errorf("host agent: read answer: %w", err)
	}
	answer := stripCodeFence(strings.TrimSpace(string(data)))
	if answer == "" {
		return "", false, fmt.Errorf("host agent: %s is empty", answerPath)
	}
	return answer, true, nil
}

// beforeHostAnswerOpen is a test seam between the Lstat and the open.
var beforeHostAnswerOpen = func(string) {}

// openRegularFile opens path without blocking (a FIFO swapped in after the
// Lstat cannot hang the poll loop) and refuses anything but a regular file.
func openRegularFile(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("host agent: read answer: %w", err)
	}
	info, err := f.Stat()
	if err == nil {
		err = notRegular(path, info)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

func notRegular(path string, info os.FileInfo) error {
	if info.Mode().IsRegular() {
		return nil
	}
	return fmt.Errorf("host agent: refusing %s: not a regular file (%s)", path, info.Mode().Type())
}

func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") || !strings.HasSuffix(s, "```") {
		return s
	}
	s = strings.TrimSuffix(s, "```")
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	return strings.TrimSpace(s)
}

// HostMarker says how a host model is attached: Explicit is
// ROBOREV_HOST_AGENT=1 (or true/yes/on, any case).
type HostMarker struct {
	Explicit bool
	Why      string
}

// ReadHostMarker reads the host marker from the environment.
func ReadHostMarker() HostMarker {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(HostMarkerEnv))); v {
	case "1", "true", "yes", "on":
		return HostMarker{Explicit: true, Why: HostMarkerEnv + "=" + v}
	}
	return HostMarker{}
}

// HostUse is how a caller would reach host.
type HostUse int

const (
	HostNamed  HostUse = iota // the primary agent or a named backup
	HostListed                // a listing, completion or health check
	HostAuto                  // auto, auto-selection or a last resort
)

// Allows is the one rule for when host may answer: always ROBOREV_HOST_AGENT=1.
func (m HostMarker) Allows(use HostUse) bool {
	return m.Explicit
}

// HostAllowed is ReadHostMarker().Allows(use).
func HostAllowed(use HostUse) bool { return ReadHostMarker().Allows(use) }

// HostUnavailableError: host was requested but no host model is attached.
// It is deterministic for the environment, so callers must not retry it.
type HostUnavailableError struct{}

func (e *HostUnavailableError) Error() string {
	return fmt.Sprintf("agent %q hands reviews to a host model, but none is attached: set %s=1 in the environment roborev runs in (restart the daemon from it), or choose a CLI agent", hostAgentName, HostMarkerEnv)
}

// IsHostUnavailable reports whether err is a HostUnavailableError.
func IsHostUnavailable(err error) bool {
	_, ok := errors.AsType[*HostUnavailableError](err)
	return ok
}

// requestedHostError fails a request naming host when host is not allowed.
func requestedHostError(name string) error {
	if resolveAlias(strings.TrimSpace(name)) != hostAgentName || HostAllowed(HostNamed) {
		return nil
	}
	return &HostUnavailableError{}
}

// ListedAgentNames lists registered agents to offer; host only with ROBOREV_HOST_AGENT=1.
func ListedAgentNames() []string {
	names := Available()
	if HostAllowed(HostListed) {
		return names
	}
	return slices.DeleteFunc(names, func(name string) bool { return name == hostAgentName })
}

func init() {
	Register(&HostAgent{})
}
