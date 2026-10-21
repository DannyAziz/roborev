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

func TestEnqueueCursorCloudRejectsDirtyAndRangeWhenJobTypeSaysReview(t *testing.T) {
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
				JobType:     storage.JobTypeReview,
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

func TestEnqueueFixRangeWithConfigHostStaysLocal(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
	t.Setenv("ROBOREV_DEFAULT_HOST", "")
	t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
	db, tmpDir := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.MaxWorkers = 0
	cfg.Host = agent.CursorCloudHost
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })

	repo := testutil.InitTestGitRepo(t, filepath.Join(tmpDir, "repo"))
	repo.CommitFile("a.txt", "one\n", "one")
	repo.CommitFile("b.txt", "two\n", "two")
	for _, tc := range []struct {
		name    string
		req     EnqueueRequest
		jobType string
	}{
		{
			name: "fix with prompt",
			req: EnqueueRequest{
				GitRef:       "HEAD~1..HEAD",
				JobType:      storage.JobTypeFix,
				CustomPrompt: "fix the findings",
				Agent:        "test",
			},
			jobType: storage.JobTypeFix,
		},
		{
			name: "task label",
			req: EnqueueRequest{
				GitRef:       "phase..two",
				CustomPrompt: "do the labeled task",
				Agent:        "test",
			},
			jobType: storage.JobTypeTask,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.RepoPath = repo.Path()
			created := enqueueViaHTTP(t, server, req)
			assert.Equal(t, "test", created.Agent)
			assert.Equal(t, tc.jobType, created.JobType)
			stored, err := db.GetJobByID(created.ID)
			require.NoError(t, err)
			assert.Equal(t, "test", stored.Agent)
			assert.Equal(t, tc.jobType, stored.JobType)
			assert.NotEqual(t, agent.CursorCloudHost, stored.Agent)
		})
	}
}

func TestCursorCloudRefusesDeclaredTypeWithoutPromptOnDirtyOrRange(t *testing.T) {
	jobTypes := []string{
		storage.JobTypeReview,
		storage.JobTypeFix,
		storage.JobTypeCompact,
		storage.JobTypeTask,
	}
	refs := []struct {
		name string
		ref  string
		diff string
	}{
		{name: "dirty", ref: "dirty", diff: "diff --git a/a.txt\n"},
		{name: "range", ref: "HEAD~1..HEAD"},
	}
	sources := []struct {
		name   string
		config bool
		flag   bool
	}{
		{name: "config host", config: true},
		{name: "--host", flag: true},
	}
	for _, source := range sources {
		for _, kind := range refs {
			for _, jobType := range jobTypes {
				t.Run(jobType+"/"+kind.name+"/"+source.name, func(t *testing.T) {
					t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
					t.Setenv(agent.EnvDefaultHost, "")
					t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
					db, tmpDir := testutil.OpenTestDBWithDir(t)
					cfg := config.DefaultConfig()
					cfg.MaxWorkers = 0
					if source.config {
						cfg.Host = agent.CursorCloudHost
					}
					server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
					t.Cleanup(func() { _ = server.Close() })
					repo := testutil.InitTestGitRepo(t, filepath.Join(tmpDir, "repo"))
					repo.CommitFile("a.txt", "one\n", "one")
					repo.CommitFile("b.txt", "two\n", "two")
					req := EnqueueRequest{
						RepoPath:    repo.Path(),
						GitRef:      kind.ref,
						JobType:     jobType,
						Agent:       "test",
						DiffContent: kind.diff,
					}
					if source.flag {
						req.Host = agent.CursorCloudHost
					}
					w := enqueueRaw(t, server, req)
					require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
					assert.Contains(t, w.Body.String(), "cannot run a "+kind.name+" review")
					jobs, err := db.ListJobs("", "", 20, 0)
					require.NoError(t, err)
					assert.Empty(t, jobs)
				})
			}
		}
	}
}

func TestPromptRunAndAnalyzeStayLocalForEveryHostSource(t *testing.T) {
	jobs := []struct {
		name   string
		gitRef string
	}{
		{name: "run", gitRef: "run"},
		{name: "analyze", gitRef: "complexity"},
	}
	sources := []struct {
		name   string
		config bool
		flag   bool
		env    bool
	}{
		{name: "config host", config: true},
		{name: "--host", flag: true},
		{name: "ROBOREV_DEFAULT_HOST", env: true},
	}
	for _, job := range jobs {
		for _, source := range sources {
			t.Run(job.name+"/"+source.name, func(t *testing.T) {
				t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret-not-real")
				t.Setenv(agent.EnvDefaultHost, "")
				t.Setenv("ROBOREV_TELEMETRY_ENABLED", "0")
				db, tmpDir := testutil.OpenTestDBWithDir(t)
				cfg := config.DefaultConfig()
				cfg.MaxWorkers = 0
				if source.config {
					cfg.Host = agent.CursorCloudHost
				}
				if source.env {
					t.Setenv(agent.EnvDefaultHost, agent.CursorCloudHost)
				}
				server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
				t.Cleanup(func() { _ = server.Close() })
				repo := testutil.InitTestGitRepo(t, filepath.Join(tmpDir, "repo"))
				repo.CommitFile("a.txt", "one\n", "one")
				req := EnqueueRequest{
					RepoPath:     repo.Path(),
					GitRef:       job.gitRef,
					CustomPrompt: "inspect this repository",
					Agent:        "test",
				}
				if source.flag {
					req.Host = agent.CursorCloudHost
				}
				created := enqueueViaHTTP(t, server, req)
				assert.Equal(t, "test", created.Agent)
				stored, err := db.GetJobByID(created.ID)
				require.NoError(t, err)
				assert.Equal(t, "test", stored.Agent)
				assert.NotEqual(t, agent.CursorCloudHost, stored.Agent)
			})
		}
	}
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
