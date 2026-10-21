package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// defaultPathEnv clears every variable that can opt into the host agent, so
// the test sees the environment of a user who never set it up.
func defaultPathEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ROBOREV_HOST_AGENT", "")
	require.NoError(t, os.Unsetenv("ROBOREV_HOST_AGENT"))
}

func defaultPathStubs(t *testing.T, commands ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, c := range commands {
		require.NoError(t, os.WriteFile(filepath.Join(dir, c), []byte("#!/bin/sh\nexit 1\n"), 0o755))
	}
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	require.NoError(t, os.Symlink(gitPath, filepath.Join(dir, "git")))
	t.Setenv("PATH", dir)
}

func defaultPathResult(name string, err error) string {
	if err != nil {
		return "error: " + strings.ReplaceAll(err.Error(), "\n", " | ")
	}
	return name
}

// With no host or auto settings and no marker, agent resolution, CI pinning,
// retries and failover match the upstream base byte for byte. The golden
// transcript below was generated on 00316ee7 and must not change.
func TestDefaultPathUnchanged(t *testing.T) {
	configs := []struct {
		name string
		set  func(*config.Config)
	}{
		{"default config", func(*config.Config) {}},
		{"named agent", func(c *config.Config) { c.DefaultAgent = "claude" }},
		{"named agent with model", func(c *config.Config) { c.DefaultAgent = "claude"; c.DefaultModel = "sonnet" }},
		{"backup agent", func(c *config.Config) { c.DefaultAgent = "codex"; c.DefaultBackupAgent = "claude" }},
		{"backup agent with model", func(c *config.Config) {
			c.DefaultAgent = "codex"
			c.DefaultBackupAgent = "claude"
			c.DefaultBackupModel = "sonnet"
		}},
		{"review workflow agent", func(c *config.Config) { c.ReviewAgent = "gemini"; c.ReviewModel = "pro" }},
		{"fix pin", func(c *config.Config) { c.FixAgent = "codex"; c.FixBackupAgent = "gemini" }},
		{"unknown agent", func(c *config.Config) { c.DefaultAgent = "bogus" }},
	}
	paths := [][]string{nil, {"codex"}, {"claude"}, {"codex", "claude"}, {"gemini"}, {"codex", "claude", "gemini"}}

	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	for _, cc := range configs {
		for _, path := range paths {
			label := fmt.Sprintf("%s / PATH=%s", cc.name, strings.Join(path, ","))
			t.Run(label, func(t *testing.T) {
				defaultPathEnv(t)
				defaultPathStubs(t, path...)
				cfg := config.DefaultConfig()
				cc.set(cfg)
				repoCfg := &config.RepoConfig{}
				add("== %s", label)
				for _, workflow := range []string{"review", "fix"} {
					res, err := agent.ResolveWorkflowConfigFromConfig("", repoCfg, cfg, workflow, "")
					if err != nil {
						add("%s workflow: %s", workflow, defaultPathResult("", err))
						continue
					}
					add("%s workflow: preferred=%q backup=%q", workflow, res.PreferredAgent, res.BackupAgent)
					var backups []string
					if res.BackupAgent != "" {
						backups = append(backups, res.BackupAgent)
					}
					a, err := agent.GetPreferredOrBackupWithConfigFromConfig(repoCfg, res.PreferredAgent, cfg, backups...)
					name := ""
					if err == nil {
						name = a.Name() + " model=" + res.ModelForSelectedAgent(a.Name(), "")
					}
					add("%s strict: %s", workflow, defaultPathResult(name, err))
					a, err = agent.GetAvailableWithConfigFromConfig(repoCfg, res.PreferredAgent, cfg, backups...)
					if err == nil {
						name = a.Name()
					}
					add("%s available: %s", workflow, defaultPathResult(name, err))
					a, err = agent.GetAvailable(res.PreferredAgent, backups...)
					if err == nil {
						name = a.Name()
					}
					add("%s registry: %s", workflow, defaultPathResult(name, err))
				}
				add("names: %s", strings.Join(agent.AvailableNamesFromConfig(repoCfg, cfg), ","))

				h := newCIPollerHarness(t, "git@github.com:acme/api.git")
				for _, member := range []string{"", "codex", "claude"} {
					name, model, backup, backupModel, err := h.Poller.resolveMatrixMemberAgent(
						h.Repo, nil, cfg, config.AgentReviewType{Agent: member, ReviewType: "default"}, "thorough",
					)
					add("ci member %q: %s", member, defaultPathResult(fmt.Sprintf("agent=%s model=%s backup=%s backup_model=%s", name, model, backup, backupModel), err))
				}

				for _, stored := range []string{"", "claude", "gemini"} {
					add("failover codex stored backup %q: %s", stored, defaultPathFailover(t, cfg, stored, false))
					add("retry codex stored backup %q: %s", stored, defaultPathFailover(t, cfg, stored, true))
				}
				// HTTP enqueue, not db.EnqueueJob: a named backup must stay
				// empty here. Pinning it on every ordinary enqueue turns these
				// rows red.
				add("enqueue: %s", defaultPathEnqueue(t, cfg))
			})
		}
	}

	got := strings.Join(lines, "\n") + "\n"
	if out := os.Getenv("ROBOREV_DEFAULT_PATH_GOLDEN"); out != "" {
		require.NoError(t, os.WriteFile(out, []byte(got), 0o644))
		return
	}
	assert.Equal(t, defaultPathGolden, got)
}

// defaultPathEnqueue queues one ordinary review through /api/enqueue and
// reports the stored agent, model, and backup pin.
func defaultPathEnqueue(t *testing.T, cfg *config.Config) string {
	t.Helper()
	server, db, _ := newTestServer(t)
	*server.configWatcher.Config() = *cfg
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("main.go", "package main\n", "add main")
	rec := enqueueRaw(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD", Panel: config.PanelNone,
	})
	if rec.Code != http.StatusCreated {
		var body ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
			return fmt.Sprintf("status=%d body=%s", rec.Code, strings.ReplaceAll(strings.TrimSpace(rec.Body.String()), "\n", " | "))
		}
		return fmt.Sprintf("status=%d error=%s", rec.Code, strings.ReplaceAll(body.Error, "\n", " | "))
	}
	var created storage.ReviewJob
	testutil.DecodeJSON(t, rec, &created)
	stored, err := db.GetJobByID(created.ID)
	require.NoError(t, err)
	return fmt.Sprintf("agent=%q model=%q backup=%q backup_model=%q backup_auto=%t",
		stored.Agent, stored.Model, stored.BackupAgent, stored.BackupModel, stored.BackupAuto)
}

// defaultPathFailover fails a codex job once (non-retryable) or until it
// stops being retried, and reports each state it passed through.
func defaultPathFailover(t *testing.T, cfg *config.Config, storedBackup string, retry bool) string {
	t.Helper()
	wtc := newWorkerTestContext(t, 1)
	wtc.reconfigurePool(cfg)
	sha := testutil.GetHeadSHA(t, wtc.TmpDir)
	commit, err := wtc.DB.GetOrCreateCommit(wtc.Repo.ID, sha, "Author", "Subject", time.Now())
	require.NoError(t, err)
	_, err = wtc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: wtc.Repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "codex", BackupAgent: storedBackup,
	})
	require.NoError(t, err)
	var steps []string
	for range 8 {
		job, err := wtc.DB.ClaimJob(testWorkerID)
		require.NoError(t, err)
		if job == nil {
			break
		}
		if retry {
			wtc.Pool.failOrRetryAgentContext(context.Background(), testWorkerID, job, job.Agent, job.Agent+" broke")
		} else {
			wtc.Pool.failoverOrFailNonRetryableAgentLocked(testWorkerID, job, job.Agent, job.Agent+" broke")
		}
		after, err := wtc.DB.GetJobByID(job.ID)
		require.NoError(t, err)
		steps = append(steps, fmt.Sprintf("%s/%s/model=%s/retries=%d/error=%q", after.Status, after.Agent, after.Model, after.RetryCount, after.Error))
		if after.Status != storage.JobStatusQueued {
			break
		}
	}
	return strings.Join(steps, " -> ")
}

const defaultPathGolden = `== default config / PATH=
review workflow: preferred="codex" backup=""
review strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix workflow: preferred="codex" backup=""
fix strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== default config / PATH=codex
review workflow: preferred="codex" backup=""
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup=""
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== default config / PATH=claude
review workflow: preferred="codex" backup=""
review strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: claude-code
review registry: claude-code
fix workflow: preferred="codex" backup=""
fix strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model= backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== default config / PATH=codex,claude
review workflow: preferred="codex" backup=""
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup=""
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== default config / PATH=gemini
review workflow: preferred="codex" backup=""
review strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: gemini
review registry: gemini
fix workflow: preferred="codex" backup=""
fix strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: gemini
fix registry: gemini
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=gemini model= backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== default config / PATH=codex,claude,gemini
review workflow: preferred="codex" backup=""
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup=""
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== named agent / PATH=
review workflow: preferred="claude" backup=""
review strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix workflow: preferred="claude" backup=""
fix strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== named agent / PATH=codex
review workflow: preferred="claude" backup=""
review strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: codex
review registry: codex
fix workflow: preferred="claude" backup=""
fix strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== named agent / PATH=claude
review workflow: preferred="claude" backup=""
review strict: claude-code model=
review available: claude-code
review registry: claude-code
fix workflow: preferred="claude" backup=""
fix strict: claude-code model=
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model= backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="" backup="" backup_model="" backup_auto=false
== named agent / PATH=codex,claude
review workflow: preferred="claude" backup=""
review strict: claude-code model=
review available: claude-code
review registry: claude-code
fix workflow: preferred="claude" backup=""
fix strict: claude-code model=
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="" backup="" backup_model="" backup_auto=false
== named agent / PATH=gemini
review workflow: preferred="claude" backup=""
review strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: gemini
review registry: gemini
fix workflow: preferred="claude" backup=""
fix strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: gemini
fix registry: gemini
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=gemini model= backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== named agent / PATH=codex,claude,gemini
review workflow: preferred="claude" backup=""
review strict: claude-code model=
review available: claude-code
review registry: claude-code
fix workflow: preferred="claude" backup=""
fix strict: claude-code model=
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="" backup="" backup_model="" backup_auto=false
== named agent with model / PATH=
review workflow: preferred="claude" backup=""
review strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix workflow: preferred="claude" backup=""
fix strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== named agent with model / PATH=codex
review workflow: preferred="claude" backup=""
review strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: codex
review registry: codex
fix workflow: preferred="claude" backup=""
fix strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== named agent with model / PATH=claude
review workflow: preferred="claude" backup=""
review strict: claude-code model=sonnet
review available: claude-code
review registry: claude-code
fix workflow: preferred="claude" backup=""
fix strict: claude-code model=sonnet
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model=sonnet backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": agent=claude-code model=sonnet backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="sonnet" backup="" backup_model="" backup_auto=false
== named agent with model / PATH=codex,claude
review workflow: preferred="claude" backup=""
review strict: claude-code model=sonnet
review available: claude-code
review registry: claude-code
fix workflow: preferred="claude" backup=""
fix strict: claude-code model=sonnet
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model=sonnet backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model=sonnet backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="sonnet" backup="" backup_model="" backup_auto=false
== named agent with model / PATH=gemini
review workflow: preferred="claude" backup=""
review strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: gemini
review registry: gemini
fix workflow: preferred="claude" backup=""
fix strict: error: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: gemini
fix registry: gemini
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=gemini model= backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== named agent with model / PATH=codex,claude,gemini
review workflow: preferred="claude" backup=""
review strict: claude-code model=sonnet
review available: claude-code
review registry: claude-code
fix workflow: preferred="claude" backup=""
fix strict: claude-code model=sonnet
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model=sonnet backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model=sonnet backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="sonnet" backup="" backup_model="" backup_auto=false
== backup agent / PATH=
review workflow: preferred="codex" backup="claude"
review strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix workflow: preferred="codex" backup="claude"
fix strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== backup agent / PATH=codex
review workflow: preferred="codex" backup="claude"
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="claude"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup=claude backup_model=
ci member "codex": agent=codex model= backup=claude backup_model=
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== backup agent / PATH=claude
review workflow: preferred="codex" backup="claude"
review strict: claude-code model=
review available: claude-code
review registry: claude-code
fix workflow: preferred="codex" backup="claude"
fix strict: claude-code model=
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model= backup= backup_model=
ci member "codex": agent=claude-code model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="" backup="" backup_model="" backup_auto=false
== backup agent / PATH=codex,claude
review workflow: preferred="codex" backup="claude"
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="claude"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup=claude backup_model=
ci member "codex": agent=codex model= backup=claude backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== backup agent / PATH=gemini
review workflow: preferred="codex" backup="claude"
review strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: gemini
review registry: gemini
fix workflow: preferred="codex" backup="claude"
fix strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: gemini
fix registry: gemini
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== backup agent / PATH=codex,claude,gemini
review workflow: preferred="codex" backup="claude"
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="claude"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup=claude backup_model=
ci member "codex": agent=codex model= backup=claude backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== backup agent with model / PATH=
review workflow: preferred="codex" backup="claude"
review strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix workflow: preferred="codex" backup="claude"
fix strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== backup agent with model / PATH=codex
review workflow: preferred="codex" backup="claude"
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="claude"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup=claude backup_model=sonnet
ci member "codex": agent=codex model= backup=claude backup_model=sonnet
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== backup agent with model / PATH=claude
review workflow: preferred="codex" backup="claude"
review strict: claude-code model=sonnet
review available: claude-code
review registry: claude-code
fix workflow: preferred="codex" backup="claude"
fix strict: claude-code model=sonnet
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model=sonnet backup= backup_model=
ci member "codex": agent=claude-code model=sonnet backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": queued/claude-code/model=sonnet/retries=0/error="" -> failed/claude-code/model=sonnet/retries=0/error="claude-code broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=sonnet/retries=0/error="" -> queued/claude-code/model=sonnet/retries=1/error="" -> queued/claude-code/model=sonnet/retries=2/error="" -> queued/claude-code/model=sonnet/retries=3/error="" -> failed/claude-code/model=sonnet/retries=3/error="claude-code broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="claude-code" model="sonnet" backup="" backup_model="" backup_auto=false
== backup agent with model / PATH=codex,claude
review workflow: preferred="codex" backup="claude"
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="claude"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup=claude backup_model=sonnet
ci member "codex": agent=codex model= backup=claude backup_model=sonnet
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": queued/claude-code/model=sonnet/retries=0/error="" -> failed/claude-code/model=sonnet/retries=0/error="claude-code broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=sonnet/retries=0/error="" -> queued/claude-code/model=sonnet/retries=1/error="" -> queued/claude-code/model=sonnet/retries=2/error="" -> queued/claude-code/model=sonnet/retries=3/error="" -> failed/claude-code/model=sonnet/retries=3/error="claude-code broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== backup agent with model / PATH=gemini
review workflow: preferred="codex" backup="claude"
review strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: gemini
review registry: gemini
fix workflow: preferred="codex" backup="claude"
fix strict: error: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: gemini
fix registry: gemini
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: claude-code) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== backup agent with model / PATH=codex,claude,gemini
review workflow: preferred="codex" backup="claude"
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="claude"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup=claude backup_model=sonnet
ci member "codex": agent=codex model= backup=claude backup_model=sonnet
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": queued/claude-code/model=sonnet/retries=0/error="" -> failed/claude-code/model=sonnet/retries=0/error="claude-code broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=sonnet/retries=0/error="" -> queued/claude-code/model=sonnet/retries=1/error="" -> queued/claude-code/model=sonnet/retries=2/error="" -> queued/claude-code/model=sonnet/retries=3/error="" -> failed/claude-code/model=sonnet/retries=3/error="claude-code broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== review workflow agent / PATH=
review workflow: preferred="gemini" backup=""
review strict: error: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix workflow: preferred="codex" backup=""
fix strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== review workflow agent / PATH=codex
review workflow: preferred="gemini" backup=""
review strict: error: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: codex
review registry: codex
fix workflow: preferred="codex" backup=""
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": agent=codex model=pro backup= backup_model=
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== review workflow agent / PATH=claude
review workflow: preferred="gemini" backup=""
review strict: error: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: claude-code
review registry: claude-code
fix workflow: preferred="codex" backup=""
fix strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": agent=claude-code model=pro backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== review workflow agent / PATH=codex,claude
review workflow: preferred="gemini" backup=""
review strict: error: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: codex
review registry: codex
fix workflow: preferred="codex" backup=""
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": agent=codex model=pro backup= backup_model=
ci member "claude": agent=claude-code model=pro backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "gemini", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== review workflow agent / PATH=gemini
review workflow: preferred="gemini" backup=""
review strict: gemini model=pro
review available: gemini
review registry: gemini
fix workflow: preferred="codex" backup=""
fix strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: gemini
fix registry: gemini
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=gemini model=pro backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="gemini" model="pro" backup="" backup_model="" backup_auto=false
== review workflow agent / PATH=codex,claude,gemini
review workflow: preferred="gemini" backup=""
review strict: gemini model=pro
review available: gemini
review registry: gemini
fix workflow: preferred="codex" backup=""
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=gemini model=pro backup= backup_model=
ci member "codex": agent=codex model=pro backup= backup_model=
ci member "claude": agent=claude-code model=pro backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="gemini" model="pro" backup="" backup_model="" backup_auto=false
== fix pin / PATH=
review workflow: preferred="codex" backup=""
review strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix workflow: preferred="codex" backup="gemini"
fix strict: error: no configured agent available (preferred: "codex", backups: gemini) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix registry: error: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: no agents available (install one of: codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, grok) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== fix pin / PATH=codex
review workflow: preferred="codex" backup=""
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="gemini"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== fix pin / PATH=claude
review workflow: preferred="codex" backup=""
review strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: claude-code
review registry: claude-code
fix workflow: preferred="codex" backup="gemini"
fix strict: error: no configured agent available (preferred: "codex", backups: gemini) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
fix available: claude-code
fix registry: claude-code
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=claude-code model= backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== fix pin / PATH=codex,claude
review workflow: preferred="codex" backup=""
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="gemini"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== fix pin / PATH=gemini
review workflow: preferred="codex" backup=""
review strict: error: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
review available: gemini
review registry: gemini
fix workflow: preferred="codex" backup="gemini"
fix strict: gemini model=
fix available: gemini
fix registry: gemini
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=gemini model= backup= backup_model=
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=503 error=no review agent available: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
== fix pin / PATH=codex,claude,gemini
review workflow: preferred="codex" backup=""
review strict: codex model=
review available: codex
review registry: codex
fix workflow: preferred="codex" backup="gemini"
fix strict: codex model=
fix available: codex
fix registry: codex
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": agent=codex model= backup= backup_model=
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: agent="codex" model="" backup="" backup_model="" backup_auto=false
== unknown agent / PATH=
review workflow: preferred="bogus" backup=""
review strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix workflow: preferred="bogus" backup=""
fix strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=400 error=invalid agent: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
== unknown agent / PATH=codex
review workflow: preferred="bogus" backup=""
review strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix workflow: preferred="bogus" backup=""
fix strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=400 error=invalid agent: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
== unknown agent / PATH=claude
review workflow: preferred="bogus" backup=""
review strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix workflow: preferred="bogus" backup=""
fix strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=400 error=invalid agent: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
== unknown agent / PATH=codex,claude
review workflow: preferred="bogus" backup=""
review strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix workflow: preferred="bogus" backup=""
fix strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=400 error=invalid agent: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
== unknown agent / PATH=gemini
review workflow: preferred="bogus" backup=""
review strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix workflow: preferred="bogus" backup=""
fix strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
ci member "codex": error: no review agent available for type=default: no configured agent available (preferred: "codex", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
ci member "claude": error: no review agent available for type=default: no configured agent available (preferred: "claude-code", backups: ) | You may need to run 'roborev daemon restart' from a shell that has access to your agents
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=400 error=invalid agent: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
== unknown agent / PATH=codex,claude,gemini
review workflow: preferred="bogus" backup=""
review strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
review registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix workflow: preferred="bogus" backup=""
fix strict: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix available: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
fix registry: error: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
names: acp,claude-code,codex,copilot,cursor,droid,gemini,grok,kilo,kiro,opencode,pi,test
ci member "": error: no review agent available for type=default: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
ci member "codex": agent=codex model= backup= backup_model=
ci member "claude": agent=claude-code model= backup= backup_model=
failover codex stored backup "": failed/codex/model=/retries=0/error="codex broke"
retry codex stored backup "": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> failed/codex/model=/retries=3/error="codex broke"
failover codex stored backup "claude": queued/claude-code/model=/retries=0/error="" -> failed/claude-code/model=/retries=0/error="claude-code broke"
retry codex stored backup "claude": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/claude-code/model=/retries=0/error="" -> queued/claude-code/model=/retries=1/error="" -> queued/claude-code/model=/retries=2/error="" -> queued/claude-code/model=/retries=3/error="" -> failed/claude-code/model=/retries=3/error="claude-code broke"
failover codex stored backup "gemini": queued/gemini/model=/retries=0/error="" -> failed/gemini/model=/retries=0/error="gemini broke"
retry codex stored backup "gemini": queued/codex/model=/retries=1/error="" -> queued/codex/model=/retries=2/error="" -> queued/codex/model=/retries=3/error="" -> queued/gemini/model=/retries=0/error="" -> queued/gemini/model=/retries=1/error="" -> queued/gemini/model=/retries=2/error="" -> queued/gemini/model=/retries=3/error="" -> failed/gemini/model=/retries=3/error="gemini broke"
enqueue: status=400 error=invalid agent: unknown agent "bogus" (known: acp, claude-code, codex, copilot, cursor, droid, gemini, grok, kilo, kiro, opencode, pi, test)
`
