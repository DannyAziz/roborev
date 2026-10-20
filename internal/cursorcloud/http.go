// Package cursorcloud is an opt-in client for the public Cursor Cloud Agents
// v0 API (https://cursor.com/docs/cloud-agent/api/v0). Nothing in roborev
// calls it yet.
//
// v0 authenticates with HTTP Basic: the API key is the username and the
// password is empty (curl -u KEY:). The key is never written to logs or errors.
//
// TODO: migrate to the Cursor cloud agents v1 API when v1 replaces the v0
// launch, status, and conversation endpoints.
package cursorcloud

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.cursor.com"

// listPageLimit is the maximum page size documented for GET /v0/agents
// (default 20, max 100). ListAgents follows nextCursor so the full set is
// returned. https://cursor.com/docs/cloud-agent/api/v0#list-agents
const listPageLimit = 100

// defaultRequestTimeout is the one deadline for a call. Waiting for
// response headers and reading the body share it, so the two phases
// cannot stack, and an injected http.Client with no header timeout
// cannot block past it. 60s matches a single metadata-style HTTP call.
const defaultRequestTimeout = 60 * time.Second

// defaultResponseHeaderTimeout is the default transport's header wait.
// The request deadline above is the bound that covers headers and the
// body; this matches it for clients that use defaultHTTPClient.
const defaultResponseHeaderTimeout = 60 * time.Second

// defaultResponseBodyTimeout bounds the body read after headers arrive.
// ResponseHeaderTimeout does not cover that read. The request deadline
// already includes the read; this limit can be shorter and is what
// reports a stalled body. It does not extend the request deadline.
//
// With the defaults, the header timer and the body timer are unreachable.
// Both equal defaultRequestTimeout, and the request deadline starts
// before either of them and is never longer, so it is the one that fires.
const defaultResponseBodyTimeout = 60 * time.Second

// maxResponseBytes bounds one response body. 8 MiB covers a long review
// document without retaining an unbounded socket body in memory.
const maxResponseBytes = 8 << 20

// requestDeadlineError is the cause of the per-request timeout. net/http
// returns context.Cause, so a stalled call surfaces this error. Unwrap
// keeps it a context.DeadlineExceeded for errors.Is.
type requestDeadlineError struct{}

func (requestDeadlineError) Error() string { return "cursor cloud: request deadline exceeded" }

func (requestDeadlineError) Unwrap() error { return context.DeadlineExceeded }

var errRequestDeadline error = requestDeadlineError{}

// responseBodyDeadlineError is returned when the body does not arrive
// before the body limit or the request deadline. Unwrap keeps it a
// context.DeadlineExceeded for errors.Is, same as requestDeadlineError.
type responseBodyDeadlineError struct{}

func (responseBodyDeadlineError) Error() string {
	return "cursor cloud: response body deadline exceeded"
}

func (responseBodyDeadlineError) Unwrap() error { return context.DeadlineExceeded }

var errResponseBodyDeadline error = responseBodyDeadlineError{}

// maxErrorMessageBytes matches the 64 KiB error-body cap used for other
// daemon HTTP errors (internal/mcpserver maxErrorBodyBytes). A non-2xx body
// is not copied in full into APIError.Message.
const maxErrorMessageBytes = 64 << 10

// Option configures an HTTP client.
type Option func(*httpClient)

// secret is an API key that fmt must not print. %v, %+v, %#v, and %s of a
// secret, and of any client value that holds one, stay redacted.
type secret string

func (secret) String() string   { return "cursorcloud.secret(redacted)" }
func (secret) GoString() string { return "cursorcloud.secret(redacted)" }

func (secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, "cursorcloud.secret(redacted)")
}

func (k secret) value() string { return string(k) }

type httpClient struct {
	apiKey              secret
	baseURL             string
	hc                  *http.Client
	cfgErr              error
	requestTimeout      time.Duration
	responseBodyTimeout time.Duration
}

func (c httpClient) String() string { return c.redactedSummary() }

func (c httpClient) GoString() string { return c.redactedSummary() }

func (c httpClient) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.redactedSummary())
}

func (c httpClient) redactedSummary() string {
	base := c.baseURL
	if key := c.apiKey.value(); key != "" {
		base = strings.ReplaceAll(base, key, "[REDACTED]")
	}
	return "cursorcloud.Client(base=" + base + ")"
}

// NewHTTPClient calls the Cursor Cloud Agents API at https://api.cursor.com.
// apiKey is trimmed; a blank key fails on the first RPC. The key is sent only
// as a Basic Authorization header.
func NewHTTPClient(apiKey string, opts ...Option) Client {
	c := &httpClient{
		apiKey:              secret(strings.TrimSpace(apiKey)),
		baseURL:             defaultBaseURL,
		hc:                  defaultHTTPClient(),
		requestTimeout:      defaultRequestTimeout,
		responseBodyTimeout: defaultResponseBodyTimeout,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// WithHTTPClient uses hc for calls. A nil client is ignored. The caller's
// timeout and transport are left as set. Each call is still capped at
// 60s (defaultRequestTimeout): the context passed to RoundTrip is
// canceled when that deadline expires, including when hc has no timeout.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *httpClient) {
		if hc != nil {
			c.hc = hc
		}
	}
}

// WithBaseURL points the client at an absolute http or https origin. A base
// path is kept and joined with each request path. Plain http is allowed only
// for a loopback host. Userinfo is stripped so a key in the URL is not sent.
func WithBaseURL(raw string) Option {
	return func(c *httpClient) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			c.cfgErr = errors.New("cursor cloud: base URL must be an absolute http(s) URL")
			return
		}
		if u.Scheme == "http" && !loopbackHost(u.Host) {
			c.cfgErr = errors.New("cursor cloud: plain http is only allowed for loopback hosts")
			return
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		u.Path = strings.TrimRight(u.Path, "/")
		c.baseURL = strings.TrimRight(u.String(), "/")
	}
}

func defaultHTTPClient() *http.Client {
	var tr *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	} else {
		tr = &http.Transport{}
	}
	tr.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewFromEnv returns a client using CURSOR_API_KEY. The value is trimmed.
// An unset or blank variable returns an error that names CURSOR_API_KEY and
// does not include the value.
func NewFromEnv() (Client, error) {
	key := strings.TrimSpace(os.Getenv(EnvAPIKey))
	if key == "" {
		return nil, errors.New(EnvAPIKey + " is unset or blank")
	}
	return NewHTTPClient(key), nil
}

// APIError is a non-2xx Cursor Cloud Agents response. Code and Message are
// redacted of the API key.
type APIError struct {
	Op         string
	StatusCode int
	Code       string
	Message    string
	// RetryAfter is the Retry-After delay (RFC 9110). HasRetryAfter is
	// false when the header was absent or not a delay-seconds / HTTP-date.
	// A present zero means retry immediately.
	RetryAfter    time.Duration
	HasRetryAfter bool
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("cursor cloud %s: status %d: %s: %s", e.Op, e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("cursor cloud %s: status %d: %s", e.Op, e.StatusCode, e.Message)
	case e.Code != "":
		return fmt.Sprintf("cursor cloud %s: status %d: %s", e.Op, e.StatusCode, e.Code)
	default:
		return fmt.Sprintf("cursor cloud %s: status %d", e.Op, e.StatusCode)
	}
}

func (c *httpClient) CreateAgent(ctx context.Context, req CreateAgentRequest) (Agent, error) {
	if strings.TrimSpace(req.Repo) == "" {
		return Agent{}, errors.New("cursor cloud create agent: repository is empty")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return Agent{}, errors.New("cursor cloud create agent: prompt is empty")
	}
	// CreateAgentRequest.Title has no v0 launch field. The API returns a
	// generated name, mapped onto Agent.Title. autoCreatePr is explicitly
	// false so a review launch does not open a pull request (documented default).
	body := launchBody{
		Model: req.Model,
		Source: launchSource{
			Repository: req.Repo,
			Ref:        req.Ref,
		},
	}
	body.Prompt.Text = req.Prompt
	var wire agentWire
	if err := c.do(ctx, "create agent", http.MethodPost, "/v0/agents", body, &wire); err != nil {
		return Agent{}, err
	}
	if wire.ID == "" {
		return Agent{}, errors.New("cursor cloud create agent: response missing id")
	}
	return toAgent(wire), nil
}

func (c *httpClient) GetAgent(ctx context.Context, id string) (Agent, error) {
	path, err := agentPath(id)
	if err != nil {
		return Agent{}, err
	}
	var wire agentWire
	if err := c.do(ctx, "get agent", http.MethodGet, path, nil, &wire); err != nil {
		return Agent{}, err
	}
	return toAgent(wire), nil
}

func (c *httpClient) FinalMessage(ctx context.Context, id string) (string, error) {
	path, err := agentPath(id)
	if err != nil {
		return "", err
	}
	var wire conversationWire
	if err := c.do(ctx, "final message", http.MethodGet, path+"/conversation", nil, &wire); err != nil {
		return "", err
	}
	for _, msg := range slices.Backward(wire.Messages) {
		if msg.Type == "assistant_message" {
			return msg.Text, nil
		}
	}
	return "", ErrNoAssistantMessage
}

func (c *httpClient) UserPrompt(ctx context.Context, id string) (string, error) {
	path, err := agentPath(id)
	if err != nil {
		return "", err
	}
	var wire conversationWire
	if err := c.do(ctx, "conversation", http.MethodGet, path+"/conversation", nil, &wire); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, msg := range wire.Messages {
		if msg.Type != "user_message" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(msg.Text)
	}
	return b.String(), nil
}

func (c *httpClient) StopAgent(ctx context.Context, id string) error {
	path, err := agentPath(id)
	if err != nil {
		return err
	}
	// POST /v0/agents/{id}/stop pauses execution and does not delete the agent.
	// https://cursor.com/docs/cloud-agent/api/v0#stop-an-agent
	return c.do(ctx, "stop agent", http.MethodPost, path+"/stop", nil, nil)
}

func (c *httpClient) DeleteAgent(ctx context.Context, id string) error {
	path, err := agentPath(id)
	if err != nil {
		return err
	}
	// DELETE /v0/agents/{id} is permanent. The v0 docs do not say it stops billing.
	// https://cursor.com/docs/cloud-agent/api/v0#delete-an-agent
	return c.do(ctx, "delete agent", http.MethodDelete, path, nil, nil)
}

func (c *httpClient) ListAgents(ctx context.Context) ([]Agent, error) {
	cursor := ""
	seen := map[string]struct{}{"": {}}
	var out []Agent
	for {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, c.redactErr(err)
			}
		}
		q := url.Values{}
		q.Set("limit", strconv.Itoa(listPageLimit))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var wire listWire
		if err := c.do(ctx, "list agents", http.MethodGet, "/v0/agents?"+q.Encode(), nil, &wire); err != nil {
			return nil, err
		}
		for _, agent := range wire.Agents {
			out = append(out, toAgent(agent))
		}
		if wire.NextCursor == "" {
			return out, nil
		}
		if _, ok := seen[wire.NextCursor]; ok {
			return nil, errors.New("cursor cloud list agents: pagination cursor repeated")
		}
		seen[wire.NextCursor] = struct{}{}
		cursor = wire.NextCursor
	}
}

type launchBody struct {
	Prompt struct {
		Text string `json:"text"`
	} `json:"prompt"`
	Model  string       `json:"model,omitempty"`
	Source launchSource `json:"source"`
	Target struct {
		AutoCreatePr bool `json:"autoCreatePr"`
	} `json:"target"`
}

type launchSource struct {
	Repository string `json:"repository"`
	Ref        string `json:"ref,omitempty"`
}

type agentWire struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Source struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
	} `json:"source"`
}

type listWire struct {
	Agents     []agentWire `json:"agents"`
	NextCursor string      `json:"nextCursor"`
}

type conversationWire struct {
	Messages []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"messages"`
}

func toAgent(w agentWire) Agent {
	return Agent{
		ID:               w.ID,
		Status:           AgentStatus(w.Status),
		Title:            w.Name,
		SourceRepository: w.Source.Repository,
		SourceRef:        w.Source.Ref,
	}
}

func agentPath(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", errors.New("cursor cloud: agent id is empty")
	}
	if strings.ContainsAny(id, "/\\?#") || id == "." || id == ".." {
		return "", errors.New("cursor cloud: agent id is invalid")
	}
	return "/v0/agents/" + url.PathEscape(id), nil
}

func (c *httpClient) do(ctx context.Context, op, method, path string, in, out any) error {
	if ctx == nil {
		return errors.New("cursor cloud: nil context")
	}
	if c.cfgErr != nil {
		return c.cfgErr
	}
	if c.apiKey.value() == "" {
		return errors.New("cursor cloud: API key is empty")
	}
	endpoint, err := c.endpoint(path)
	if err != nil {
		return c.redactErr(err)
	}
	var payload []byte
	if in != nil {
		payload, err = json.Marshal(in)
		if err != nil {
			return c.redactErr(fmt.Errorf("cursor cloud: encode request: %w", err))
		}
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	// One deadline covers response headers and the full body read.
	ctx, cancel := context.WithTimeoutCause(ctx, c.requestTimeout, errRequestDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return c.redactErr(fmt.Errorf("cursor cloud: build request: %w", err))
	}
	req.Header.Set("Authorization", c.authorization())
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.redactErr(fmt.Errorf("cursor cloud: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := c.readResponseBody(ctx, resp.Body)
	if err != nil {
		return c.redactErr(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.apiError(op, resp.StatusCode, data, resp.Header)
	}
	if out == nil {
		return nil
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("cursor cloud " + op + ": empty response")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return c.redactErr(fmt.Errorf("cursor cloud %s: decode response: %w", op, err))
	}
	return nil
}

func (c *httpClient) readResponseBody(ctx context.Context, body io.ReadCloser) ([]byte, error) {
	timer := time.NewTimer(c.responseBodyTimeout)
	defer timer.Stop()
	type readResult struct {
		data []byte
		err  error
	}
	// Buffer one result so the reader can finish after this function
	// returns. Waiting here would pin the caller when Close does not
	// unblock Read. A RoundTripper that ignores the request context,
	// and Body.Close, are the caller's responsibility. Callers who
	// inject a custom body must make Close unblock Read, or the reader
	// goroutine leaks.
	done := make(chan readResult, 1)
	go func() {
		data, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
		done <- readResult{data, err}
	}()
	select {
	case result := <-done:
		if result.err != nil {
			return nil, fmt.Errorf("cursor cloud: read response: %w", result.err)
		}
		if int64(len(result.data)) > maxResponseBytes {
			return nil, errors.New("cursor cloud: response body exceeds 8 MiB")
		}
		return result.data, nil
	case <-timer.C:
		// Close, then return without waiting for Read. net/http bodies
		// unblock Read from Close. A custom body may not, and waiting
		// would pin the caller. The buffered send lets the reader exit
		// later if Read does return.
		_ = body.Close()
		return nil, errResponseBodyDeadline
	case <-ctx.Done():
		_ = body.Close()
		if errors.Is(context.Cause(ctx), errRequestDeadline) {
			return nil, errResponseBodyDeadline
		}
		return nil, ctx.Err()
	}
}

func (c *httpClient) endpoint(rawPath string) (string, error) {
	path, query, _ := strings.Cut(rawPath, "?")
	path = strings.TrimPrefix(path, "/")
	joined, err := url.JoinPath(c.baseURL, path)
	if err != nil {
		return "", errors.New("cursor cloud: invalid request path")
	}
	if query != "" {
		joined += "?" + query
	}
	return joined, nil
}

func (c *httpClient) authorization() string {
	token := base64.StdEncoding.EncodeToString([]byte(c.apiKey.value() + ":"))
	return "Basic " + token
}

func (c *httpClient) apiError(op string, status int, data []byte, header http.Header) error {
	code, message, ok := parseAPIErrorBody(data)
	if !ok {
		message = string(data)
	}
	// Redact the full error text, Code and Message, before the 64 KiB cap.
	// Truncating first keeps the prefix of a secret that straddles the cut.
	code = c.redact(strings.TrimSpace(code))
	message = c.redact(message)
	if len(message) > maxErrorMessageBytes {
		message = message[:maxErrorMessageBytes]
	}
	retryAfter, hasRetryAfter := parseRetryAfter(header.Get("Retry-After"), time.Now())
	return &APIError{
		Op:            op,
		StatusCode:    status,
		Code:          code,
		Message:       strings.TrimSpace(message),
		RetryAfter:    retryAfter,
		HasRetryAfter: hasRetryAfter,
	}
}

// parseRetryAfter reads RFC 9110 Retry-After: delay-seconds or an HTTP-date.
// A date already in the past is a present header with a zero wait.
func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		return 0, true
	}
	return delay, true
}

func parseAPIErrorBody(data []byte) (code, message string, ok bool) {
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		return "", "", false
	}
	code, _ = top["code"].(string)
	message, _ = top["message"].(string)
	switch nested := top["error"].(type) {
	case string:
		if code == "" {
			code = nested
		}
		if message == "" {
			message = nested
		}
	case map[string]any:
		if code == "" {
			code, _ = nested["code"].(string)
		}
		if message == "" {
			message, _ = nested["message"].(string)
		}
		if message == "" {
			message, _ = nested["error"].(string)
		}
	}
	return code, message, true
}

func (c *httpClient) redact(s string) string {
	key := c.apiKey.value()
	if key == "" || s == "" {
		return s
	}
	s = strings.ReplaceAll(s, key, "[REDACTED]")
	encoded := base64.StdEncoding.EncodeToString([]byte(key + ":"))
	return strings.ReplaceAll(s, encoded, "[REDACTED]")
}

func (c *httpClient) redactErr(err error) error {
	if err == nil {
		return nil
	}
	msg := c.redact(err.Error())
	if msg == err.Error() {
		return err
	}
	// Drop the unwrap chain so the original text cannot be recovered.
	return errors.New(msg)
}

var _ Client = (*httpClient)(nil)
