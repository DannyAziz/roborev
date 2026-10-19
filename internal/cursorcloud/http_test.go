package cursorcloud_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/cursorcloud"
)

const testKey = "super-secret-cursor-key"

func TestNewFromEnv(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "")
	_, err := cursorcloud.NewFromEnv()
	require.ErrorContains(t, err, "CURSOR_API_KEY")
	require.ErrorContains(t, err, "unset or blank")

	t.Setenv(cursorcloud.EnvAPIKey, "   ")
	_, err = cursorcloud.NewFromEnv()
	require.ErrorContains(t, err, "CURSOR_API_KEY")
	assert.NotContains(t, err.Error(), testKey)

	t.Setenv(cursorcloud.EnvAPIKey, testKey)
	client, err := cursorcloud.NewFromEnv()
	require.NoError(t, err)
	require.NotNil(t, client)
}

func TestHTTPCreateAndGet(t *testing.T) {
	var gotBody map[string]any
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/agents":
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			raw := readBody(t, r)
			if !assert.NoError(t, json.Unmarshal(raw, &gotBody)) {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			writeJSON(t, w, map[string]any{
				"id": "bc_1", "name": "Review widgets", "status": "CREATING",
			})
		case "/v0/agents/bc_1":
			assert.Equal(t, http.MethodGet, r.Method)
			writeJSON(t, w, map[string]any{
				"id": "bc_1", "name": "Review widgets", "status": "FINISHED",
			})
		default:
			http.NotFound(w, r)
		}
	})

	created, err := client.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{
		Repo:   "https://github.com/acme/widgets",
		Ref:    "abc123",
		Prompt: "review this commit",
		Model:  "claude-4-sonnet",
		Title:  "ignored-by-v0",
	})
	require.NoError(t, err)
	// v0 launch returns CREATING. The client passes that status through unchanged.
	assert.Equal(t, cursorcloud.StatusCreating, created.Status)
	assert.NotEqual(t, cursorcloud.StatusRunning, created.Status)
	assert.Equal(t, cursorcloud.Agent{
		ID: "bc_1", Status: cursorcloud.StatusCreating, Title: "Review widgets",
	}, created)
	// CreateAgentRequest.Title is not sent on v0. The body has no title, and
	// the response name is mapped to Agent.Title.
	_, hasTitle := gotBody["title"]
	assert.False(t, hasTitle)
	_, hasName := gotBody["name"]
	assert.False(t, hasName)
	assert.Equal(t, "Review widgets", created.Title)
	body, err := json.Marshal(gotBody)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "ignored-by-v0")
	assert.Equal(t, "review this commit", gotBody["prompt"].(map[string]any)["text"])
	assert.Equal(t, "claude-4-sonnet", gotBody["model"])
	source := gotBody["source"].(map[string]any)
	assert.Equal(t, "https://github.com/acme/widgets", source["repository"])
	assert.Equal(t, "abc123", source["ref"])
	target := gotBody["target"].(map[string]any)
	assert.Equal(t, false, target["autoCreatePr"])

	got, err := client.GetAgent(context.Background(), "bc_1")
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusFinished, got.Status)
	assert.Equal(t, []string{http.MethodPost, http.MethodGet}, methods(rec))
	assertAuth(t, rec)
}

func TestHTTPCreateOmitsEmptyModelAndRef(t *testing.T) {
	var raw []byte
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw = readBody(t, r)
		writeJSON(t, w, map[string]any{"id": "bc_2", "name": "n", "status": "RUNNING"})
	})
	_, err := client.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{
		Repo:   "https://github.com/acme/widgets",
		Prompt: "review",
	})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"model"`)
	assert.NotContains(t, string(raw), `"ref"`)
	assert.NotContains(t, string(raw), "ignored")
	assert.Contains(t, string(raw), `"autoCreatePr":false`)
}

func TestHTTPCreateValidationDoesNotDial(t *testing.T) {
	client := cursorcloud.NewHTTPClient(testKey, cursorcloud.WithBaseURL("http://127.0.0.1:1"))
	_, err := client.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{Prompt: "x"})
	require.ErrorContains(t, err, "repository is empty")
	assertNoKey(t, err)
	_, err = client.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{Repo: "https://github.com/acme/widgets"})
	require.ErrorContains(t, err, "prompt is empty")
	_, err = client.GetAgent(context.Background(), " ")
	require.ErrorContains(t, err, "agent id is empty")
	_, err = client.GetAgent(context.Background(), "../secret")
	require.ErrorContains(t, err, "agent id is invalid")
	err = client.StopAgent(context.Background(), " ")
	require.ErrorContains(t, err, "agent id is empty")
	err = client.DeleteAgent(context.Background(), " ")
	require.ErrorContains(t, err, "agent id is empty")
	err = client.DeleteAgent(context.Background(), "../secret")
	require.ErrorContains(t, err, "agent id is invalid")
	err = client.StopAgent(nil, "bc_1") //nolint:staticcheck // SA1012: nil context is the case under test.
	require.ErrorContains(t, err, "nil context")
	_, err = client.GetAgent(nil, "bc_1") //nolint:staticcheck // SA1012: nil context is the case under test.
	require.ErrorContains(t, err, "nil context")
	assertNoKey(t, err)
}

func TestHTTPFinalMessage(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v0/agents/bc_1/conversation", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		writeJSON(t, w, map[string]any{
			"id": "bc_1",
			"messages": []map[string]string{
				{"id": "m1", "type": "user_message", "text": "review"},
				{"id": "m2", "type": "assistant_message", "text": "first"},
				{"id": "m3", "type": "user_message", "text": "more"},
				{"id": "m4", "type": "assistant_message", "text": "the review"},
			},
		})
	})
	text, err := client.FinalMessage(context.Background(), "bc_1")
	require.NoError(t, err)
	assert.Equal(t, "the review", text)
	assertAuth(t, rec)

	empty, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{
			"messages": []map[string]string{{"type": "user_message", "text": "only user"}},
		})
	})
	// FinalMessage errors when the conversation has no assistant_message.
	// It does not return an empty message as a successful review.
	text, err = empty.FinalMessage(context.Background(), "bc_1")
	require.ErrorIs(t, err, cursorcloud.ErrNoAssistantMessage)
	assert.Empty(t, text)
	assertNoKey(t, err)
}

func TestHTTPStopAndDelete(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/agents/bc_1/stop":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"bc_1"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v0/agents/bc_1":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})
	require.NoError(t, client.StopAgent(context.Background(), "bc_1"))
	require.NoError(t, client.DeleteAgent(context.Background(), "bc_1"))
	assert.Equal(t, []string{http.MethodPost, http.MethodDelete}, methods(rec))
	assertAuth(t, rec)
}

func TestHTTPListPaginates(t *testing.T) {
	client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/v0/agents", r.URL.Path)
		assert.Equal(t, "100", r.URL.Query().Get("limit"))
		switch r.URL.Query().Get("cursor") {
		case "":
			writeJSON(t, w, map[string]any{
				"agents":     []map[string]string{{"id": "bc_a", "name": "A", "status": "RUNNING"}},
				"nextCursor": "bc_next",
			})
		case "bc_next":
			writeJSON(t, w, map[string]any{
				"agents": []map[string]string{{"id": "bc_b", "name": "B", "status": "FINISHED"}},
			})
		default:
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		}
	})
	agents, err := client.ListAgents(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []cursorcloud.Agent{
		{ID: "bc_a", Status: cursorcloud.StatusRunning, Title: "A"},
		{ID: "bc_b", Status: cursorcloud.StatusFinished, Title: "B"},
	}, agents)
	require.Len(t, rec.reqs, 2)
	assertAuth(t, rec)
}

func TestHTTPListRepeatedCursor(t *testing.T) {
	hits := 0
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeJSON(t, w, map[string]any{
			"agents":     []map[string]string{{"id": "bc_a", "name": "A", "status": "RUNNING"}},
			"nextCursor": "again",
		})
	})
	_, err := client.ListAgents(context.Background())
	require.ErrorContains(t, err, "pagination cursor repeated")
	assert.Equal(t, 2, hits)
	assertNoKey(t, err)
}

func TestHTTPErrorMappingAndRedaction(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(testKey + ":"))
	cases := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantSubstr string
	}{
		{
			name:       "nested message and code",
			status:     http.StatusUnauthorized,
			body:       `{"error":{"message":"denied ` + testKey + `","code":"unauthorized"}}`,
			wantCode:   "unauthorized",
			wantSubstr: "denied [REDACTED]",
		},
		{
			name:       "nested error string",
			status:     http.StatusBadRequest,
			body:       `{"error":{"error":"Failed to determine repository default branch"}}`,
			wantSubstr: "Failed to determine repository default branch",
		},
		{
			name:       "top-level code and message",
			status:     http.StatusInternalServerError,
			body:       `{"code":"internal","message":"Error"}`,
			wantCode:   "internal",
			wantSubstr: "Error",
		},
		{
			name:       "error string and message",
			status:     http.StatusBadRequest,
			body:       `{"error":"invalid_request","message":"repository not found"}`,
			wantCode:   "invalid_request",
			wantSubstr: "repository not found",
		},
		{
			name:       "plain body with key and basic token",
			status:     http.StatusForbidden,
			body:       "nope " + testKey + " " + encoded,
			wantSubstr: "nope [REDACTED] [REDACTED]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			_, err := client.GetAgent(context.Background(), "bc_1")
			require.Error(t, err)
			assertNoKey(t, err)
			assert.NotContains(t, err.Error(), encoded)
			var apiErr *cursorcloud.APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tc.status, apiErr.StatusCode)
			assert.Equal(t, tc.wantCode, apiErr.Code)
			assert.Contains(t, apiErr.Message, tc.wantSubstr)
			assert.NotContains(t, apiErr.Message, testKey)
			assertAuth(t, rec)
		})
	}
}

func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	hits := 0
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/v0/agents/elsewhere", http.StatusFound)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := client.GetAgent(ctx, "bc_1")
	var apiErr *cursorcloud.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusFound, apiErr.StatusCode)
	assert.Equal(t, 1, hits)
	assertNoKey(t, err)
}

func TestHTTPEmptyKeyAndBadBaseURL(t *testing.T) {
	client := cursorcloud.NewHTTPClient("  ", nil, cursorcloud.WithBaseURL("://bad "+testKey))
	_, err := client.GetAgent(context.Background(), "bc_1")
	require.ErrorContains(t, err, "absolute http(s) URL")
	assertNoKey(t, err)

	client = cursorcloud.NewHTTPClient("", cursorcloud.WithBaseURL("http://127.0.0.1:1"))
	_, err = client.ListAgents(context.Background())
	require.ErrorContains(t, err, "API key is empty")
	assertNoKey(t, err)
}

func TestHTTPBaseURLKeepsPathAndStripsUserinfo(t *testing.T) {
	var got *http.Request
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Clone(r.Context())
		return jsonResponse(http.StatusOK, `{"id":"bc_1","name":"n","status":"RUNNING"}`), nil
	})}
	client := cursorcloud.NewHTTPClient(testKey,
		cursorcloud.WithBaseURL("https://alice:"+testKey+"@example.com/prefix"),
		cursorcloud.WithHTTPClient(hc),
	)
	_, err := client.GetAgent(context.Background(), "bc_1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.URL.User, "userinfo must be stripped")
	assert.NotContains(t, got.URL.String(), testKey)
	assert.Equal(t, "/prefix/v0/agents/bc_1", got.URL.Path)
	assert.True(t, strings.HasPrefix(got.Header.Get("Authorization"), "Basic "))
}

func TestHTTPPlainHTTPOnlyForLoopback(t *testing.T) {
	called := false
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("should not dial")
	})}
	client := cursorcloud.NewHTTPClient(testKey, cursorcloud.WithHTTPClient(hc), cursorcloud.WithBaseURL("http://example.com"))
	_, err := client.GetAgent(context.Background(), "bc_1")
	require.ErrorContains(t, err, "loopback")
	assert.False(t, called)
	assert.NotContains(t, err.Error(), testKey)

	client = cursorcloud.NewHTTPClient(testKey, cursorcloud.WithBaseURL("http://127.0.0.1:9"))
	_, err = client.GetAgent(context.Background(), "bc_1")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "loopback")
	assert.NotContains(t, err.Error(), testKey)
}

func TestHTTPCreateDoesNotRetryTransientError(t *testing.T) {
	hits := 0
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v0/agents" {
			hits++
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"message":"unavailable"}`)
			return
		}
		http.NotFound(w, r)
	})
	_, err := client.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{
		Repo:   "https://github.com/acme/widgets",
		Prompt: "review",
	})
	require.Error(t, err)
	assert.Equal(t, 1, hits)
}

func TestHTTPClientIsInjectable(t *testing.T) {
	var called bool
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		assert.True(t, strings.HasPrefix(r.Header.Get("Authorization"), "Basic "))
		return jsonResponse(http.StatusOK, `{"id":"bc_1","name":"n","status":"CREATING"}`), nil
	})}
	client := cursorcloud.NewHTTPClient(testKey, cursorcloud.WithHTTPClient(hc))
	got, err := client.GetAgent(context.Background(), "bc_1")
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, cursorcloud.StatusCreating, got.Status)
}

func TestCreatingStatusIsNotTerminal(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"id": "bc_1", "name": "n", "status": "CREATING"})
	})
	got, err := client.GetAgent(context.Background(), "bc_1")
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusCreating, got.Status)
	assert.NotEqual(t, cursorcloud.StatusFinished, got.Status)
	assert.NotEqual(t, cursorcloud.StatusError, got.Status)
	assert.NotEqual(t, cursorcloud.StatusExpired, got.Status)
}

func TestHTTPContextPropagates(t *testing.T) {
	type ctxKey struct{}
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		if r.Context().Value(ctxKey{}) != "marker" {
			return jsonResponse(http.StatusInternalServerError, `{"message":"missing ctx"}`), nil
		}
		return jsonResponse(http.StatusOK, `{"id":"bc_1","name":"n","status":"RUNNING"}`), nil
	})}
	client := cursorcloud.NewHTTPClient(testKey, cursorcloud.WithHTTPClient(hc))
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	_, err := client.GetAgent(ctx, "bc_1")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.GetAgent(ctx, "bc_1")
	require.ErrorIs(t, err, context.Canceled)
}

func TestHTTPTransportErrorRedactsKeyInURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	client := cursorcloud.NewHTTPClient(testKey, cursorcloud.WithBaseURL(srv.URL))
	_, err := client.GetAgent(context.Background(), testKey)
	require.Error(t, err)
	assertNoKey(t, err)
	assert.NotContains(t, err.Error(), base64.StdEncoding.EncodeToString([]byte(testKey+":")))
}

type requestLog struct {
	reqs []httpRequest
}

type httpRequest struct {
	method string
	auth   string
	url    string
}

func newClient(t *testing.T, handler http.HandlerFunc) (cursorcloud.Client, *requestLog) {
	t.Helper()
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.reqs = append(log.reqs, httpRequest{
			method: r.Method,
			auth:   r.Header.Get("Authorization"),
			url:    r.URL.String(),
		})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return cursorcloud.NewHTTPClient(testKey, nil, cursorcloud.WithBaseURL(srv.URL)), log
}

func assertAuth(t *testing.T, log *requestLog) {
	t.Helper()
	require.NotEmpty(t, log.reqs)
	for _, req := range log.reqs {
		assert.True(t, strings.HasPrefix(req.auth, "Basic "), req.auth)
		assert.NotContains(t, req.url, testKey)
	}
}

func assertNoKey(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testKey)
	apiErr, ok := errors.AsType[*cursorcloud.APIError](err)
	if !ok {
		return
	}
	assert.NotContains(t, apiErr.Message, testKey)
	assert.NotContains(t, apiErr.Code, testKey)
	assert.NotContains(t, apiErr.Op, testKey)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func methods(log *requestLog) []string {
	out := make([]string, len(log.reqs))
	for i, req := range log.reqs {
		out[i] = req.method
	}
	return out
}

func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return body
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(v))
}
