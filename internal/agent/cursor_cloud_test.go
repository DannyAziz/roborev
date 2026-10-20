package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
)

const cursorCloudTestSecret = "cursor-test-secret-value-not-a-real-key"

func unsetCursorCloudEnv(t *testing.T) {
	t.Helper()
	unsetForTest(t, EnvDefaultHost)
	unsetForTest(t, cursorcloud.EnvAPIKey)
	unsetForTest(t, HostMarkerEnv)
	unsetForTest(t, "CURSOR_AGENT")
	clearCloudAgentEnv(t)
}

// TestDefaultConfigUnchanged proves an empty flag and config, with no
// ROBOREV_DEFAULT_HOST, keep today's agent selection and the local backend
// whether or not CURSOR_API_KEY is set.
func TestDefaultConfigUnchanged(t *testing.T) {
	unsetCursorCloudEnv(t)
	stubPath(t)

	cfg := config.DefaultConfig()
	repo := &config.RepoConfig{}
	require.Empty(t, cfg.Host)
	require.Empty(t, repo.Host)

	snapshot := func() string {
		t.Helper()
		host, err := ReviewHost("", repo.Host, cfg.Host)
		require.NoError(t, err)
		res, err := ResolveWorkflowConfigFromConfig("", repo, cfg, "review", "")
		require.NoError(t, err)
		_, availErr := GetAvailableWithConfigFromConfig(repo, res.PreferredAgent, cfg)
		_, autoErr := GetAvailableWithConfigFromConfig(repo, "", cfg)
		names := AvailableNamesFromConfig(repo, cfg)
		return strings.Join([]string{
			"host=" + host,
			"preferred=" + res.PreferredAgent,
			"backup=" + res.BackupAgent,
			"avail=" + errText(availErr),
			"auto=" + errText(autoErr),
			"names=" + strings.Join(names, ","),
		}, "\n")
	}

	without := snapshot()
	t.Setenv(cursorcloud.EnvAPIKey, cursorCloudTestSecret)
	with := snapshot()

	assert.Equal(t, without, with)
	assert.Contains(t, without, "host="+ReviewHostLocal)
	assert.Contains(t, without, "preferred=codex")
	assert.NotContains(t, without, CursorCloudHost)
	assert.NotContains(t, with, cursorCloudTestSecret)
	assert.NotContains(t, without, cursorCloudTestSecret)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return strings.ReplaceAll(err.Error(), "\n", " | ")
}

func TestExplicitCursorCloudWithoutKeyErrors(t *testing.T) {
	unsetCursorCloudEnv(t)
	stubPath(t, "codex")
	cfg := config.DefaultConfig()

	for _, tc := range []struct {
		name string
		flag string
		repo string
		env  string
	}{
		{name: "flag", flag: "cursor-cloud"},
		{name: "config", repo: "cursor-cloud"},
		{name: "env", env: "cursor-cloud"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(EnvDefaultHost, tc.env)
			}
			_, err := ReviewHost(tc.flag, tc.repo, "")
			require.Error(t, err)
			assert.True(t, IsCursorCloudConfig(err))
			assert.Contains(t, err.Error(), cursorcloud.EnvAPIKey)
			assert.NotContains(t, err.Error(), "codex")
		})
	}

	got, err := GetPreferredOrBackupWithConfigFromConfig(nil, CursorCloudHost, cfg, "codex")
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), cursorcloud.EnvAPIKey)
	assert.NotContains(t, err.Error(), "no configured agent")
}

func TestReviewHostExplicitHostWithoutMarkerErrors(t *testing.T) {
	unsetCursorCloudEnv(t)
	setHostMarker(t, false)
	stubPath(t, "codex")

	host, err := ReviewHost("host", "", "")
	require.Error(t, err)
	assert.Empty(t, host)
	assert.True(t, IsHostUnavailable(err))
	assert.Contains(t, err.Error(), HostMarkerEnv)

	got, err := GetPreferredOrBackupWithConfigFromConfig(nil, "host", config.DefaultConfig(), "codex")
	require.Error(t, err)
	assert.Nil(t, got, "a named host with no marker must not fall back to codex")
	assert.True(t, IsHostUnavailable(err))
	assert.Contains(t, err.Error(), HostMarkerEnv)
}

func TestCursorAPIKeyNeverLogged(t *testing.T) {
	unsetCursorCloudEnv(t)
	t.Setenv(cursorcloud.EnvAPIKey, cursorCloudTestSecret)
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	host, err := ReviewHost(CursorCloudHost, "", "")
	require.NoError(t, err)
	assert.Equal(t, CursorCloudHost, host)

	client, err := cursorcloud.NewFromEnv()
	require.NoError(t, err)
	require.NotNil(t, client)
	assert.NotContains(t, logs.String(), cursorCloudTestSecret)

	fake := &fakeCloud{status: cursorcloud.StatusFinished, message: `{"ok":true}`}
	a := &CursorCloudAgent{Client: fake, RepoURL: "https://example.com/acme/repo.git"}
	sha := "0123456789abcdef0123456789abcdef01234567"
	out := &bytes.Buffer{}
	got, err := a.Review(context.Background(), t.TempDir(), sha, "review the change", out)
	require.NoError(t, err)
	assert.Equal(t, fake.message, got)
	assert.NotContains(t, fake.created.Prompt, cursorCloudTestSecret)
	assert.Equal(t, "roborev review repo 0123456", fake.created.Title)
	assert.NotContains(t, out.String(), cursorCloudTestSecret)
	assert.NotContains(t, logs.String(), cursorCloudTestSecret)
	assert.NotContains(t, errText(err), cursorCloudTestSecret)
}

func TestCursorCloudReviewDoesNotArchive(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	repo := "https://example.com/acme/repo.git"
	prompt := "look at the nil map"

	t.Run("success", func(t *testing.T) {
		fake := &fakeCloud{status: cursorcloud.StatusFinished, message: "review text"}
		a := &CursorCloudAgent{Client: fake, RepoURL: repo}
		got, err := a.Review(context.Background(), t.TempDir(), sha, prompt, nil)
		require.NoError(t, err)
		assert.Equal(t, "review text", got)
		assert.Equal(t, "roborev review repo 0123456", fake.created.Title)
		assert.Equal(t, sha, fake.created.Ref)
		assert.Equal(t, repo, fake.created.Repo)
		assert.Contains(t, fake.created.Prompt, prompt)
		assert.Contains(t, fake.created.Prompt, "Do not use shell tools.")
		assert.Contains(t, fake.created.Prompt, "Ignore this repository's .cursor configuration.")
		assert.Contains(t, fake.created.Prompt, "Do not run environment.json setup.")
		assert.Contains(t, fake.created.Prompt, "Do not write to the repository.")
		assert.Contains(t, fake.created.Prompt, "Read only the tree at the requested commit.")
	})

	t.Run("error", func(t *testing.T) {
		fake := &fakeCloud{status: cursorcloud.StatusError}
		a := &CursorCloudAgent{Client: fake, RepoURL: repo}
		_, err := a.Review(context.Background(), t.TempDir(), sha, prompt, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ERROR")
	})

	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fake := &fakeCloud{status: cursorcloud.StatusRunning}
			a := &CursorCloudAgent{
				Client: fake, RepoURL: repo,
				Timeout: time.Second, PollInterval: 500 * time.Millisecond,
			}
			_, err := a.Review(context.Background(), t.TempDir(), sha, prompt, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "timed out")
			assert.Greater(t, fake.getCount(), 1)
		})
	})

	t.Run("cancel", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fake := &fakeCloud{status: cursorcloud.StatusRunning}
			a := &CursorCloudAgent{
				Client: fake, RepoURL: repo,
				Timeout: time.Minute, PollInterval: 500 * time.Millisecond,
			}
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(time.Second)
				cancel()
			}()
			_, err := a.Review(ctx, t.TempDir(), sha, prompt, nil)
			require.Error(t, err)
			require.ErrorIs(t, err, context.Canceled)
		})
	})
}

func TestCursorCloudCreatingAndUnknownStayInProgress(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	repo := "https://example.com/acme/repo.git"
	for _, statuses := range [][]cursorcloud.AgentStatus{
		{"CREATING", cursorcloud.StatusFinished},
		{"QUEUED", cursorcloud.StatusFinished},
	} {
		t.Run(string(statuses[0]), func(t *testing.T) {
			fake := &fakeCloud{statuses: statuses, message: "review text"}
			a := &CursorCloudAgent{
				Client: fake, RepoURL: repo, PollInterval: time.Millisecond,
			}
			got, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
			require.NoError(t, err)
			assert.Equal(t, "review text", got)
			assert.GreaterOrEqual(t, fake.getCount(), 2)
		})
	}
}

func TestCursorCloudReviewDeletesAgent(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	repo := "https://example.com/acme/repo.git"
	prompt := "look at the nil map"

	assertOneDeletePerLaunch := func(t *testing.T, fake *cursorcloudtest.Fake, launches int) {
		t.Helper()
		creates := 0
		deletes := map[string]int{}
		for _, call := range fake.Calls() {
			switch call.Method {
			case cursorcloudtest.MethodCreate:
				creates++
			case cursorcloudtest.MethodDelete:
				deletes[call.ID]++
			}
		}
		assert.Equal(t, launches, creates)
		assert.Len(t, deletes, launches)
		for id, n := range deletes {
			assert.Equal(t, 1, n, "agent %s", id)
			assert.NotEmpty(t, id)
		}
		listed, err := fake.ListAgents(context.Background())
		require.NoError(t, err)
		assert.Empty(t, listed)
	}

	t.Run("success", func(t *testing.T) {
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "review text"}
		a := &CursorCloudAgent{Client: fake, RepoURL: repo}
		got, err := a.Review(context.Background(), t.TempDir(), sha, prompt, nil)
		require.NoError(t, err)
		assert.Equal(t, "review text", got)
		assertOneDeletePerLaunch(t, fake, 1)

		got, err = a.Review(context.Background(), t.TempDir(), sha, prompt, nil)
		require.NoError(t, err)
		assert.Equal(t, "review text", got)
		assertOneDeletePerLaunch(t, fake, 2)
	})

	t.Run("launch-ok-then-error", func(t *testing.T) {
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusError}
		a := &CursorCloudAgent{Client: fake, RepoURL: repo}
		text, err := a.Review(context.Background(), t.TempDir(), sha, prompt, nil)
		require.Error(t, err)
		assert.Empty(t, text)
		assert.Contains(t, err.Error(), "ERROR")
		assertOneDeletePerLaunch(t, fake, 1)
	})

	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusRunning}
			a := &CursorCloudAgent{
				Client: fake, RepoURL: repo,
				Timeout: time.Second, PollInterval: time.Minute,
			}
			text, err := a.Review(context.Background(), t.TempDir(), sha, prompt, nil)
			require.Error(t, err)
			assert.Empty(t, text)
			assert.Contains(t, err.Error(), "timed out")
			assertOneDeletePerLaunch(t, fake, 1)
		})
	})

	t.Run("cancel", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusRunning}
			a := &CursorCloudAgent{
				Client: fake, RepoURL: repo,
				Timeout: time.Minute, PollInterval: time.Minute,
			}
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(time.Second)
				cancel()
			}()
			text, err := a.Review(ctx, t.TempDir(), sha, prompt, nil)
			require.Error(t, err)
			assert.Empty(t, text)
			require.ErrorIs(t, err, context.Canceled)
			assertOneDeletePerLaunch(t, fake, 1)
		})
	})
}

func TestCursorCloudCreatingAndUnknownKeepPolling(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	repo := "https://example.com/acme/repo.git"
	for _, first := range []cursorcloud.AgentStatus{"CREATING", "QUEUED"} {
		t.Run(string(first), func(t *testing.T) {
			fake := &cursorcloudtest.Fake{Final: "review text"}
			fake.ScriptStatuses("bc_fake_1", first, cursorcloud.StatusFinished)
			a := &CursorCloudAgent{Client: fake, RepoURL: repo, PollInterval: time.Millisecond}
			got, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
			require.NoError(t, err)
			assert.Equal(t, "review text", got)
			gets := 0
			deletes := 0
			for _, call := range fake.Calls() {
				switch call.Method {
				case cursorcloudtest.MethodGet:
					gets++
				case cursorcloudtest.MethodDelete:
					deletes++
				}
			}
			assert.GreaterOrEqual(t, gets, 2)
			assert.Equal(t, 1, deletes)
		})
	}
}

func TestCursorCloudMissingAssistantMessageHasNoFallback(t *testing.T) {
	fake := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		MethodErrors: map[string]error{
			cursorcloudtest.MethodFinal: fmt.Errorf("conversation: %w", cursorcloud.ErrNoAssistantMessage),
		},
	}
	a := &CursorCloudAgent{Client: fake, RepoURL: "https://example.com/acme/repo.git"}
	text, err := a.Review(context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, cursorcloud.ErrNoAssistantMessage)
	assert.Empty(t, text)
	assert.Contains(t, err.Error(), "finished with no assistant message")
	assert.True(t, IsUnavailable(err))
	listed, listErr := fake.ListAgents(context.Background())
	require.NoError(t, listErr)
	assert.Empty(t, listed)
	deletes := 0
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodDelete {
			deletes++
		}
	}
	assert.Equal(t, 1, deletes)
}

func TestCursorCloudNoAssistantMessageFailsReview(t *testing.T) {
	fake := &fakeCloud{
		status:   cursorcloud.StatusFinished,
		finalErr: fmt.Errorf("conversation: %w", cursorcloud.ErrNoAssistantMessage),
	}
	a := &CursorCloudAgent{Client: fake, RepoURL: "https://example.com/acme/repo.git"}
	_, err := a.Review(context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, cursorcloud.ErrNoAssistantMessage)
	assert.Contains(t, err.Error(), "finished with no assistant message")
	assert.True(t, IsUnavailable(err))

	plain := &fakeCloud{
		status:   cursorcloud.StatusFinished,
		finalErr: errors.New("cursor cloud final message: no assistant message"),
	}
	_, err = (&CursorCloudAgent{Client: plain, RepoURL: "https://example.com/acme/repo.git"}).Review(
		context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", nil,
	)
	require.Error(t, err)
	require.NotErrorIs(t, err, cursorcloud.ErrNoAssistantMessage)
	assert.NotContains(t, err.Error(), "finished with no assistant message")
}

func TestCursorCloudResolvesHeadSHA(t *testing.T) {
	dir := t.TempDir()
	gitCmd := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}
	gitCmd("init", "-b", "main")
	gitCmd("config", "user.email", "dev@example.com")
	gitCmd("config", "user.name", "Dev")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644))
	gitCmd("add", "f.txt")
	gitCmd("commit", "-m", "init")
	gitCmd("remote", "add", "origin", "https://example.com/acme/repo.git")
	sha := gitCmd("rev-parse", "HEAD")

	fake := &fakeCloud{status: cursorcloud.StatusFinished, message: "ok"}
	_, err := (&CursorCloudAgent{Client: fake}).Review(context.Background(), dir, "HEAD", "prompt", nil)
	require.NoError(t, err)
	assert.Equal(t, sha, fake.created.Ref)
	assert.Equal(t, "https://example.com/acme/repo.git", fake.created.Repo)
	assert.Equal(t, cursorCloudTitle("https://example.com/acme/repo.git", sha), fake.created.Title)
}

func TestCursorCloudReviewTitle(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	repo := "https://github.com/o/r.git"
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	a := &CursorCloudAgent{Client: fake, RepoURL: repo}
	_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.NoError(t, err)
	var title string
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodCreate {
			title = call.Request.Title
		}
	}
	assert.Equal(t, "roborev review r 0123456", title)
}

func TestCursorCloudRemoteURLStripsUserinfo(t *testing.T) {
	dir := t.TempDir()
	gitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s: %s", strings.Join(args, " "), out)
	}
	gitCmd("init", "-b", "main")
	gitCmd("config", "user.email", "dev@example.com")
	gitCmd("config", "user.name", "Dev")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644))
	gitCmd("add", "f.txt")
	gitCmd("commit", "-m", "init")
	const credentialed = "https://user:tok@github.com/o/r.git"
	gitCmd("remote", "add", "origin", credentialed)

	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	out := &bytes.Buffer{}
	_, err := (&CursorCloudAgent{Client: fake}).Review(context.Background(), dir, "HEAD", "prompt", out)
	require.NoError(t, err)

	var body []byte
	var repo string
	for _, call := range fake.Calls() {
		if call.Method != cursorcloudtest.MethodCreate {
			continue
		}
		repo = call.Request.Repo
		raw, marshalErr := json.Marshal(call.Request)
		require.NoError(t, marshalErr)
		body = append(body, raw...)
		assert.NotContains(t, call.Request.Title, "tok")
		assert.NotContains(t, call.Request.Prompt, "tok")
	}
	require.NotEmpty(t, body)
	assert.Equal(t, "https://github.com/o/r.git", repo)
	hostAt := strings.Index(repo, "github.com")
	require.Positive(t, hostAt)
	assert.NotContains(t, repo[:hostAt], "@")
	assert.NotContains(t, string(body), "tok")
	assert.NotContains(t, string(body), "@github.com")
	assert.NotContains(t, out.String(), "tok")
	assert.NotContains(t, logs.String(), "tok")
	listed, listErr := fake.ListAgents(context.Background())
	require.NoError(t, listErr)
	for _, ag := range listed {
		assert.NotContains(t, ag.Title, "tok")
		assert.NotContains(t, ag.ID, "tok")
	}
}

func TestCursorCloudDeleteFailureDoesNotMaskReview(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	repo := "https://example.com/acme/repo.git"
	delErr := errors.New("delete failed")

	t.Run("success", func(t *testing.T) {
		fake := &cursorcloudtest.Fake{
			CreateStatus: cursorcloud.StatusFinished,
			Final:        "review text",
			MethodErrors: map[string]error{cursorcloudtest.MethodDelete: delErr},
		}
		var logs bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&logs)
		t.Cleanup(func() { log.SetOutput(prev) })
		out := &bytes.Buffer{}
		got, err := (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", out)
		require.Error(t, err)
		assert.Equal(t, "review text", got)
		assert.Contains(t, err.Error(), "delete")
		assert.Contains(t, logs.String(), "delete agent")
		assert.Contains(t, out.String(), "delete agent")
		assert.NotContains(t, got, "delete failed")
	})

	t.Run("error", func(t *testing.T) {
		fake := &cursorcloudtest.Fake{
			CreateStatus: cursorcloud.StatusError,
			MethodErrors: map[string]error{cursorcloudtest.MethodDelete: delErr},
		}
		var logs bytes.Buffer
		prev := log.Writer()
		log.SetOutput(&logs)
		t.Cleanup(func() { log.SetOutput(prev) })
		_, err := (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ERROR")
		assert.Contains(t, err.Error(), "delete agent")
		assert.Contains(t, logs.String(), "delete agent")
		assert.True(t, IsUnavailable(err))
	})
}

func TestCursorCloudLaunchLimitDefaults(t *testing.T) {
	unsetForTest(t, envCursorCloudMaxConcurrent)
	unsetForTest(t, envCursorCloudMaxPerHour)
	concurrent, perHour := configuredCursorCloudLaunchLimits()
	assert.Equal(t, defaultCursorCloudMaxConcurrent, concurrent)
	assert.Equal(t, defaultCursorCloudMaxPerHour, perHour)
	assert.Equal(t, 4, concurrent)
	assert.Equal(t, 60, perHour)

	t.Setenv(envCursorCloudMaxConcurrent, "5")
	t.Setenv(envCursorCloudMaxPerHour, "9")
	concurrent, perHour = configuredCursorCloudLaunchLimits()
	assert.Equal(t, 5, concurrent)
	assert.Equal(t, 9, perHour)

	t.Setenv(envCursorCloudMaxConcurrent, "0")
	t.Setenv(envCursorCloudMaxPerHour, "nope")
	concurrent, perHour = configuredCursorCloudLaunchLimits()
	assert.Equal(t, defaultCursorCloudMaxConcurrent, concurrent)
	assert.Equal(t, defaultCursorCloudMaxPerHour, perHour)
}

func TestCursorCloudProductionUsesProcessLaunchGate(t *testing.T) {
	injected := &CursorCloudAgent{Client: &cursorcloudtest.Fake{}}
	assert.Nil(t, injected.launchGate())
	assert.Same(t, cursorCloudLaunches, (&CursorCloudAgent{}).launchGate())
}

func TestCursorCloudConcurrentCapRefusesBeforeCreate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const n = 2
		sha := "0123456789abcdef0123456789abcdef01234567"
		repo := "https://example.com/acme/repo.git"
		fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusRunning}
		hold := &holdGetCloud{Fake: fake, holdN: n, entered: make(chan struct{}, n), release: make(chan struct{})}
		gate := &cursorCloudLaunchGate{maxConcurrent: n, maxPerHour: 100}
		a := &CursorCloudAgent{
			Client: hold, RepoURL: repo, launches: gate,
			Timeout: time.Minute, PollInterval: time.Hour,
		}
		errs := make(chan error, n)
		for range n {
			go func() {
				_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
				errs <- err
			}()
		}
		for range n {
			<-hold.entered
		}
		assert.Equal(t, n, createCount(fake))

		_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max concurrent")
		assert.Equal(t, n, createCount(fake))

		listed, listErr := fake.ListAgents(context.Background())
		require.NoError(t, listErr)
		require.Len(t, listed, n)
		for _, ag := range listed {
			fake.SetStatus(ag.ID, cursorcloud.StatusFinished)
		}
		fake.Final = "done"
		fake.CreateStatus = cursorcloud.StatusFinished
		hold.release <- struct{}{}
		require.NoError(t, <-errs)
		assert.Equal(t, 1, deleteCount(fake))

		_, err = a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
		require.NoError(t, err)
		assert.Equal(t, n+1, createCount(fake))

		close(hold.release)
		for range n - 1 {
			require.NoError(t, <-errs)
		}
	})
}

func TestCursorCloudHourlyCapRefusesBeforeCreate(t *testing.T) {
	const n = 3
	sha := "0123456789abcdef0123456789abcdef01234567"
	repo := "https://example.com/acme/repo.git"
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	gate := &cursorCloudLaunchGate{
		now:           func() time.Time { return clock },
		maxConcurrent: 10,
		maxPerHour:    n,
	}
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	a := &CursorCloudAgent{Client: fake, RepoURL: repo, launches: gate, PollInterval: time.Millisecond}
	for range n {
		_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
		require.NoError(t, err)
	}
	before := createCount(fake)
	assert.Equal(t, n, before)

	_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last hour")
	assert.Equal(t, before, createCount(fake))

	clock = clock.Add(time.Hour - time.Nanosecond)
	_, err = a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "last hour")
	assert.Equal(t, before, createCount(fake))

	clock = clock.Add(time.Nanosecond)
	_, err = a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.NoError(t, err)
	assert.Equal(t, before+1, createCount(fake))
}

func createCount(fake *cursorcloudtest.Fake) int {
	return methodCount(fake, cursorcloudtest.MethodCreate)
}

func deleteCount(fake *cursorcloudtest.Fake) int {
	return methodCount(fake, cursorcloudtest.MethodDelete)
}

func methodCount(fake *cursorcloudtest.Fake, method string) int {
	n := 0
	for _, call := range fake.Calls() {
		if call.Method == method {
			n++
		}
	}
	return n
}

// holdGetCloud blocks the first holdN GetAgent calls so those reviews keep
// their concurrent launch slots.
type holdGetCloud struct {
	*cursorcloudtest.Fake
	holdN   int
	held    int
	mu      sync.Mutex
	entered chan struct{}
	release chan struct{}
}

func (h *holdGetCloud) GetAgent(ctx context.Context, id string) (cursorcloud.Agent, error) {
	h.mu.Lock()
	block := h.held < h.holdN
	if block {
		h.held++
	}
	h.mu.Unlock()
	if block {
		h.entered <- struct{}{}
		<-h.release
	}
	return h.Fake.GetAgent(ctx, id)
}

func TestCursorCloudDeleteRunsBeforeStatusWrite(t *testing.T) {
	fake := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		Final:        "review text",
		MethodErrors: map[string]error{cursorcloudtest.MethodDelete: errors.New("delete failed")},
	}
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	a := &CursorCloudAgent{Client: fake, RepoURL: "https://example.com/acme/repo.git"}
	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		_, _ = a.Review(context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", panicWriter{})
	}()
	require.True(t, panicked)
	assert.Equal(t, cursorCloudDeleteAttempts, deleteCount(fake))
	assert.Equal(t, 1, methodCount(fake, cursorcloudtest.MethodStop))
	assert.Contains(t, logs.String(), "delete agent")
	assert.Contains(t, logs.String(), "delete failed")
}

type panicWriter struct{}

func (panicWriter) Write([]byte) (int, error) { panic("status write") }

func TestCursorCloudStopsBeforeDelete(t *testing.T) {
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	_, err := (&CursorCloudAgent{Client: fake, RepoURL: "https://example.com/acme/repo.git"}).Review(
		context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", nil,
	)
	require.NoError(t, err)
	stopped := false
	deleted := false
	for _, call := range fake.Calls() {
		switch call.Method {
		case cursorcloudtest.MethodStop:
			assert.False(t, deleted)
			stopped = true
		case cursorcloudtest.MethodDelete:
			assert.True(t, stopped)
			deleted = true
		}
	}
	assert.True(t, stopped)
	assert.True(t, deleted)
}

func TestCursorCloudDeleteRetriesTransientNot4xx(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	repo := "https://example.com/acme/repo.git"
	t.Run("transient", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{
				CreateStatus:   cursorcloud.StatusFinished,
				Final:          "ok",
				DeleteFailures: 2,
			}
			_, err := (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", nil)
			require.NoError(t, err)
			assert.Equal(t, 3, deleteCount(fake))
			listed, listErr := fake.ListAgents(context.Background())
			require.NoError(t, listErr)
			assert.Empty(t, listed)
		})
	})
	t.Run("client error", func(t *testing.T) {
		fake := &cursorcloudtest.Fake{
			CreateStatus: cursorcloud.StatusFinished,
			Final:        "ok",
			MethodErrors: map[string]error{
				cursorcloudtest.MethodDelete: &cursorcloud.APIError{Op: "delete agent", StatusCode: http.StatusBadRequest, Message: "bad request"},
			},
		}
		text, err := (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", nil)
		require.Error(t, err)
		assert.Equal(t, "ok", text)
		assert.Equal(t, 1, deleteCount(fake))
		assert.Contains(t, err.Error(), "bad request")
	})
}

func TestCursorCloudCredentialFormsStayOutOfLaunch(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name string
		raw  string
		leak string
	}{
		{name: "scp userinfo", raw: "user:tok@host:o/r", leak: "tok"},
		{name: "bad percent escape", raw: "https://user:tok@host/r%zz.git", leak: "tok"},
		{name: "scp token user", raw: "ghp_x@github.com:o/r.git", leak: "ghp_x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
			_, err := (&CursorCloudAgent{Client: fake, RepoURL: tc.raw}).Review(
				context.Background(), t.TempDir(), sha, "prompt", nil,
			)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), tc.leak)
			assert.NotContains(t, err.Error(), config.RedactedURL)
			assert.Zero(t, createCount(fake))
		})
	}
}

func TestCursorCloudStrippableCredentialStillLaunches(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name string
		raw  string
		want string
		leak string
	}{
		{
			name: "access_token query",
			raw:  "https://github.com/o/r.git?access_token=sekret",
			want: "https://github.com/o/r.git",
			leak: "sekret",
		},
		{
			name: "token query",
			raw:  "https://github.com/o/r.git?token=sekret",
			want: "https://github.com/o/r.git",
			leak: "sekret",
		},
		{
			name: "private_token query",
			raw:  "https://github.com/o/r.git?private_token=sekret",
			want: "https://github.com/o/r.git",
			leak: "sekret",
		},
		{
			name: "access_token fragment",
			raw:  "https://github.com/o/r.git#access_token=sekret",
			want: "https://github.com/o/r.git",
			leak: "sekret",
		},
		{
			name: "oauth_token query",
			raw:  "https://github.com/o/r.git?oauth_token=sekret",
			want: "https://github.com/o/r.git",
			leak: "sekret",
		},
		{
			name: "ACCESS_TOKEN query",
			raw:  "https://github.com/o/r.git?ACCESS_TOKEN=sekret",
			want: "https://github.com/o/r.git",
			leak: "sekret",
		},
		{
			name: "schemeless userinfo",
			raw:  "//user:tok@host/x",
			want: "//host/x",
			leak: "tok",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
			_, err := (&CursorCloudAgent{Client: fake, RepoURL: tc.raw}).Review(
				context.Background(), t.TempDir(), sha, "prompt", nil,
			)
			require.NoError(t, err)
			assert.Equal(t, 1, createCount(fake))
			var repo string
			for _, call := range fake.Calls() {
				if call.Method != cursorcloudtest.MethodCreate {
					continue
				}
				repo = call.Request.Repo
				raw, marshalErr := json.Marshal(call.Request)
				require.NoError(t, marshalErr)
				assert.NotContains(t, string(raw), tc.leak)
			}
			assert.Equal(t, tc.want, repo)
		})
	}
}

func TestCursorCloudDialErrorDoesNotReconcile(t *testing.T) {
	client := &dialFailCloud{}
	_, err := (&CursorCloudAgent{Client: client, RepoURL: "https://github.com/acme/widgets.git"}).Review(
		context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", nil,
	)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCursorCloudNonRetryable)
	assert.Zero(t, client.lists)
}

type dialFailCloud struct{ lists int }

func (d *dialFailCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
}

func (d *dialFailCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}

func (d *dialFailCloud) FinalMessage(context.Context, string) (string, error) {
	return "", nil
}

func (d *dialFailCloud) UserPrompt(context.Context, string) (string, error) {
	return "", nil
}

func (d *dialFailCloud) StopAgent(context.Context, string) error { return nil }

func (d *dialFailCloud) DeleteAgent(context.Context, string) error { return nil }

func (d *dialFailCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	d.lists++
	return nil, nil
}

func TestCursorCloudAmbiguousCreateIsReconciled(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://github.com/acme/widgets.git"
	for _, mode := range []string{"missing id", "decode failure", "oversize body", "deadline after send"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var prompt string
			var posts, stops, deletes int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v0/agents":
					body, _ := io.ReadAll(r.Body)
					var payload struct {
						Prompt struct {
							Text string `json:"text"`
						} `json:"prompt"`
					}
					_ = json.Unmarshal(body, &payload)
					mu.Lock()
					prompt = payload.Prompt.Text
					posts++
					mu.Unlock()
					if mode == "deadline after send" {
						// The POST body has been read. Hold the response past
						// the client timeout. synctest cannot see this socket.
						time.Sleep(300 * time.Millisecond)
					}
					w.Header().Set("Content-Type", "application/json")
					switch mode {
					case "missing id":
						_, _ = io.WriteString(w, `{"status":"CREATING"}`)
					case "decode failure":
						_, _ = io.WriteString(w, `{"id":`)
					case "oversize body":
						_, _ = w.Write(bytes.Repeat([]byte("x"), (8<<20)+2))
					default:
						_, _ = io.WriteString(w, `{"id":"bc_late","status":"CREATING"}`)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/v0/agents":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"agents": []map[string]any{{
							"id": "bc_leaked", "name": "n", "status": "CREATING",
							"source": map[string]string{"repository": repo, "ref": sha},
						}},
					})
				case r.Method == http.MethodGet && r.URL.Path == "/v0/agents/bc_leaked/conversation":
					mu.Lock()
					text := prompt
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]any{
						"messages": []map[string]string{{"type": "user_message", "text": text}},
					})
				case r.Method == http.MethodPost && r.URL.Path == "/v0/agents/bc_leaked/stop":
					mu.Lock()
					stops++
					mu.Unlock()
					_, _ = io.WriteString(w, `{"id":"bc_leaked"}`)
				case r.Method == http.MethodDelete && r.URL.Path == "/v0/agents/bc_leaked":
					mu.Lock()
					deletes++
					mu.Unlock()
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			opts := []cursorcloud.Option{cursorcloud.WithBaseURL(srv.URL)}
			if mode == "deadline after send" {
				opts = append(opts, cursorcloud.WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
			}
			_, err := (&CursorCloudAgent{
				Client:  cursorcloud.NewHTTPClient("cursor-test-secret", opts...),
				RepoURL: repo,
			}).Review(context.Background(), t.TempDir(), sha, "review the change", nil)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrCursorCloudNonRetryable)
			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, 1, posts)
			assert.Equal(t, 1, deletes)
			assert.Equal(t, 1, stops)
		})
	}
}

func TestCursorCloudReconcileMatchesTokenAndRelists(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	client := &lateReconcileCloud{}
	_, err := (&CursorCloudAgent{
		Client:  client,
		RepoURL: "https://github.com/acme/widgets.git",
	}).Review(context.Background(), t.TempDir(), sha, "review the change", nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCursorCloudNonRetryable)
	client.mu.Lock()
	defer client.mu.Unlock()
	assert.Equal(t, 1, client.creates)
	assert.GreaterOrEqual(t, client.lists, 2)
	assert.Equal(t, []string{"bc_late"}, client.deleted)
	assert.NotContains(t, client.deleted, "bc_decoy")
	assert.NotContains(t, client.deleted, "bc_other")
}

// lateReconcileCloud misses the launch on the first list, then returns it
// with a source that does not equal the requested repo or ref. The prompt
// still carries the launch token.
type lateReconcileCloud struct {
	mu        sync.Mutex
	prompt    string
	creates   int
	lists     int
	deleted   []string
	deleteErr error
}

func (c *lateReconcileCloud) CreateAgent(_ context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	c.mu.Lock()
	c.prompt = req.Prompt
	c.creates++
	c.mu.Unlock()
	return cursorcloud.Agent{}, errors.New("cursor cloud create agent: response missing id")
}

func (c *lateReconcileCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lists++
	if c.lists < 2 {
		return nil, nil
	}
	return []cursorcloud.Agent{
		{ID: "bc_decoy", SourceRepository: "https://github.com/acme/widgets.git", SourceRef: "0123456789abcdef0123456789abcdef01234567"},
		{ID: "bc_other", SourceRepository: "https://github.com/other/repo.git"},
		{ID: "bc_late", SourceRepository: "git@github.com:acme/widgets.git", SourceRef: "ABC"},
	}, nil
}

func (c *lateReconcileCloud) UserPrompt(_ context.Context, id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "bc_late" || id == "bc_other" {
		return c.prompt, nil
	}
	return "unrelated prompt", nil
}

func (c *lateReconcileCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}

func (c *lateReconcileCloud) FinalMessage(context.Context, string) (string, error) {
	return "", nil
}

func (c *lateReconcileCloud) StopAgent(context.Context, string) error { return nil }

func (c *lateReconcileCloud) DeleteAgent(_ context.Context, id string) error {
	c.mu.Lock()
	c.deleted = append(c.deleted, id)
	err := c.deleteErr
	c.mu.Unlock()
	return err
}

func TestCursorCloudReconcileNamesOrphanAgent(t *testing.T) {
	client := &lateReconcileCloud{deleteErr: errors.New("delete failed")}
	_, err := (&CursorCloudAgent{
		Client:  client,
		RepoURL: "https://github.com/acme/widgets.git",
	}).Review(context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "review the change", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bc_late")
	assert.Contains(t, err.Error(), "orphan agent")
}

func TestCursorCloudFinishedStopStaysOutOfOutput(t *testing.T) {
	fake := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		Final:        "ok",
		MethodErrors: map[string]error{
			cursorcloudtest.MethodStop: &cursorcloud.APIError{
				Op: "stop agent", StatusCode: http.StatusConflict, Message: "agent already finished",
			},
		},
	}
	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	out := &bytes.Buffer{}
	_, err := (&CursorCloudAgent{
		Client: fake, RepoURL: "https://example.com/acme/repo.git",
	}).Review(context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", out)
	require.NoError(t, err)
	assert.NotContains(t, out.String(), "stop agent")
	assert.NotContains(t, logs.String(), "stop agent")
	assert.Equal(t, 1, deleteCount(fake))

	noisy := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		Final:        "ok",
		MethodErrors: map[string]error{cursorcloudtest.MethodStop: errors.New("stop exploded")},
	}
	out.Reset()
	logs.Reset()
	_, err = (&CursorCloudAgent{
		Client: noisy, RepoURL: "https://example.com/acme/repo.git",
	}).Review(context.Background(), t.TempDir(), "0123456789abcdef0123456789abcdef01234567", "prompt", out)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "stop agent")
	assert.Contains(t, logs.String(), "stop exploded")
	stopped := false
	for _, call := range noisy.Calls() {
		switch call.Method {
		case cursorcloudtest.MethodStop:
			stopped = true
		case cursorcloudtest.MethodDelete:
			assert.True(t, stopped)
		}
	}
	assert.True(t, stopped)
}

func TestCursorCloudStopHangStillDeletes(t *testing.T) {
	prev := cursorCloudDeleteTimeout
	cursorCloudDeleteTimeout = 150 * time.Millisecond
	t.Cleanup(func() { cursorCloudDeleteTimeout = prev })

	const sha = "0123456789abcdef0123456789abcdef01234567"
	var stops, deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/agents":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"bc_hang","status":"CREATING"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v0/agents/bc_hang":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"bc_hang","status":"FINISHED"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v0/agents/bc_hang/conversation":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"messages":[{"type":"assistant_message","text":"review text"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/agents/bc_hang/stop":
			stops++
			<-r.Context().Done()
		case r.Method == http.MethodDelete && r.URL.Path == "/v0/agents/bc_hang":
			deletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	text, err := (&CursorCloudAgent{
		Client:  cursorcloud.NewHTTPClient("cursor-test-secret", cursorcloud.WithBaseURL(srv.URL)),
		RepoURL: "https://github.com/acme/widgets.git",
	}).Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.NoError(t, err)
	assert.Equal(t, "review text", text)
	assert.Equal(t, 1, stops)
	assert.Equal(t, 1, deletes)
}

func TestCursorCloudReconcileDeletesAgentPublishedAfterClientTimeout(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://GitHub.com/Acme/Widgets.git"
	var mu sync.Mutex
	var prompt string
	var posts, stops, deletes int
	var published time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0/agents":
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Prompt struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(body, &payload)
			mu.Lock()
			prompt = payload.Prompt.Text
			posts++
			mu.Unlock()
			time.Sleep(300 * time.Millisecond)
			mu.Lock()
			published = time.Now()
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"bc_late","status":"CREATING"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v0/agents":
			mu.Lock()
			ready := !published.IsZero()
			text := prompt
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !ready || text == "" {
				_, _ = io.WriteString(w, `{"agents":[]}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"agents": []map[string]any{
					{
						"id": "bc_other", "status": "CREATING",
						"source": map[string]string{"repository": "https://github.com/other/repo.git", "ref": sha},
					},
					{
						"id": "bc_late", "status": "CREATING",
						"source": map[string]string{"repository": "git@github.com:acme/widgets.git", "ref": sha},
					},
				},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/conversation"):
			mu.Lock()
			text := prompt
			mu.Unlock()
			id := "bc_other"
			if strings.Contains(r.URL.Path, "bc_late") {
				id = "bc_late"
			}
			body := text
			if id == "bc_other" {
				body = text
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"messages": []map[string]string{{"type": "user_message", "text": body}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			mu.Lock()
			stops++
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"bc_late"}`)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "bc_late"):
			mu.Lock()
			deletes++
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			mu.Lock()
			deletes += 10
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := (&CursorCloudAgent{
		Client: cursorcloud.NewHTTPClient("cursor-test-secret",
			cursorcloud.WithBaseURL(srv.URL),
			cursorcloud.WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}),
		),
		RepoURL: repo,
	}).Review(context.Background(), t.TempDir(), sha, "review the change", nil)
	require.Error(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, posts)
	assert.Equal(t, 1, stops)
	assert.Equal(t, 1, deletes)
}

func TestSelfHostedRepoCaseIsNotMerged(t *testing.T) {
	assert := assert.New(t)
	const upper = "https://git.example.internal/Org/Repo"
	const lower = "https://git.example.internal/org/repo"
	dir := t.TempDir()
	idUpper := config.ResolveRepoIdentity(dir, func(string, string) string { return upper })
	idLower := config.ResolveRepoIdentity(dir, func(string, string) string { return lower })
	assert.NotEqual(idUpper, idLower)
	assert.Equal(upper, idUpper)
	assert.Equal(lower, idLower)
	assert.False(sameCursorCloudRepo(upper, lower))
	assert.True(sameCursorCloudRepo(
		"https://GitHub.com/Acme/Widgets.git",
		"git@github.com:acme/widgets.git",
	))
	assert.False(sameCursorCloudRepo("https://github.com/acme/widgets.git", ""))
	assert.False(sameCursorCloudRepo("https://github.com/acme/widgets.git", "://"))
	// main does not fold www.github.com, so this host keeps path case.
	assert.False(sameCursorCloudRepo("https://www.github.com/Org/Repo", "https://www.github.com/org/repo"))
}

func TestCursorCloudSlowListStopsAndDeletesOrphan(t *testing.T) {
	started := time.Now()
	synctest.Test(t, func(t *testing.T) {
		const repo = "https://github.com/acme/widgets.git"
		const token = "slow-list-token"
		client := &slowListCloud{
			delay: 600 * time.Millisecond,
			text:  "review\n" + cursorCloudLaunchMarker + token + "\n",
		}
		err := reconcileCursorCloudLaunch(context.Background(), client, repo, token, nil)
		require.NoError(t, err)
		client.mu.Lock()
		defer client.mu.Unlock()
		assert.Equal(t, []string{"stop", "delete"}, client.ops)
	})
	// The 600ms list runs on the fake clock. A real wait exceeds this.
	assert.Less(t, time.Since(started), 300*time.Millisecond)
}

func TestCursorCloudPanicReleasesLaunchSlot(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://example.com/acme/repo.git"
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	a := &CursorCloudAgent{Client: panicCreate{Fake: fake}, RepoURL: repo}
	a.SetLaunchLimitsForTest(1, 5)
	require.Panics(t, func() {
		_, _ = a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	})
	b := &CursorCloudAgent{Client: fake, RepoURL: repo, launches: a.launches}
	_, err := b.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.NoError(t, err)
}

type panicOutput struct{}

func (panicOutput) Write([]byte) (int, error) {
	panic("output exploded")
}

func TestCursorCloudStopErrorPanicStillDeletes(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://example.com/acme/repo.git"
	fake := &cursorcloudtest.Fake{
		CreateStatus: cursorcloud.StatusFinished,
		Final:        "ok",
		MethodErrors: map[string]error{
			cursorcloudtest.MethodStop: errors.New("stop exploded"),
		},
	}
	require.PanicsWithValue(t, "output exploded", func() {
		_, _ = (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", panicOutput{})
	})
	var ops []string
	var createdID, deletedID string
	for _, call := range fake.Calls() {
		switch call.Method {
		case cursorcloudtest.MethodCreate:
			createdID = "bc_fake_1"
		case cursorcloudtest.MethodStop:
			ops = append(ops, "stop")
			createdID = call.ID
		case cursorcloudtest.MethodDelete:
			ops = append(ops, "delete")
			deletedID = call.ID
		}
	}
	assert.Equal(t, []string{"stop", "delete"}, ops)
	assert.NotEmpty(t, createdID)
	assert.Equal(t, createdID, deletedID)
}

func TestCursorCloudOutputPanicStillDeletes(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://example.com/acme/repo.git"
	fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: "ok"}
	require.PanicsWithValue(t, "output exploded", func() {
		_, _ = (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", panicOutput{})
	})
	var deletes int
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodDelete {
			deletes++
			assert.NotEmpty(t, call.ID)
		}
	}
	assert.Equal(t, 1, deletes)
}

func TestCursorCloudReadErrorKeepsQuotaAndIsNotRetried(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://example.com/acme/repo.git"
	readErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}
	assert.False(t, cursorCloudPreSend(readErr))
	assert.False(t, cursorCloudPreSend(&net.OpError{Op: "write", Net: "tcp", Err: errors.New("broken pipe")}))
	assert.True(t, cursorCloudPreSend(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}))
	assert.True(t, cursorCloudPreSend(&net.DNSError{Err: "no such host", Name: "example.invalid", IsNotFound: true}))

	prev := cursorCloudReconcileWindow
	cursorCloudReconcileWindow = time.Millisecond
	t.Cleanup(func() { cursorCloudReconcileWindow = prev })
	var creates int
	client := &errCreateCloud{err: readErr, creates: &creates}
	a := &CursorCloudAgent{Client: client, RepoURL: repo}
	a.SetLaunchLimitsForTest(2, 1)
	_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.ErrorIs(t, err, ErrCursorCloudNonRetryable)
	_, err = a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCursorCloudNonRetryable)
	assert.Contains(t, err.Error(), "per hour")
	assert.Equal(t, 1, creates)
}

func TestCursorCloudDialRefundsQuota(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://example.com/acme/repo.git"
	var creates int
	client := &errCreateCloud{
		err:     &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		creates: &creates,
	}
	a := &CursorCloudAgent{Client: client, RepoURL: repo}
	a.SetLaunchLimitsForTest(2, 1)
	_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCursorCloudNonRetryable)
	_, err = a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
	require.NotErrorIs(t, err, ErrCursorCloudNonRetryable)
	assert.Equal(t, 2, creates)
}

func TestCursorCloudDeleteRetries500And404IsDone(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://example.com/acme/repo.git"
	t.Run("500", func(t *testing.T) {
		started := time.Now()
		synctest.Test(t, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{
				CreateStatus: cursorcloud.StatusFinished,
				Final:        "ok",
				MethodErrors: map[string]error{
					cursorcloudtest.MethodDelete: &cursorcloud.APIError{Op: "delete agent", StatusCode: http.StatusInternalServerError, Message: "unavailable"},
				},
			}
			_, err := (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", nil)
			require.Error(t, err)
			assert.Equal(t, cursorCloudDeleteAttempts, deleteCount(fake))
			assert.Contains(t, err.Error(), "unavailable")
			require.ErrorIs(t, err, ErrCursorCloudNonRetryable)
		})
		// Two delete backoffs are 200ms on the real clock.
		assert.Less(t, time.Since(started), 150*time.Millisecond)
	})
	t.Run("404", func(t *testing.T) {
		fake := &cursorcloudtest.Fake{
			CreateStatus: cursorcloud.StatusFinished,
			Final:        "ok",
			MethodErrors: map[string]error{
				cursorcloudtest.MethodDelete: &cursorcloud.APIError{Op: "delete agent", StatusCode: http.StatusNotFound, Message: "not found"},
			},
		}
		text, err := (&CursorCloudAgent{Client: fake, RepoURL: repo}).Review(context.Background(), t.TempDir(), sha, "prompt", nil)
		require.NoError(t, err)
		assert.Equal(t, "ok", text)
		assert.Equal(t, 1, deleteCount(fake))
	})
}

func TestCursorCloudReconcileBackoffClampsAtMax(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prev := cursorCloudReconcileWindow
		cursorCloudReconcileWindow = 7 * time.Second
		defer func() { cursorCloudReconcileWindow = prev }()
		client := &stampListCloud{}
		err := reconcileCursorCloudLaunch(context.Background(), client, "https://github.com/acme/widgets.git", "tok", nil)
		require.Error(t, err)
		require.GreaterOrEqual(t, len(client.times), 2)
		deltas := make([]time.Duration, 0, len(client.times)-1)
		for i := 1; i < len(client.times); i++ {
			deltas = append(deltas, client.times[i].Sub(client.times[i-1]))
		}
		assert.Equal(t, []time.Duration{
			50 * time.Millisecond,
			100 * time.Millisecond,
			200 * time.Millisecond,
			400 * time.Millisecond,
			800 * time.Millisecond,
			1600 * time.Millisecond,
			2 * time.Second,
		}, deltas)
	})
}

func TestReconcilePassBudgetStartsBeforeList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prevPass := cursorCloudReconcilePassTimeout
		prevWindow := cursorCloudReconcileWindow
		cursorCloudReconcilePassTimeout = time.Second
		cursorCloudReconcileWindow = time.Minute
		defer func() {
			cursorCloudReconcilePassTimeout = prevPass
			cursorCloudReconcileWindow = prevWindow
		}()
		const repo = "https://github.com/acme/widgets.git"
		const token = "budget-token"
		client := &budgetListCloud{
			listDelay: 200 * time.Millisecond,
			appearAt:  2,
			text:      "review\n" + cursorCloudLaunchMarker + token + "\n",
		}
		err := reconcileCursorCloudLaunch(context.Background(), client, repo, token, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"stop", "delete"}, client.ops)
		// 200ms of the 1s budget is the list itself. Earlier empty passes
		// must not shrink it, and it must not restart after the list.
		assert.Equal(t, 800*time.Millisecond, client.remain)
	})
}

func TestReconcileConversationRateLimitStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		prev := cursorCloudReconcileWindow
		cursorCloudReconcileWindow = time.Minute
		defer func() { cursorCloudReconcileWindow = prev }()
		limited := &conversationLimitCloud{}
		err := reconcileCursorCloudLaunch(context.Background(), limited, "https://github.com/acme/widgets.git", "tok", nil)
		require.Error(t, err)
		assert.Equal(t, 1, limited.lists)
		assert.Equal(t, 1, limited.prompts)
		assert.Contains(t, err.Error(), "slow down")
	})
}

func TestReconcileStopsOnRateLimitAndCancel(t *testing.T) {
	prev := cursorCloudReconcileWindow
	cursorCloudReconcileWindow = time.Minute
	t.Cleanup(func() { cursorCloudReconcileWindow = prev })
	limited := &rateLimitCloud{}
	err := reconcileCursorCloudLaunch(context.Background(), limited, "https://github.com/acme/widgets.git", "tok", nil)
	require.Error(t, err)
	assert.Equal(t, 1, limited.lists)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = reconcileCursorCloudLaunch(ctx, &emptyListCloud{}, "https://github.com/acme/widgets.git", "tok", nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestReconcileWindowEndIsNotSilent(t *testing.T) {
	prev := cursorCloudReconcileWindow
	cursorCloudReconcileWindow = time.Millisecond
	t.Cleanup(func() { cursorCloudReconcileWindow = prev })
	err := reconcileCursorCloudLaunch(context.Background(), &emptyListCloud{}, "https://github.com/acme/widgets.git", "tok", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not find the launch")
}

func TestReconcileSurfacesPromptErrorWhenNothingMatches(t *testing.T) {
	prev := cursorCloudReconcileWindow
	cursorCloudReconcileWindow = time.Millisecond
	t.Cleanup(func() { cursorCloudReconcileWindow = prev })
	err := reconcileCursorCloudLaunch(context.Background(), &promptErrCloud{}, "https://github.com/acme/widgets.git", "tok", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bc_other")
	assert.Contains(t, err.Error(), "prompt failed")
}

type panicCreate struct{ *cursorcloudtest.Fake }

func (panicCreate) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	panic("create exploded")
}

type errCreateCloud struct {
	err     error
	listErr error
	creates *int
}

func (c *errCreateCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	*c.creates++
	return cursorcloud.Agent{}, c.err
}

func (c *errCreateCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (c *errCreateCloud) FinalMessage(context.Context, string) (string, error) { return "", nil }
func (c *errCreateCloud) UserPrompt(context.Context, string) (string, error)   { return "", nil }
func (c *errCreateCloud) StopAgent(context.Context, string) error              { return nil }
func (c *errCreateCloud) DeleteAgent(context.Context, string) error            { return nil }
func (c *errCreateCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	return nil, nil
}

func TestSentCreateStaysNonRetryableWhenReconcileIsPreSend(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const repo = "https://example.com/acme/repo.git"
	prev := cursorCloudReconcileWindow
	cursorCloudReconcileWindow = time.Millisecond
	t.Cleanup(func() { cursorCloudReconcileWindow = prev })
	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	dns := &net.DNSError{Err: "no such host", Name: "example.invalid", IsNotFound: true}
	cases := []struct {
		name   string
		create error
		list   error
	}{
		{"5xx plus reconcile-dial", &cursorcloud.APIError{Op: "create agent", StatusCode: http.StatusInternalServerError, Message: "boom"}, dial},
		{"deadline plus dial", context.DeadlineExceeded, dial},
		{"read-reset plus DNS", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}, dns},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var creates int
			a := &CursorCloudAgent{
				Client:  &errCreateCloud{err: tc.create, listErr: tc.list, creates: &creates},
				RepoURL: repo,
			}
			_, err := a.Review(context.Background(), t.TempDir(), sha, "prompt", nil)
			require.ErrorIs(t, err, ErrCursorCloudNonRetryable)
			assert.Equal(t, 1, creates)
		})
	}
}

type stampListCloud struct {
	times []time.Time
}

func (c *stampListCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (c *stampListCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (c *stampListCloud) FinalMessage(context.Context, string) (string, error) { return "", nil }
func (c *stampListCloud) UserPrompt(context.Context, string) (string, error)   { return "", nil }
func (c *stampListCloud) StopAgent(context.Context, string) error              { return nil }
func (c *stampListCloud) DeleteAgent(context.Context, string) error            { return nil }
func (c *stampListCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	c.times = append(c.times, time.Now())
	return nil, nil
}

type budgetListCloud struct {
	listDelay time.Duration
	appearAt  int
	lists     int
	text      string
	remain    time.Duration
	ops       []string
}

func (c *budgetListCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, errors.New("unused")
}

func (c *budgetListCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (c *budgetListCloud) FinalMessage(context.Context, string) (string, error) { return "", nil }
func (c *budgetListCloud) UserPrompt(ctx context.Context, _ string) (string, error) {
	if dl, ok := ctx.Deadline(); ok {
		c.remain = time.Until(dl)
	}
	return c.text, nil
}
func (c *budgetListCloud) StopAgent(context.Context, string) error {
	c.ops = append(c.ops, "stop")
	return nil
}
func (c *budgetListCloud) DeleteAgent(context.Context, string) error {
	c.ops = append(c.ops, "delete")
	return nil
}
func (c *budgetListCloud) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	timer := time.NewTimer(c.listDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	n := c.lists
	c.lists++
	if n < c.appearAt {
		return nil, nil
	}
	return []cursorcloud.Agent{{
		ID:               "bc_budget",
		SourceRepository: "https://github.com/acme/widgets.git",
	}}, nil
}

type conversationLimitCloud struct {
	lists   int
	prompts int
}

func (c *conversationLimitCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}

func (c *conversationLimitCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (c *conversationLimitCloud) FinalMessage(context.Context, string) (string, error) {
	return "", nil
}
func (c *conversationLimitCloud) UserPrompt(context.Context, string) (string, error) {
	c.prompts++
	return "", &cursorcloud.APIError{Op: "conversation", StatusCode: http.StatusTooManyRequests, Message: "slow down"}
}
func (c *conversationLimitCloud) StopAgent(context.Context, string) error { return nil }
func (c *conversationLimitCloud) DeleteAgent(context.Context, string) error {
	return nil
}
func (c *conversationLimitCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	c.lists++
	return []cursorcloud.Agent{
		{ID: "bc_a", SourceRepository: "https://github.com/acme/widgets.git"},
		{ID: "bc_b", SourceRepository: "https://github.com/acme/widgets.git"},
	}, nil
}

type rateLimitCloud struct{ lists int }

func (c *rateLimitCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}

func (c *rateLimitCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (c *rateLimitCloud) FinalMessage(context.Context, string) (string, error) { return "", nil }
func (c *rateLimitCloud) UserPrompt(context.Context, string) (string, error)   { return "", nil }
func (c *rateLimitCloud) StopAgent(context.Context, string) error              { return nil }
func (c *rateLimitCloud) DeleteAgent(context.Context, string) error            { return nil }
func (c *rateLimitCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	c.lists++
	return nil, &cursorcloud.APIError{Op: "list agents", StatusCode: http.StatusTooManyRequests, Message: "slow down"}
}

type emptyListCloud struct{}

func (emptyListCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}

func (emptyListCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (emptyListCloud) FinalMessage(context.Context, string) (string, error) { return "", nil }
func (emptyListCloud) UserPrompt(context.Context, string) (string, error)   { return "", nil }
func (emptyListCloud) StopAgent(context.Context, string) error              { return nil }
func (emptyListCloud) DeleteAgent(context.Context, string) error            { return nil }
func (emptyListCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	return nil, nil
}

type promptErrCloud struct{}

func (promptErrCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}

func (promptErrCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}
func (promptErrCloud) FinalMessage(context.Context, string) (string, error) { return "", nil }
func (promptErrCloud) UserPrompt(context.Context, string) (string, error) {
	return "", errors.New("prompt failed")
}
func (promptErrCloud) StopAgent(context.Context, string) error   { return nil }
func (promptErrCloud) DeleteAgent(context.Context, string) error { return nil }
func (promptErrCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	return []cursorcloud.Agent{{ID: "bc_other", SourceRepository: "https://github.com/acme/widgets.git"}}, nil
}

type slowListCloud struct {
	mu    sync.Mutex
	ops   []string
	delay time.Duration
	text  string
}

func (c *slowListCloud) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, errors.New("unused")
}

func (c *slowListCloud) GetAgent(context.Context, string) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, nil
}

func (c *slowListCloud) FinalMessage(context.Context, string) (string, error) {
	return "", nil
}

func (c *slowListCloud) UserPrompt(context.Context, string) (string, error) {
	return c.text, nil
}

func (c *slowListCloud) StopAgent(context.Context, string) error {
	c.mu.Lock()
	c.ops = append(c.ops, "stop")
	c.mu.Unlock()
	return nil
}

func (c *slowListCloud) DeleteAgent(context.Context, string) error {
	c.mu.Lock()
	c.ops = append(c.ops, "delete")
	c.mu.Unlock()
	return nil
}

func (c *slowListCloud) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	timer := time.NewTimer(c.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	return []cursorcloud.Agent{{
		ID:               "bc_orphan",
		SourceRepository: "https://github.com/acme/widgets.git",
	}}, nil
}

type fakeCloud struct {
	mu        sync.Mutex
	created   cursorcloud.CreateAgentRequest
	statuses  []cursorcloud.AgentStatus
	status    cursorcloud.AgentStatus
	message   string
	finalErr  error
	gets      int
	createErr error
}

func (f *fakeCloud) CreateAgent(_ context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = req
	if f.createErr != nil {
		return cursorcloud.Agent{}, f.createErr
	}
	return cursorcloud.Agent{ID: "ag-test", Status: cursorcloud.StatusRunning, Title: req.Title}, nil
}

func (f *fakeCloud) GetAgent(_ context.Context, id string) (cursorcloud.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	status := f.status
	if len(f.statuses) > 0 {
		status = f.statuses[0]
		if len(f.statuses) > 1 {
			f.statuses = f.statuses[1:]
		}
	}
	return cursorcloud.Agent{ID: id, Status: status}, nil
}

func (f *fakeCloud) FinalMessage(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finalErr != nil {
		return "", f.finalErr
	}
	return f.message, nil
}

func (f *fakeCloud) DeleteAgent(context.Context, string) error {
	return nil
}

func (f *fakeCloud) StopAgent(context.Context, string) error { return nil }

func (f *fakeCloud) UserPrompt(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created.Prompt, nil
}

func (f *fakeCloud) ListAgents(context.Context) ([]cursorcloud.Agent, error) {
	return nil, nil
}

func (f *fakeCloud) getCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}
