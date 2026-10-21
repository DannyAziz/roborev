package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

const outsideFileSecret = "OUTSIDE-FILE-SECRET-7d3e"

// processOutsideFileJob runs one claimed job through the real worker.
func processOutsideFileJob(t *testing.T, server *Server, id int64) {
	t.Helper()
	claimed, err := server.db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, id, claimed.ID)
	done := make(chan struct{})
	go func() {
		server.workerPool.processJob(testWorkerID, claimed)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("processJob did not return")
	}
}

// assertNoOutsideSecret fails when secret appears in any request body the
// fake Cursor Cloud endpoint recorded.
func assertNoOutsideSecret(t *testing.T, fake *cursorcloudtest.Fake, secret string) {
	t.Helper()
	for _, call := range fake.Calls() {
		raw, err := json.Marshal(call)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), secret, "method %s", call.Method)
	}
}

// TestCursorCloudCustomReviewFileOutsideRepoRefused pins that a cursor-cloud
// review refuses, before create, a custom review template or include that
// names or resolves to a file outside the repository.
func TestCursorCloudCustomReviewFileOutsideRepoRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics")
	}
	for _, tc := range []struct {
		name string
		// spec is the [review.types.leak] body; outside is the directory
		// holding leak.md, a sibling of the repository.
		spec  func(outside string) string
		setup func(t *testing.T, repo *testutil.TestRepo, outside string)
	}{
		{name: "absolute template", spec: func(outside string) string {
			return fmt.Sprintf("template = %q\n", filepath.Join(outside, "leak.md"))
		}},
		{name: "home template", spec: func(string) string { return "template = \"~/leak.md\"\n" }},
		{name: "dot-dot template", spec: func(outside string) string {
			return fmt.Sprintf("template = %q\n", "../"+filepath.Base(outside)+"/leak.md")
		}},
		{name: "committed symlink template", spec: func(string) string {
			return "template = \"rubric.md\"\n"
		}, setup: func(t *testing.T, repo *testutil.TestRepo, outside string) {
			require.NoError(t, os.Symlink(filepath.Join(outside, "leak.md"), filepath.Join(repo.Path(), "rubric.md")))
			repo.RunGit("add", "rubric.md")
			repo.RunGit("commit", "-m", "link")
		}},
		{name: "symlinked directory template", spec: func(string) string {
			return "template = \"shared/leak.md\"\n"
		}, setup: func(t *testing.T, repo *testutil.TestRepo, outside string) {
			require.NoError(t, os.Symlink(outside, filepath.Join(repo.Path(), "shared")))
			repo.RunGit("add", "shared")
			repo.RunGit("commit", "-m", "link")
		}},
		{name: "absolute include", spec: func(outside string) string {
			return fmt.Sprintf("template = \"r.tmpl\"\n[review.types.leak.includes]\nrubric = %q\n", filepath.Join(outside, "leak.md"))
		}, setup: func(t *testing.T, repo *testutil.TestRepo, _ string) {
			repo.CommitFile("r.tmpl", "{{ index .Includes \"rubric\" }}", "tmpl")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "leak.md"), []byte("Rubric: "+outsideFileSecret+"\n"), 0o600))
			server, fake := cursorCloudPinServer(t, nil)
			parent := t.TempDir()
			repo := testutil.InitTestGitRepo(t, filepath.Join(parent, "repo"))
			outside := filepath.Join(parent, "outside")
			require.NoError(t, os.MkdirAll(outside, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(outside, "leak.md"), []byte("Rubric: "+outsideFileSecret+"\n"), 0o600))
			repo.CommitFile(".roborev.toml", "host = \"cursor-cloud\"\n[review.types.leak]\n"+tc.spec(outside), "cfg")
			if tc.setup != nil {
				tc.setup(t, repo, outside)
			}
			repo.CommitFile("a.txt", "one\n", "one")

			created := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", ReviewType: "leak"})
			require.Equal(t, agent.CursorCloudHost, created.Agent)
			// A prompt build error is retried; run every attempt to the end.
			server.workerPool.retryBackoff = 0
			job := &created
			for range 5 {
				processOutsideFileJob(t, server, created.ID)
				var err error
				job, err = server.db.GetJobByID(created.ID)
				require.NoError(t, err)
				if job.Status != storage.JobStatusQueued {
					break
				}
			}

			assert.Zero(t, createCountFromFake(fake))
			assertNoOutsideSecret(t, fake, outsideFileSecret)
			assert.Equal(t, storage.JobStatusFailed, job.Status)
			assert.Contains(t, job.Error, "outside the repository")
		})
	}
}

// TestCursorCloudCustomReviewFileInsideRepoStillSent guards that a template
// and includes inside the repository, including through an in-repository
// symlink, still reach the cursor-cloud prompt.
func TestCursorCloudCustomReviewFileInsideRepoStillSent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics")
	}
	server, fake := cursorCloudPinServer(t, nil)
	repo := testutil.InitTestGitRepo(t, filepath.Join(t.TempDir(), "repo"))
	repo.CommitFile(".roborev.toml", "host = \"cursor-cloud\"\n[review.types.inrepo]\ntemplate = \".roborev/reviews/r.tmpl\"\n"+
		"[review.types.inrepo.includes]\nrubric = \".roborev/reviews/link.md\"\n", "cfg")
	repo.CommitFile(".roborev/reviews/r.tmpl", "Template marker IN-REPO-TMPL.\n{{ index .Includes \"rubric\" }}", "tmpl")
	repo.CommitFile(".roborev/reviews/rubric.md", "Include marker IN-REPO-RUBRIC.", "rubric")
	require.NoError(t, os.Symlink("rubric.md", filepath.Join(repo.Path(), ".roborev", "reviews", "link.md")))
	repo.RunGit("add", ".roborev/reviews/link.md")
	repo.RunGit("commit", "-m", "link")
	repo.CommitFile("a.txt", "one\n", "one")

	created := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", ReviewType: "inrepo"})
	require.Equal(t, agent.CursorCloudHost, created.Agent)
	processOutsideFileJob(t, server, created.ID)

	require.Equal(t, 1, createCountFromFake(fake))
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodCreate {
			assert.Contains(t, call.Request.Prompt, "IN-REPO-TMPL")
			assert.Contains(t, call.Request.Prompt, "IN-REPO-RUBRIC")
		}
	}
}
