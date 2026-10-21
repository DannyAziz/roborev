package cursorcloudtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/cursorcloud"
)

func TestFakeLifecycleAndCalls(t *testing.T) {
	fake := &Fake{CreateStatus: cursorcloud.StatusRunning}
	ctx := context.Background()
	req := cursorcloud.CreateAgentRequest{
		Repo:   "https://github.com/acme/widgets",
		Ref:    "abc",
		Prompt: "review",
		Title:  "widgets review",
	}
	created, err := fake.CreateAgent(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "bc_fake_1", created.ID)
	assert.Equal(t, cursorcloud.StatusRunning, created.Status)
	assert.Empty(t, created.Title)

	fake.ScriptStatuses(created.ID, cursorcloud.StatusRunning, cursorcloud.StatusFinished)
	first, err := fake.GetAgent(ctx, created.ID)
	require.NoError(t, err)
	second, err := fake.GetAgent(ctx, created.ID)
	require.NoError(t, err)
	third, err := fake.GetAgent(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusRunning, first.Status)
	assert.Equal(t, cursorcloud.StatusFinished, second.Status)
	assert.Equal(t, cursorcloud.StatusFinished, third.Status)

	fake.SetFinalMessage(created.ID, "ship it")
	text, err := fake.FinalMessage(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "ship it", text)

	other, err := fake.CreateAgent(ctx, req)
	require.NoError(t, err)
	listed, err := fake.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, created.ID, listed[0].ID)
	assert.Equal(t, other.ID, listed[1].ID)

	calls := fake.Calls()
	require.GreaterOrEqual(t, len(calls), 2)
	assert.Equal(t, MethodCreate, calls[0].Method)
	assert.Equal(t, req, calls[0].Request)
	assert.Equal(t, MethodGet, calls[1].Method)
	assert.Equal(t, created.ID, calls[1].ID)
}

func TestFakeDefaultFinalAndErrors(t *testing.T) {
	fake := &Fake{Final: "default review"}
	fake.Seed(cursorcloud.Agent{ID: "bc_seed", Status: cursorcloud.StatusError, Title: "seeded"})
	ctx := context.Background()

	text, err := fake.FinalMessage(ctx, "bc_seed")
	require.NoError(t, err)
	assert.Equal(t, "default review", text)

	fake.SetStatus("bc_seed", cursorcloud.StatusExpired)
	got, err := fake.GetAgent(ctx, "bc_seed")
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusExpired, got.Status)

	boom := errors.New("injected")
	fake.MethodErrors = map[string]error{MethodGet: boom}
	_, err = fake.GetAgent(ctx, "bc_seed")
	require.ErrorIs(t, err, boom)
	listed, err := fake.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	fake.Error = errors.New("all down")
	_, err = fake.ListAgents(ctx)
	require.ErrorContains(t, err, "all down")
	calls := fake.Calls()
	assert.Equal(t, MethodList, calls[len(calls)-1].Method)

	_, err = fake.FinalMessage(ctx, "missing")
	require.Error(t, err)
	fake.Error = nil
	fake.MethodErrors = nil
	_, err = fake.FinalMessage(ctx, "missing")
	var apiErr *cursorcloud.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 404, apiErr.StatusCode)

	fake.Final = ""
	_, err = fake.FinalMessage(ctx, "bc_seed")
	require.ErrorIs(t, err, cursorcloud.ErrNoAssistantMessage)
}

// TestFakeMatchesHTTPClient runs the same cases against the fake and an
// httptest-backed real client so the two cannot drift apart.
func TestFakeMatchesHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v0/agents/missing" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	clients := map[string]cursorcloud.Client{
		"fake": &Fake{},
		"http": cursorcloud.NewHTTPClient("k", cursorcloud.WithBaseURL(srv.URL)),
	}
	ctx := context.Background()
	cases := []struct {
		name       string
		call       func(cursorcloud.Client) error
		wantErr    string
		wantStatus int
	}{
		{"nil context", func(c cursorcloud.Client) error {
			_, err := c.GetAgent(nil, "bc_1") //nolint:staticcheck // SA1012: nil context is the case under test.
			return err
		}, "nil context", 0},
		{"empty repo", func(c cursorcloud.Client) error {
			_, err := c.CreateAgent(ctx, cursorcloud.CreateAgentRequest{Prompt: "p"})
			return err
		}, "repository is empty", 0},
		{"empty prompt", func(c cursorcloud.Client) error {
			_, err := c.CreateAgent(ctx, cursorcloud.CreateAgentRequest{Repo: "r"})
			return err
		}, "prompt is empty", 0},
		{"empty id", func(c cursorcloud.Client) error {
			_, err := c.GetAgent(ctx, " ")
			return err
		}, "agent id is empty", 0},
		{"invalid id", func(c cursorcloud.Client) error {
			_, err := c.FinalMessage(ctx, "../x")
			return err
		}, "agent id is invalid", 0},
		{"missing agent", func(c cursorcloud.Client) error {
			_, err := c.GetAgent(ctx, "missing")
			return err
		}, "status 404", http.StatusNotFound},
	}
	for _, tc := range cases {
		for name, client := range clients {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				err := tc.call(client)
				require.ErrorContains(t, err, tc.wantErr)
				if tc.wantStatus != 0 {
					var apiErr *cursorcloud.APIError
					require.ErrorAs(t, err, &apiErr)
					assert.Equal(t, tc.wantStatus, apiErr.StatusCode)
				}
			})
		}
	}
}

func TestFakeCreateMatchesV0Launch(t *testing.T) {
	fake := &Fake{}
	created, err := fake.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{
		Repo: "https://example.com/acme/repo.git", Prompt: "review", Title: "ignored",
	})
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusCreating, created.Status)
	assert.Empty(t, created.Title)
}
