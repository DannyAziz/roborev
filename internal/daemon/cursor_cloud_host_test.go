package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEnqueueCursorCloudRejectsDirtyAndRangeWithoutJobType(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	server, db, tmpDir := newTestServer(t)
	repo := testutil.InitTestGitRepo(t, filepath.Join(tmpDir, "repo"))
	repo.CommitFile("a.txt", "one\n", "one")
	repo.CommitFile("b.txt", "two\n", "two")

	cases := []struct {
		name   string
		gitRef string
		diff   string
		want   string
	}{
		{name: "dirty", gitRef: "dirty", diff: "diff --git a/a.txt\n", want: "dirty"},
		{name: "range", gitRef: "HEAD~1..HEAD", want: "range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
				RepoPath:    repo.Path(),
				GitRef:      tc.gitRef,
				Host:        agent.CursorCloudHost,
				DiffContent: tc.diff,
			})
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), "cannot run a "+tc.want+" review")
		})
	}
	jobs, err := db.ListJobs("", "", 20, 0)
	require.NoError(t, err)
	assert.Empty(t, jobs)
}

func TestResolveSingleAgentCursorCloudRequiresKey(t *testing.T) {
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	t.Setenv(cursorcloud.EnvAPIKey, "")
	execution, early := (&Server{}).resolveSingleAgent(singleAgentInputs{
		req:      EnqueueRequest{Host: "cursor-cloud"},
		cfg:      config.DefaultConfig(),
		repoCfg:  &config.RepoConfig{},
		workflow: "review",
	})
	require.NotNil(t, early)
	assert.Equal(t, http.StatusBadRequest, early.Status)
	assert.Empty(t, execution.Agent)
	body, ok := early.Body.(ErrorResponse)
	require.True(t, ok)
	assert.Contains(t, body.Error, cursorcloud.EnvAPIKey)
	assert.NotContains(t, body.Error, "codex")
}

func TestResolveSingleAgentCursorCloudNoLocalFallback(t *testing.T) {
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-value-not-a-real-key")
	execution, early := (&Server{}).resolveSingleAgent(singleAgentInputs{
		req:      EnqueueRequest{Host: "cursor-cloud"},
		cfg:      config.DefaultConfig(),
		repoCfg:  &config.RepoConfig{},
		workflow: "review",
	})
	require.Nil(t, early)
	assert.Equal(t, "cursor-cloud", execution.Agent)
	assert.Empty(t, execution.BackupAgent)
	assert.NotContains(t, execution.Model, "cursor-test-secret")
}

func TestCursorCloudRejectsDirtyAndRangeBeforeSelection(t *testing.T) {
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-value-not-a-real-key")
	cfg := config.DefaultConfig()
	for _, jobType := range []string{storage.JobTypeDirty, storage.JobTypeRange} {
		t.Run(jobType, func(t *testing.T) {
			execution, early := (&Server{}).resolveSingleAgent(singleAgentInputs{
				req:      EnqueueRequest{Host: agent.CursorCloudHost, JobType: jobType},
				cfg:      cfg,
				repoCfg:  &config.RepoConfig{},
				workflow: "review",
			})
			require.NotNil(t, early)
			assert.Equal(t, http.StatusBadRequest, early.Status)
			assert.Empty(t, execution.Agent)
			body, ok := early.Body.(ErrorResponse)
			require.True(t, ok)
			assert.Contains(t, body.Error, agent.CursorCloudHost)
			assert.Contains(t, body.Error, jobType)
			assert.NotContains(t, body.Error, "codex")
		})
	}

	execution, early := (&Server{}).resolveSingleAgent(singleAgentInputs{
		req:      EnqueueRequest{Host: agent.CursorCloudHost, JobType: storage.JobTypeReview},
		cfg:      cfg,
		repoCfg:  &config.RepoConfig{},
		workflow: "review",
	})
	require.Nil(t, early)
	assert.Equal(t, agent.CursorCloudHost, execution.Agent)
}

func TestResolveSingleAgentHostWithoutMarker(t *testing.T) {
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	t.Setenv("ROBOREV_HOST_AGENT", "")
	t.Setenv("CURSOR_AGENT", "")
	execution, early := (&Server{}).resolveSingleAgent(singleAgentInputs{
		req:      EnqueueRequest{Host: "host"},
		cfg:      config.DefaultConfig(),
		repoCfg:  &config.RepoConfig{},
		workflow: "review",
	})
	require.NotNil(t, early)
	assert.Equal(t, http.StatusBadRequest, early.Status)
	assert.Empty(t, execution.Agent)
	body, ok := early.Body.(ErrorResponse)
	require.True(t, ok)
	assert.Contains(t, body.Error, "ROBOREV_HOST_AGENT")
}
