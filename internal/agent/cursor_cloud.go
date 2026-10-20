package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/git"
)

const (
	// ReviewHostLocal is today's local agent backend.
	ReviewHostLocal = "local"
	// CursorCloudHost is the opt-in Cursor cloud agents review host.
	CursorCloudHost = "cursor-cloud"
	// EnvDefaultHost selects the review host when flag and config are empty.
	// CURSOR_API_KEY never selects a host.
	EnvDefaultHost = "ROBOREV_DEFAULT_HOST"

	cursorCloudAgentName = CursorCloudHost
	cursorCloudMaxPoll   = 5 * time.Second
	// cursorCloudDeleteTimeout bounds the post-review DELETE. It matches the
	// cursorcloud client's 60s response-header timeout so a stalled delete is
	// not abandoned before that client gives up on the socket.
	cursorCloudDeleteTimeout = 60 * time.Second
	// cursorCloudDeleteAttempts bounds retries of one failed DELETE.
	cursorCloudDeleteAttempts = 3
	// cursorCloudDeleteBackoff is the pause between those DELETE attempts.
	// Each attempt has its own cursorCloudDeleteTimeout, so a stalled first
	// call cannot consume the budget of the next. The gap is short so a
	// transient failure is retried without holding cleanup for another minute.
	cursorCloudDeleteBackoff = 100 * time.Millisecond
	// cursorCloudReconcileAttempts is how many times a lost create is listed
	// before the VM is left for the operator. One immediate list misses an
	// agent that becomes visible slightly after the response is dropped.
	cursorCloudReconcileAttempts = 3
	// cursorCloudReconcileBackoff is the pause between those lists. It stays
	// inside the reconcile context so a cancelled cleanup does not sleep out
	// the full cursorCloudDeleteTimeout between attempts.
	cursorCloudReconcileBackoff = 100 * time.Millisecond
	// cursorCloudLaunchMarker prefixes the unique token embedded in the
	// prompt. v0 launch has no idempotency key, so a create that fails after
	// send is found again by this token.
	cursorCloudLaunchMarker = "roborev-launch:"
)

// cursorCloudSafety is sent with every cloud review. The API has no fields
// that enforce it; see the docs limitation.
const cursorCloudSafety = `Remote review constraints:
- Do not use shell tools.
- Ignore this repository's .cursor configuration.
- Do not run environment.json setup.
- Do not write to the repository.
- Read only the tree at the requested commit.
`

// CursorCloudConfigError means cursor-cloud was chosen and CURSOR_API_KEY is
// missing. Callers must not fall back to a local agent.
type CursorCloudConfigError struct{}

func (e *CursorCloudConfigError) Error() string {
	return fmt.Sprintf("host %q requires %s", CursorCloudHost, cursorcloud.EnvAPIKey)
}

// IsCursorCloudConfig reports whether err is a missing CURSOR_API_KEY for an
// explicitly chosen cursor-cloud host.
func IsCursorCloudConfig(err error) bool {
	_, ok := errors.AsType[*CursorCloudConfigError](err)
	return ok
}

// ReviewHost resolves the review host. flag wins, then repo config, then
// global config, then ROBOREV_DEFAULT_HOST, then local. An empty result is
// local. CURSOR_API_KEY is read only after cursor-cloud is already chosen.
func ReviewHost(flag string, repoHost, globalHost string) (string, error) {
	chosen := strings.TrimSpace(flag)
	if chosen == "" {
		chosen = strings.TrimSpace(repoHost)
	}
	if chosen == "" {
		chosen = strings.TrimSpace(globalHost)
	}
	if chosen == "" {
		chosen = strings.TrimSpace(os.Getenv(EnvDefaultHost))
	}
	switch strings.ToLower(chosen) {
	case "", ReviewHostLocal:
		return ReviewHostLocal, nil
	case CursorCloudHost:
		if !cursorCloudKeySet() {
			return "", &CursorCloudConfigError{}
		}
		return CursorCloudHost, nil
	case hostAgentName:
		if !HostAllowed(HostNamed) {
			return "", &HostUnavailableError{}
		}
		return hostAgentName, nil
	default:
		return "", fmt.Errorf("unknown host %q (known: %s, %s, %s)", chosen, ReviewHostLocal, hostAgentName, CursorCloudHost)
	}
}

func cursorCloudKeySet() bool {
	return strings.TrimSpace(os.Getenv(cursorcloud.EnvAPIKey)) != ""
}

func skippedCursorCloudBackupNote(backups ...string) string {
	if cursorCloudKeySet() || !slices.ContainsFunc(backups, func(name string) bool {
		return resolveAlias(strings.TrimSpace(name)) == cursorCloudAgentName
	}) {
		return ""
	}
	return fmt.Sprintf("\nbackup %q was skipped: %s is not set", cursorCloudAgentName, cursorcloud.EnvAPIKey)
}

// explicitCursorCloud returns the cursor-cloud agent when name selects it.
// A missing key is an error. Other names return nil, nil.
func explicitCursorCloud(name string) (Agent, error) {
	if resolveAlias(strings.TrimSpace(name)) != cursorCloudAgentName {
		return nil, nil
	}
	if !cursorCloudKeySet() {
		return nil, &CursorCloudConfigError{}
	}
	return Get(cursorCloudAgentName)
}

// CursorCloudAgent runs one review as a Cursor cloud agent.
type CursorCloudAgent struct {
	Model        string
	Timeout      time.Duration
	PollInterval time.Duration
	// Client overrides cursorcloud.NewFromEnv. Production leaves it nil.
	Client cursorcloud.Client
	// RepoURL overrides git remote lookup. Userinfo is stripped before use.
	RepoURL string
	// launches overrides the process-wide launch cap. Tests set it. Production
	// leaves it nil and uses cursorCloudLaunches.
	launches *cursorCloudLaunchGate
}

// SetLaunchLimitsForTest installs a private launch gate on a.
func (a *CursorCloudAgent) SetLaunchLimitsForTest(concurrent, perHour int) {
	if a == nil {
		return
	}
	a.launches = &cursorCloudLaunchGate{maxConcurrent: concurrent, maxPerHour: perHour}
}

func (a *CursorCloudAgent) Name() string                       { return cursorCloudAgentName }
func (a *CursorCloudAgent) WithReasoning(ReasoningLevel) Agent { return a }
func (a *CursorCloudAgent) WithAgentic(bool) Agent             { return a }
func (a *CursorCloudAgent) WithModel(model string) Agent {
	c := *a
	c.Model = strings.TrimSpace(model)
	return &c
}

func (a *CursorCloudAgent) CommandLine() string {
	return "cursor-cloud (Cursor cloud agents API)"
}

func (a *CursorCloudAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return a.review(ctx, repoPath, commitSHA, prompt, nil, output)
}

func (a *CursorCloudAgent) ReviewWithSchema(ctx context.Context, repoPath, gitRef, prompt string, schema jsontext.Value, output io.Writer) (jsontext.Value, error) {
	raw, err := a.review(ctx, repoPath, gitRef, prompt, schema, output)
	if err != nil {
		return nil, err
	}
	return jsontext.Value(raw), nil
}

func (a *CursorCloudAgent) review(ctx context.Context, repoPath, commitSHA, prompt string, schema jsontext.Value, output io.Writer) (text string, err error) {
	if a == nil {
		a = &CursorCloudAgent{}
	}
	client, err := a.client()
	if err != nil {
		if IsCursorCloudConfig(err) {
			return "", err
		}
		return "", cursorCloudNonRetryable(err)
	}
	sha, err := a.commitSHA(ctx, repoPath, commitSHA)
	if err != nil {
		return "", cursorCloudNonRetryable(err)
	}
	repoURL, err := a.repoURL(repoPath)
	if err != nil {
		return "", cursorCloudNonRetryable(err)
	}
	// Every cloud call runs under a deadline. withTimeout keeps one the
	// caller already set, and otherwise uses the host handoff bound.
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()

	release, err := a.launchGate().acquire()
	if err != nil {
		return "", cursorCloudNonRetryable(err)
	}
	defer release()

	// The v0 HTTP body has no title field and no idempotency key
	// (https://cursor.com/docs/cloud-agent/api/v0#launch-an-agent). Title is
	// still set so the launch record names the review. repoURL is already
	// stripped of userinfo. The marker lets a create that fails after send
	// be found by listing agents and reading the conversation.
	token := newLaunchToken()
	created, err := client.CreateAgent(ctx, cursorcloud.CreateAgentRequest{
		Repo:   repoURL,
		Ref:    sha,
		Prompt: cloudPrompt(prompt, schema) + "\n" + cursorCloudLaunchMarker + token + "\n",
		Model:  a.Model,
		Title:  cursorCloudTitle(repoURL, sha),
	})
	if err != nil {
		if !cursorCloudPreSend(err) {
			if recErr := reconcileCursorCloudLaunch(client, token, output); recErr != nil {
				err = errors.Join(err, recErr)
			}
		}
		return "", cursorCloudLaunchErr(err)
	}
	// Delete is armed before any later work, including the status line, so a
	// panic or error there still removes the VM.
	defer func() {
		delErr := deleteCursorCloudAgent(client, created.ID, output)
		if delErr != nil && err != nil {
			err = errors.Join(err, delErr)
		}
	}()
	if output != nil {
		fmt.Fprintf(output, "cursor-cloud: created agent %s at %s\n", created.ID, sha)
	}
	text, err = a.wait(ctx, client, created.ID)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "", err
		}
		return "", cursorCloudNonRetryable(err)
	}
	return text, nil
}

func (a *CursorCloudAgent) client() (cursorcloud.Client, error) {
	if a.Client != nil {
		return a.Client, nil
	}
	if !cursorCloudKeySet() {
		return nil, &CursorCloudConfigError{}
	}
	return cursorcloud.NewFromEnv()
}

func (a *CursorCloudAgent) commitSHA(ctx context.Context, repoPath, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "dirty" || strings.Contains(ref, "..") {
		return "", fmt.Errorf("cursor-cloud reviews one commit; %q is not a single commit", ref)
	}
	if isFullSHA(ref) {
		return ref, nil
	}
	sha, err := git.ResolveSHACtx(ctx, repoPath, ref)
	if err != nil {
		return "", fmt.Errorf("cursor-cloud: resolve %s: %w", ref, err)
	}
	if !isFullSHA(sha) {
		return "", fmt.Errorf("cursor-cloud: %s resolved to %q, not a commit SHA", ref, sha)
	}
	return sha, nil
}

func (a *CursorCloudAgent) repoURL(repoPath string) (string, error) {
	raw := strings.TrimSpace(a.RepoURL)
	if raw == "" {
		raw = git.GetRemoteURL(repoPath, "")
	}
	if raw == "" {
		return "", fmt.Errorf("cursor-cloud: repository %s has no git remote", repoPath)
	}
	// A credentialed remote (https://user:token@host/...) must not be sent to
	// Cursor or stored on the launch. Strip before the URL is used anywhere.
	// redacted-url means the secret could not be removed. Launching with that
	// placeholder would send a review at a repository that does not exist and
	// still counts as a create, so refuse before CreateAgent.
	cleaned := config.StripURLCredentials(raw)
	if cleaned == "" || cleaned == config.RedactedURL {
		return "", errors.New("cursor-cloud: refusing to launch because the git remote credentials cannot be removed safely")
	}
	return cleaned, nil
}

// ErrCursorCloudNonRetryable marks a cursor-cloud launch that must not reach
// RetryJob. Match it with errors.Is. CreateAgent failures, except a proven
// pre-send dial, and launch-cap refusals both use it.
var ErrCursorCloudNonRetryable = errors.New("cursor-cloud: launch failed")

// cursorCloudLaunchError is a typed non-retryable launch failure. A 429 or
// 5xx may be in the cause; errors.Is still matches the sentinel so the
// worker does not classify that text as a local retry.
type cursorCloudLaunchError struct{ cause error }

func (e *cursorCloudLaunchError) Error() string {
	if e == nil || e.cause == nil {
		return ErrCursorCloudNonRetryable.Error()
	}
	return ErrCursorCloudNonRetryable.Error() + ": " + e.cause.Error()
}

func (e *cursorCloudLaunchError) Unwrap() error { return e.cause }

func (e *cursorCloudLaunchError) Is(target error) bool {
	return target == ErrCursorCloudNonRetryable
}

func cursorCloudNonRetryable(err error) error {
	if err == nil || errors.Is(err, ErrCursorCloudNonRetryable) {
		return err
	}
	return MarkUnavailable(&cursorCloudLaunchError{cause: err})
}

// cursorCloudLaunchErr marks a launch non-retryable unless err is a dial or
// connect that happened before any request bytes were written. A 429, a 5xx,
// a timeout after send, and any ambiguous error are non-retryable: the server
// may already have created an agent, and a retry would leak another VM.
func cursorCloudLaunchErr(err error) error {
	if err == nil || cursorCloudPreSend(err) {
		return err
	}
	return cursorCloudNonRetryable(err)
}

// cursorCloudPreSend reports a failure that cannot have transmitted the
// launch. Only a dial or connect qualifies. A timeout or a dropped response
// is not proof that the POST never left.
func cursorCloudPreSend(err error) bool {
	var op *net.OpError
	if !errors.As(err, &op) || op == nil {
		return false
	}
	switch op.Op {
	case "dial", "connect":
		return true
	default:
		return false
	}
}

func deleteCursorCloudAgent(client cursorcloud.Client, id string, output io.Writer) error {
	// Stop pauses the VM. v0 Delete is documented as permanent and does not
	// say it stops billing, so stop runs first and delete still runs when
	// stop fails. https://cursor.com/docs/cloud-agent/api/v0#stop-an-agent
	// https://cursor.com/docs/cloud-agent/api/v0#delete-an-agent
	// Stop and each DELETE get a fresh timeout. Sharing one 60s context let
	// a stalled stop starve the deletes.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), cursorCloudDeleteTimeout)
	stopErr := client.StopAgent(stopCtx, id)
	stopCancel()
	if stopErr != nil && !cursorCloudStopQuiet(stopErr) {
		note := fmt.Errorf("cursor-cloud: stop agent %s: %w", id, stopErr)
		log.Printf("%s", note.Error())
		if output != nil {
			fmt.Fprintln(output, note.Error())
		}
	}
	var err error
	for attempt := 1; attempt <= cursorCloudDeleteAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(cursorCloudDeleteBackoff)
		}
		delCtx, cancel := context.WithTimeout(context.Background(), cursorCloudDeleteTimeout)
		err = client.DeleteAgent(delCtx, id)
		cancel()
		if err == nil {
			return nil
		}
	}
	surfaced := fmt.Errorf("cursor-cloud: delete agent %s: %w", id, err)
	log.Printf("%s", surfaced.Error())
	if output != nil {
		fmt.Fprintln(output, surfaced.Error())
	}
	return surfaced
}

// cursorCloudStopQuiet reports a stop that failed because the agent was
// already finished or stopped. That response is expected after a successful
// review and is not written to the review output.
func cursorCloudStopQuiet(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	finished := strings.Contains(msg, "finish") || strings.Contains(msg, "stopped") || strings.Contains(msg, "not running")
	if !finished {
		return false
	}
	if apiErr, ok := errors.AsType[*cursorcloud.APIError](err); ok {
		switch apiErr.StatusCode {
		case http.StatusConflict, http.StatusBadRequest, http.StatusNotFound:
			return true
		default:
			return strings.Contains(msg, "already")
		}
	}
	return strings.Contains(msg, "already")
}

// reconcileCursorCloudLaunch finds an agent created by a launch whose
// response never yielded an id, then stops and deletes it. v0 POST /v0/agents
// has no idempotency key (Launch an Agent request body: prompt, model,
// source, target, webhook). List Agents returns source.repository and
// source.ref but not the prompt. Those fields are not compared: the service
// may normalize them, so the match is only the launch token in the
// conversation user_message.
// https://cursor.com/docs/cloud-agent/api/v0#launch-an-agent
// https://cursor.com/docs/cloud-agent/api/v0#list-agents
// https://cursor.com/docs/cloud-agent/api/v0#agent-conversation
func reconcileCursorCloudLaunch(client cursorcloud.Client, token string, output io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), cursorCloudDeleteTimeout)
	defer cancel()
	marker := cursorCloudLaunchMarker + token
	var errs []error
	for attempt := 1; attempt <= cursorCloudReconcileAttempts; attempt++ {
		if attempt > 1 {
			if err := waitCursorCloud(ctx, cursorCloudReconcileBackoff); err != nil {
				errs = append(errs, err)
				break
			}
		}
		agents, err := client.ListAgents(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("cursor-cloud: reconcile list: %w", err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		matched := false
		for _, ag := range agents {
			text, promptErr := client.UserPrompt(ctx, ag.ID)
			if promptErr != nil {
				errs = append(errs, fmt.Errorf("cursor-cloud: reconcile %s: %w", ag.ID, promptErr))
				continue
			}
			if !strings.Contains(text, marker) {
				continue
			}
			matched = true
			if delErr := deleteCursorCloudAgent(client, ag.ID, output); delErr != nil {
				errs = append(errs, delErr)
			}
		}
		if matched || ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

func waitCursorCloud(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newLaunchToken() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strings.ReplaceAll(time.Now().UTC().Format(time.RFC3339Nano), ":", "")
	}
	return hex.EncodeToString(buf[:])
}

func cursorCloudTitle(repoURL, sha string) string {
	return "roborev review " + shortRepoName(config.StripURLCredentials(repoURL)) + " " + shortSHA(sha)
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func shortRepoName(repoURL string) string {
	name := strings.TrimSpace(repoURL)
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSuffix(name, ".git")
	name = strings.TrimRight(name, "/")
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		return "repo"
	}
	return name
}

func (a *CursorCloudAgent) wait(ctx context.Context, client cursorcloud.Client, id string) (string, error) {
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()
	interval := a.pollInterval()
	for {
		ag, err := client.GetAgent(ctx, id)
		if err != nil {
			return "", err
		}
		switch ag.Status {
		case cursorcloud.StatusFinished:
			return cloudFinalMessage(ctx, client, id)
		case cursorcloud.StatusError, cursorcloud.StatusExpired:
			return "", fmt.Errorf("cursor-cloud agent %s ended %s", id, ag.Status)
		default:
			// CREATING, and any status outside the contract, is still running.
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return "", fmt.Errorf("cursor-cloud: timed out waiting for agent %s: %w", id, ctx.Err())
			}
			return "", ctx.Err()
		case <-timer.C:
		}
		if interval < cursorCloudMaxPoll {
			interval *= 2
			if interval > cursorCloudMaxPoll {
				interval = cursorCloudMaxPoll
			}
		}
	}
}

func (a *CursorCloudAgent) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.Timeout > 0 {
		return context.WithTimeout(ctx, a.Timeout)
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	// Same bound as the file-handoff host when the caller set no deadline.
	return context.WithTimeout(ctx, defaultHostTimeout)
}

func (a *CursorCloudAgent) pollInterval() time.Duration {
	if a.PollInterval > 0 {
		return a.PollInterval
	}
	return defaultHostPollInterval
}

func cloudFinalMessage(ctx context.Context, client cursorcloud.Client, id string) (string, error) {
	text, err := client.FinalMessage(ctx, id)
	if err == nil {
		return text, nil
	}
	if errors.Is(err, cursorcloud.ErrNoAssistantMessage) {
		return "", fmt.Errorf("cursor-cloud agent %s finished with no assistant message: %w", id, err)
	}
	return "", err
}

func cloudPrompt(prompt string, schema jsontext.Value) string {
	var b strings.Builder
	b.WriteString(cursorCloudSafety)
	b.WriteString("\n")
	b.WriteString(prompt)
	if len(schema) > 0 {
		b.WriteString("\n\nOutput exactly one JSON document that validates against this schema, with no prose and no code fences:\n")
		b.Write(schema)
	}
	return b.String()
}

func isFullSHA(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func init() {
	Register(&CursorCloudAgent{})
}

var _ StructuredReviewAgent = (*CursorCloudAgent)(nil)
