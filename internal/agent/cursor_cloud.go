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
	// cursorCloudDeleteAttempts bounds retries of one failed DELETE.
	// A 4xx is not retried; it will not succeed on the next attempt.
	cursorCloudDeleteAttempts = 3
	// cursorCloudDeleteBackoff separates this process's DELETE attempts.
	// DELETE /v0/agents does not define a retry interval
	// (https://cursor.com/docs/cloud-agent/api/v0#delete-an-agent).
	// 100ms only keeps the three local attempts from being one burst.
	cursorCloudDeleteBackoff = 100 * time.Millisecond
	// cursorCloudReconcileBackoff is the pause between those lists.
	cursorCloudReconcileBackoff = 50 * time.Millisecond
)

// cursorCloudReconcileWindow bounds every reconcile pass after a create
// was sent. Polling uses context.WithoutCancel plus this timeout, so a
// caller deadline or cancel cannot skip a later list. It is the same
// length as cursorCloudDeleteTimeout.
//
// 60s is an existing policy value nobody has checked against a measured
// publish lag. It is the same length as cursorcloud's defaultRequestTimeout
// for one metadata call. 500ms was only longer than this package's test
// fixture and is not used.
var cursorCloudReconcileWindow = 60 * time.Second

// cursorCloudReconcilePassTimeout is the one deadline shared by every
// candidate conversation read in a reconcile pass. It is the existing
// per-call cursorCloudDeleteTimeout reused as a budget for the whole
// pass. Nobody has measured a multi-candidate conversation read; this is
// not a new limit derived from that workload.
var cursorCloudReconcilePassTimeout = cursorCloudDeleteTimeout

// cursorCloudDeleteTimeout bounds one stop, one DELETE, one reconcile list,
// or one reconcile conversation read. Reconcile passes run on a context
// detached from the review, so a review context that is already done
// cannot skip a later list. A stop that never answers must not cancel
// the deletes. 60s matches the cursorcloud client's defaultRequestTimeout,
// its one deadline for a metadata call.
var cursorCloudDeleteTimeout = 60 * time.Second

const (
	// cursorCloudLaunchMarker prefixes the unique token embedded in the
	// prompt. v0 launch has no idempotency key, so a create that fails after
	// send is found again by this token.
	cursorCloudLaunchMarker = "roborev-launch:"
)

func CursorCloudLaunchMarker(daemonID, token string) string {
	token = strings.TrimSpace(token)
	if daemonID = strings.TrimSpace(daemonID); daemonID == "" {
		return cursorCloudLaunchMarker + token
	}
	return cursorCloudLaunchMarker + daemonID + ":" + token
}

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
	launches   *cursorCloudLaunchGate
	DaemonID   string
	BindLaunch func(token, repoURL string) error
	BindAgent  func(token, id string) error
	// NoteCleanupFailure records a stop or delete that did not finish.
	// The launch row stays uncleaned.
	NoteCleanupFailure func(msg string) error
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
	// The registry agent has no Client. Batch and plan call Review on that
	// agent, which has no launch row. The worker sets BindLaunch first.
	// A test double sets Client and may omit the binding.
	if a.Client == nil && a.BindLaunch == nil {
		return "", errors.New("cursor-cloud: refusing to launch without a daemon launch binding")
	}
	client, err := a.client()
	if err != nil {
		if IsCursorCloudConfig(err) {
			return "", err
		}
		// Client setup has not launched. A local backup may run it.
		return "", MarkUnavailable(err)
	}
	sha, err := a.commitSHA(ctx, repoPath, commitSHA)
	if err != nil {
		return "", MarkUnavailable(err)
	}
	repoURL, err := a.repoURL(repoPath)
	if err != nil {
		return "", MarkUnavailable(err)
	}
	// Every cloud call runs under a deadline. withTimeout keeps one the
	// caller already set, and otherwise uses the host handoff bound.
	ctx, cancel := a.withTimeout(ctx)
	defer cancel()

	release, refund, err := a.launchGate().acquire()
	if err != nil {
		// The cap refused before CreateAgent. Nothing was launched, so a
		// local backup may take the job. The hourly counter was not charged.
		return "", MarkUnavailable(err)
	}
	defer release()

	// The v0 HTTP body has no title field and no idempotency key
	// (https://cursor.com/docs/cloud-agent/api/v0#launch-an-agent). Title is
	// still set so the launch record names the review. repoURL is already
	// stripped of userinfo. The marker lets a create that fails after send
	// be found by listing agents and reading the conversation.
	token := newLaunchToken()
	if err := ctx.Err(); err != nil {
		// Already done before CreateAgent. The POST was not sent.
		refund()
		return "", err
	}
	if a.BindLaunch != nil {
		if bindErr := a.BindLaunch(token, repoURL); bindErr != nil {
			refund()
			return "", bindErr
		}
	}
	marker := CursorCloudLaunchMarker(a.DaemonID, token)
	reconcileToken := token
	if id := strings.TrimSpace(a.DaemonID); id != "" {
		reconcileToken = id + ":" + token
	}
	created, err := client.CreateAgent(ctx, cursorcloud.CreateAgentRequest{
		Repo:   repoURL,
		Ref:    sha,
		Prompt: cloudPrompt(prompt, schema) + "\n" + marker + "\n",
		Model:  a.Model,
		Title:  cursorCloudTitle(repoURL, sha),
	})
	if err != nil {
		// Classify the create error before reconcile is joined. A reconcile
		// dial or DNS error would otherwise make cursorCloudPreSend true and
		// drop the sentinel, and the worker would POST /v0/agents again.
		sent := !cursorCloudPreSend(err)
		if !sent {
			// The POST never left, so this attempt does not spend the hour.
			refund()
			return "", err
		}
		// A 429 rejects the create. Nothing was launched, so do not spend
		// the reconcile window looking for a VM.
		if cursorCloudRateLimited(err) {
			return "", cursorCloudNonRetryable(err)
		}
		if recErr := reconcileCursorCloudLaunch(ctx, client, repoURL, reconcileToken, output); recErr != nil {
			err = errors.Join(err, recErr)
		}
		return "", cursorCloudNonRetryable(err)
	}
	// Delete is armed before any later work, including the status line, so a
	// panic or error there still removes the VM.
	defer func() {
		delErr := deleteCursorCloudAgent(client, created.ID, output)
		if delErr == nil {
			return
		}
		// The VM is still there. Say so now; the row stays uncleaned for the
		// next startup sweep. Do not wait for that restart to mention it.
		msg := fmt.Sprintf("cursor-cloud orphan risk: pending cleanup for agent %s: %v", created.ID, delErr)
		log.Print(msg)
		if a.NoteCleanupFailure != nil {
			if noteErr := a.NoteCleanupFailure(msg); noteErr != nil {
				delErr = errors.Join(delErr, noteErr)
			}
		}
		if err != nil {
			err = errors.Join(err, delErr)
			return
		}
		err = cursorCloudNonRetryable(delErr)
	}()
	if a.BindAgent != nil {
		if bindErr := a.BindAgent(token, created.ID); bindErr != nil {
			return "", cursorCloudNonRetryable(bindErr)
		}
	}
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

// ErrCursorCloudNonRetryable marks a cursor-cloud create that may already
// have been sent. Match it with errors.Is. Pre-send dial and DNS failures,
// and a launch-cap refusal, do not use it: those never reached CreateAgent's
// request bytes, so a local backup may run the job.
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

// cursorCloudPreSend reports a failure that cannot have transmitted the
// launch. net.OpError.Op is "dial" while Dial is still establishing the
// socket, and a DNSError is that lookup failing before a connection exists
// (https://pkg.go.dev/net#OpError, https://pkg.go.dev/net#DNSError).
// "read" and "write" mean the conn already exists, so the POST may have
// been sent. A timeout or a dropped response is not proof that it never left.
func cursorCloudPreSend(err error) bool {
	if err == nil {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns != nil {
		return true
	}
	var op *net.OpError
	if !errors.As(err, &op) || op == nil {
		return false
	}
	return op.Op == "dial"
}

func deleteCursorCloudAgent(client cursorcloud.Client, id string, output io.Writer) error {
	// Stop pauses the VM. v0 Delete is documented as permanent and does not
	// say it stops billing, so stop runs first and delete still runs when
	// stop fails. https://cursor.com/docs/cloud-agent/api/v0#stop-an-agent
	// https://cursor.com/docs/cloud-agent/api/v0#delete-an-agent
	// Stop and each DELETE get a fresh timeout. Sharing one 60s context let
	// a stalled stop starve the deletes.
	var stopNotePanic any
	defer func() {
		if stopNotePanic != nil {
			panic(stopNotePanic)
		}
	}()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), cursorCloudDeleteTimeout)
	stopErr := client.StopAgent(stopCtx, id)
	stopCancel()
	if stopErr != nil && !cursorCloudStopQuiet(stopErr) {
		note := fmt.Errorf("cursor-cloud: stop agent %s: %w", id, stopErr)
		stopNotePanic = cleanupNotePanic(output, note.Error())
	}
	var err error
	for attempt := 1; attempt <= cursorCloudDeleteAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(cursorCloudDeleteBackoff)
		}
		delCtx, cancel := context.WithTimeout(context.Background(), cursorCloudDeleteTimeout)
		err = client.DeleteAgent(delCtx, id)
		cancel()
		if err == nil || cursorCloudDeleteGone(err) {
			// 404 means the agent is already gone. That attempt is done.
			return nil
		}
		if cursorCloudDeleteFinal(err) {
			break
		}
	}
	surfaced := fmt.Errorf("cursor-cloud: delete agent %s: %w", id, err)
	writeCursorCloudCleanupNote(output, surfaced.Error())
	return surfaced
}

// writeCursorCloudCleanupNote records a cleanup failure. A panicking
// writer must not skip DeleteAgent or replace the panic that entered
// cleanup; that original panic still leaves Review.
func writeCursorCloudCleanupNote(output io.Writer, note string) {
	if p := cleanupNotePanic(output, note); p != nil {
		panic(p)
	}
}

// cleanupNotePanic writes note and returns a writer panic instead of
// letting it skip the rest of cleanup. The caller panics after delete.
func cleanupNotePanic(output io.Writer, note string) (panicked any) {
	if note == "" {
		return nil
	}
	defer func() { panicked = recover() }()
	log.Printf("%s", note)
	if output != nil {
		fmt.Fprintln(output, note)
	}
	return nil
}

// cursorCloudDeleteFinal reports a DELETE that must not be retried.
// 4xx other than 404 is the client's request, not a transient server
// failure. 500 is not final: it is retried, and the delete stays not-done
// when every attempt fails.
func cursorCloudDeleteFinal(err error) bool {
	apiErr, ok := errors.AsType[*cursorcloud.APIError](err)
	return ok && apiErr != nil && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusNotFound
}

// cursorCloudDeleteGone reports a DELETE whose agent is already absent.
func cursorCloudDeleteGone(err error) bool {
	apiErr, ok := errors.AsType[*cursorcloud.APIError](err)
	return ok && apiErr != nil && apiErr.StatusCode == http.StatusNotFound
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
// source.ref but not the prompt. Repository equality uses config.RemoteIdentity
// so .git and scp versus https still match. Path case is folded only for
// github.com. The launch token in the
// user_message confirms it is this launch. A cleanup error names the orphan id.
// https://cursor.com/docs/cloud-agent/api/v0#launch-an-agent
// https://cursor.com/docs/cloud-agent/api/v0#list-agents
// https://cursor.com/docs/cloud-agent/api/v0#agent-conversation
func reconcileCursorCloudLaunch(ctx context.Context, client cursorcloud.Client, repoURL, token string, output io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	// The create was sent. Every pass, including the wait between lists,
	// uses a context the caller's deadline and cancel cannot finish.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cursorCloudReconcileWindow)
	defer cancel()
	// started only decides whether another pass begins. Each pass's
	// conversation reads share one deadline that starts before that
	// pass's list, so a slow list consumes it.
	started := time.Now()
	marker := cursorCloudLaunchMarker + token
	var errs []error
	foundLaunch := false
	windowExpired := false
	backoff := cursorCloudReconcileBackoff
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			windowExpired = true
			errs = appendReconcileErr(errs, err)
			break
		}
		if attempt > 0 && !time.Now().Before(started.Add(cursorCloudReconcileWindow)) {
			windowExpired = true
			break
		}
		// Start the 60s before ListAgents. One clock for the whole
		// reconcile would already be spent by earlier passes, and starting
		// it after the list returns would hide the list's duration.
		passCtx, passCancel := context.WithTimeout(ctx, cursorCloudReconcilePassTimeout)
		listCtx, listCancel := context.WithTimeout(ctx, cursorCloudDeleteTimeout)
		agents, err := client.ListAgents(listCtx)
		listCancel()
		var found bool
		var matchErr error
		if err == nil {
			found, matchErr = deleteMatchingCursorCloudLaunch(passCtx, client, agents, repoURL, marker, output)
		}
		passCancel()
		if err != nil {
			errs = appendReconcileErr(errs, fmt.Errorf("cursor-cloud: reconcile list: %w", err))
			if cursorCloudRateLimited(err) {
				break
			}
		} else if found || matchErr != nil {
			if matchErr != nil {
				errs = appendReconcileErr(errs, matchErr)
			}
			if found {
				foundLaunch = true
				break
			}
			// A conversation 429 stops the reconcile the same way a list 429
			// does. Reading more candidates or listing again would keep
			// spending the rate limit.
			if cursorCloudRateLimited(matchErr) {
				break
			}
		}
		if err := waitCursorCloud(ctx, backoff); err != nil {
			windowExpired = true
			errs = appendReconcileErr(errs, err)
			break
		}
		backoff *= 2
		if backoff > cursorCloudReconcileBackoffMax {
			backoff = cursorCloudReconcileBackoffMax
		}
	}
	if !foundLaunch {
		if windowExpired {
			writeCursorCloudCleanupNote(output, "cursor-cloud: reconcile window expired before the launch was found")
		}
		// The cap still bounds the join. The terminal miss replaces the
		// last slot when the list is already full, so it is never dropped.
		errs = appendReconcileTerminal(errs, errors.New("cursor-cloud: reconcile did not find the launch"))
	}
	return errors.Join(errs...)
}

const (
	cursorCloudReconcileBackoffMax = 2 * time.Second
	cursorCloudReconcileMaxErrs    = 8
)

func appendReconcileErr(errs []error, err error) []error {
	if err == nil || len(errs) >= cursorCloudReconcileMaxErrs {
		return errs
	}
	return append(errs, err)
}

func appendReconcileTerminal(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	if len(errs) >= cursorCloudReconcileMaxErrs {
		errs = append([]error(nil), errs[:cursorCloudReconcileMaxErrs-1]...)
	}
	return append(errs, err)
}

func cursorCloudRateLimited(err error) bool {
	// errors.As stops at the first APIError. A 429 joined after a 500
	// still has to stop the reconcile, whichever sibling is first.
	if err == nil {
		return false
	}
	if apiErr, ok := err.(*cursorcloud.APIError); ok { //nolint:errorlint // walk every joined sibling; errors.As returns only the first APIError
		return apiErr != nil && apiErr.StatusCode == http.StatusTooManyRequests
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return slices.ContainsFunc(joined.Unwrap(), cursorCloudRateLimited)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return cursorCloudRateLimited(wrapped.Unwrap())
	}
	return false
}

// MarkCursorCloudSent marks a failure after CreateAgent was sent. The
// worker must not POST /v0/agents again for this attempt.
func MarkCursorCloudSent(err error) error {
	return cursorCloudNonRetryable(err)
}

func deleteMatchingCursorCloudLaunch(ctx context.Context, client cursorcloud.Client, agents []cursorcloud.Agent, repoURL, marker string, output io.Writer) (bool, error) {
	var errs []error
	found := false
	for _, ag := range agents {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if !sameCursorCloudRepo(repoURL, ag.SourceRepository) {
			continue
		}
		text, promptErr := client.UserPrompt(ctx, ag.ID)
		if promptErr != nil {
			errs = append(errs, fmt.Errorf("cursor-cloud: orphan agent %s: %w", ag.ID, promptErr))
			if cursorCloudRateLimited(promptErr) {
				break
			}
			continue
		}
		if !strings.Contains(text, marker) {
			continue
		}
		found = true
		if delErr := deleteCursorCloudAgent(client, ag.ID, output); delErr != nil {
			return true, fmt.Errorf("cursor-cloud: orphan agent %s: %w", ag.ID, delErr)
		}
	}
	return found, errors.Join(errs...)
}

func StopAndDeleteCursorCloudAgent(client cursorcloud.Client, id string) error {
	return deleteCursorCloudAgent(client, id, nil)
}

// CursorCloudRepoUsable is false for empty input and the redacted-url sentinel.
func CursorCloudRepoUsable(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == config.RedactedURL {
		return false
	}
	return config.RemoteIdentity(raw) != ""
}

func DeleteCursorCloudLaunchByMarker(ctx context.Context, client cursorcloud.Client, daemonID, token, repoURL string) (bool, error) {
	if client == nil || ctx == nil || strings.TrimSpace(daemonID) == "" || strings.TrimSpace(token) == "" || !CursorCloudRepoUsable(repoURL) {
		return false, errors.New("cursor-cloud: cleanup marker is incomplete")
	}
	listCtx, cancel := context.WithTimeout(ctx, cursorCloudDeleteTimeout)
	agents, err := client.ListAgents(listCtx)
	cancel()
	if err != nil {
		return false, fmt.Errorf("cursor-cloud: cleanup list: %w", err)
	}
	passCtx, passCancel := context.WithTimeout(ctx, cursorCloudReconcilePassTimeout)
	defer passCancel()
	return deleteMatchingCursorCloudLaunch(passCtx, client, agents, repoURL, CursorCloudLaunchMarker(daemonID, token), nil)
}

func sameCursorCloudRepo(want, got string) bool {
	// A missing, redacted, or unparseable source is not evidence of this
	// repository. The launch token is only read after the source names it.
	if !CursorCloudRepoUsable(want) || !CursorCloudRepoUsable(got) {
		return false
	}
	wantID := config.RemoteIdentity(want)
	gotID := config.RemoteIdentity(got)
	if wantID == "" || gotID == "" {
		return false
	}
	return wantID == gotID
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
