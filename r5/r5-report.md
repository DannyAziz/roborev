# roborev host backend — r5

r5 is three commits on `00316ee7`, on local branch `host-r5` in `/tmp/roborev`. Nothing was pushed (see "No-push evidence").

| # | Commit | Subject | Prod lines | Test lines |
|---|--------|---------|-----------:|-----------:|
| 1 | `c3c7c0b5` | Add host review backend gated on ROBOREV_HOST_AGENT=1 | 399 | 1922 |
| 2 | `4c17b8cf` | Detect Cursor cloud hosts; host is an automatic pick only with the marker | 265 | 770 |
| 3 | `8964d812` | Add the auto agent keyword; host is its last resort only with the marker | 327 | 914 |
| | combined `00316ee7..8964d812` (tree `2ab06ccb`) | | 955 | 3588 |

Line counts are added plus deleted lines from `git diff --numstat <c>~1 <c>`. Files ending in `_test.go` count as test lines; everything else, including `docs/`, counts as prod. Each commit is under 400 prod lines; commit 1 is at 399.

Unless a line says otherwise, `file:line` references are to the final tree `8964d812`.

## Conflicts with opt-in requirement

The requirement (Ethan, 2026-10-19 07:18 ET) has three parts:
- With no new config and `ROBOREV_HOST_AGENT` unset, behavior is byte-for-byte that of `00316ee7`.
- There is no new required env, secret or dependency.
- There is no cursor-cloud default.

Most of the diff meets it; the evidence is in "Default path unchanged". The items below do not meet it literally, or only meet it by argument. Each item names its opt-in, if any, and what would remove it.

1. **Cursor cloud agent detection (commit 2) is Cursor-specific code in the shared repo.** `ReadHostMarker` (`internal/agent/host.go:315`) treats `CURSOR_AGENT=1` plus any `CLOUD_AGENT_*` variable as a "detected" marker.
   - **What detection does.** It only lets a host that is named run: as `default_agent`, a backup or `--agent host`. It never selects host, lists it, or makes it an automatic pick (`Allows`, `host.go:349`).
   - **Why this is still opt-in.** Naming host needs a config that `00316ee7` rejects (`unknown agent "host"`), or an explicit `--agent host`. So detection changes nothing for a user who has not opted in. `TestDefaultPathUnchanged` and the CLI golden run with every `CURSOR_AGENT*`/`CLOUD_AGENT_*` variable removed. `TestHostRulingByMarkerMode` shows detection with an unset or non-host config fails exactly like no marker.
   - **Why it is flagged.** It is a cloud-environment behavior that ships enabled for anyone who names host. If the requirement is read as "no cloud-specific behavior until cursor-cloud lands", it should be removed.
   - **How to remove it.** Make `Allows` return `m.Explicit` for every use (one line, `host.go:350-352`). Then flip the "detected only" rows in `TestHostMarkerAllows` and `TestHostRulingByMarkerMode`. `ReadHostMarker`'s detection code would then be dead and could be dropped.
   - Nothing in the repo defaults to host or to cursor-cloud. The built-in default agent is still `codex`, from `config/workflow.go`, which is unchanged.
2. **The `backup_auto` schema column is added for every user.**
   - `internal/storage/db.go:1132-1139` runs `ALTER TABLE review_jobs ADD COLUMN backup_auto INTEGER NOT NULL DEFAULT 0` on daemon start, whatever the config.
   - The column is additive, defaults to 0, and is `json:"-"`, so API output is unchanged (CLI golden).
   - The `00316ee7` binary still opens the migrated DB. I checked this on one data dir with the `8964d812` binary: head enqueued a job (the column exists), base listed that job and enqueued one more, and head then listed both. Both binaries log the same "Database migration: …" lines on every start.
   - It is still a schema change on the default path. The alternative is to drop the column and lose the P2-1/F02 provenance.
3. **Host is in the agent registry for everyone.**
   - `agent/spec.go:176-178` and `agentname/agentname.go:38` register `host`. Every listing filters it out without `ROBOREV_HOST_AGENT=1` (`ListedAgentNames`, `host.go:425`). Every automatic pick skips it (`name != hostAgentName`).
   - Output is unchanged: the CLI golden includes completion, check-agents and the unknown-agent lists.
   - Internally, `agent.Available()` now includes `host`. The one visible effect in the upstream test run is an extra subtest, `TestAgentSpecsCommandOverrides/host`.
4. **Configs that `00316ee7` rejected now load or fail with different text.**
   - `auto` and `host` in agent keys now load. `00316ee7` rejects them at config load as unknown agents, so no config that works on base changes meaning.
   - New rejections, with new text: `classify_agent` or `classify_backup_agent` set to `host` or `auto`; `auto` in nested agent keys; `auto` or `host` in `budget.agent_costs` (`config/config.go:539-573`, `config/budget.go:46-51`). Base rejected each of these too, but as `unknown agent`. A user who hits them on base gets a different error message on head.
   - `--agent auto`, `--agent host` and `check-agents --agent host` are explicit opt-ins on the command line. Base answered each with an unknown-agent error.
5. **One extra config read on the final-failure path.**
   - When a job fails for good with no stored backup, `skippedHostBackupNote` (`worker.go:1970`) re-reads workflow config to check whether the backup is `host` or `auto`.
   - On the default path the note is empty, so the error text is unchanged (golden failover and retry rows). The read itself is new work on Danny's path.

No other conflict was found. "Default path unchanged" lists every other prod hunk and why it cannot change output without opt-in.

## The ruling

The 11:04 correction and the opt-in requirement are kept in one helper, `internal/agent/host.go:349`:

```go
func (m HostMarker) Allows(use HostUse) bool {
	if use == HostNamed {
		return m.Explicit || m.Detected
	}
	return m.Explicit
}
```

- **Marker values.** `Explicit` means `ROBOREV_HOST_AGENT` is `1`, `true`, `yes` or `on`. Any other non-empty value, `0` included, turns off both kinds (`ReadHostMarker`, `host.go:315`).
- **Uses.** `HostNamed` is a primary or a named backup. `HostListed` is a listing, completion or health check. `HostAuto` is `auto`, auto-selection or a last resort (`host.go:336-342`).
- **Commit 1.** `Allows` is `return m.Explicit`. Detection arrives only in commit 2.
- **`ROBOREV_HOST_AGENT=1`.** Host is an automatic fallback, as in r4. This covers the `auto` primary, a CI blank agent, the non-strict last resort, and runtime failover through an `auto` backup or a CI or fix `auto` pin.
- **Strict enqueue with an `auto` backup never takes host, in any mode.** `strictAutoBackupOr` (`acp_resolution.go:264`) passes `lastResort=false` at `:267`.
- **Detection only.** Host runs only when named. It is never listed and never an automatic pick.
- **No marker, or `=0`.** Naming host fails fast with a `HostUnavailableError` that names `ROBOREV_HOST_AGENT`. Failover skips host and tries the next agent. When none is left, the error names `ROBOREV_HOST_AGENT`.
- **Docs.** `docs/configuration.md:1098-1099` (rows) and `:1156` ("Host as an automatic pick").

**r4 report line 232 was corrected.** It now says a strict named primary with an `auto` backup never gets host at enqueue. At runtime with `=1` it can: through failover via `auto`, or a CI or fix `auto` pin.

`TestHostRulingByMarkerMode` (`internal/daemon/auto_backup_test.go:258`) runs `/api/enqueue`, the CI resolver, `ResolveAutoBackup` and rerun with no agent CLI on PATH, and asserts that no handoff dir is created. Each cell is the agent of the enqueued job. "fail" means HTTP ≥ 400 and nothing enqueued.

| Row | neither | `=0` (with detection vars set) | detected only | `=1` |
|-----|---------|------|---------------|------|
| explicit host primary | fail | fail | host | host |
| explicit host backup (`codex` + `host`) | fail | fail | host | host |
| unset agent | fail | fail | fail | fail |
| unset agent with `auto` backup | fail | fail | fail | fail |
| named primary with `auto` backup (`gemini` + `auto`) | fail | fail | fail | fail |
| `auto` primary | fail | fail | fail | host |
| strict named primary unavailable (`codex`) | fail | fail | fail | fail |
| CI blank agent | error | error | error | host |
| `ResolveAutoBackup(exclude=codex)` | "" | "" | "" | host |

`TestHostFailoverByMarkerMode` (`auto_backup_test.go:354`) is the runtime-failover table the 11:04 correction asked for. It calls `failoverOrFailNonRetryableAgentLocked` on a failing `codex` job, in the same 4 modes:

| Row | neither / `=0` | detected only | `=1` |
|-----|----------------|---------------|------|
| workflow `auto` backup, nothing installed | failed, error names `ROBOREV_HOST_AGENT` | same | requeued as host |
| stored `auto` backup, nothing installed | failed, names `ROBOREV_HOST_AGENT` | same | requeued as host |
| workflow `auto` backup, `claude` on PATH | requeued as `claude-code` (host skipped) | same | same |
| named host backup (stored) | failed, names `ROBOREV_HOST_AGENT` | requeued as host | requeued as host |
| named host backup (workflow) | failed, names `ROBOREV_HOST_AGENT` | requeued as host | requeued as host |
| empty `auto` pin | failed, "found no available agent when the job was queued" | same | same |

Every row asserts that no handoff dir exists: the job is only requeued, never run.

## Verification per commit

Each commit was checked out in its own clean worktree. I ran `go test -count=1 ./...` once per mode: 50 packages, 43 with tests and 7 without. Every command ran with `ROBOREV_TELEMETRY_ENABLED=0`. The modes:
- **neither:** `ROBOREV_HOST_AGENT` unset, and every `CURSOR_AGENT*` and `CLOUD_AGENT_*` variable removed with `env -u`.
- **detected:** `ROBOREV_HOST_AGENT` unset, `CURSOR_AGENT=1`, `CLOUD_AGENT_SUITE=1`.
- **`=1`:** `ROBOREV_HOST_AGENT=1`.
- **`=0`:** `ROBOREV_HOST_AGENT=0`.

| Commit | `go vet ./...` | `gofmt -l .` | neither | detected | `=1` | `=0` |
|--------|----------------|--------------|---------|----------|------|------|
| `c3c7c0b5` | clean | empty | 43 ok | 43 ok | 43 ok | 43 ok |
| `4c17b8cf` | clean | empty | 43 ok | 43 ok | 43 ok | 43 ok |
| `8964d812` | clean | empty | 43 ok | 43 ok | 43 ok | 43 ok |

The logs are `/tmp/r5-final-<c1|c2|c3>-<mode>.log`, plus `/tmp/r5-final-<c>-static.txt` for vet and gofmt.

**Environment notes.**
- **Signing timeout.** This VM's global git config signs commits through a helper (`commit.gpgsign=true`, `gpg.format=ssh`). The helper sometimes stalls for 30 seconds. One `=1` run of the previous commit 2 (`ea8714ea`) failed in `cmd/roborev`, in `TestAutoInstallHooks`: the setup step's `git commit` printed "Couldn't find key in agent?" and exited 128. That is a signing failure, not a failure in roborev code.
  - From then on, every harness run (the c2 and c3 suites above, the base-test run, the mutation sweep and the flake runs) used `GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig`. That file has a test user name and email and signing off.
  - The c1 row above ran before the switch, with the VM's own git config, and passed.
- **dbus orphans.** Daemon tests leave `dbus-daemon` processes behind, which use up inotify instances. I ran a reaper during the runs (`/tmp/dbus-reaper.sh`). I did not check whether the base shows the same thing.
- **Earlier flake.** An earlier iteration of commit 1 failed once in `TestHandleJobOutput` (`internal/daemon`) under `=1`, and its rerun passed. I did not find a root cause. The final `c3c7c0b5` passed all 4 modes.

**Commit-1 bisect hazard.** Commit 1 builds and passes on its own in all 4 modes (table above). It already contains the guards that make host safe without the later commits:
- `check-agents` never smoke-tests host (`TestCheckAgentsNeverSmokeTestsHost`);
- budget config rejects host (`TestBudgetValidation`);
- budget routing never picks host (`TestBudgetRoutingNeverSelectsHost`).

The BISECT mutants below are red for all three.

## Default path unchanged

**What "default path" means here.** `ROBOREV_HOST_AGENT` is unset, there are no `CURSOR_AGENT*`/`CLOUD_AGENT_*` variables, and no config names `host` or `auto`. Base rejects such configs at load.

### Evidence

1. **Upstream tests, base versus head.** I ran the `00316ee7` test files against head prod code (`/tmp/r5-basetests`), with the marker unset and the Cursor variables removed, using `go test -count=1 -v ./...`.
   - The tree is `f0192807` prod with base `_test.go` files. Its prod files are identical to `8964d812`: `git diff f0192807 8964d812` touches only `internal/daemon/host_agent_test.go`.

   | Tree | PASS | FAIL | SKIP |
   |------|-----:|-----:|-----:|
   | `00316ee7` | 11650 | 0 | 32 |
   | head prod + base tests | 11651 | 0 | 32 |

   The sorted outcome lists differ in only three places:
   - Two subtests are named after a pointer (`TestACPTerminalOnlyLimitsOutputWhenRequested/0x…`). Their names change on every run.
   - Two subtests are named after a fresh git SHA (`TestPreparePreservesCompletePrompt/<sha>`). These also change on every run.
   - One extra subtest, `TestAgentSpecsCommandOverrides/host`, comes from conflict 3.

   Files: `/tmp/r5-bt-base.outcomes`, `/tmp/r5-bt-head2.outcomes`.
2. **New table test, run on base and head.** `TestDefaultPathUnchanged` (`internal/daemon/default_path_test.go:57`, commit 1) crosses 8 configs with 6 PATH sets.
   - The configs: default; named agent; named agent with model; backup agent; backup agent with model; review workflow agent with model; fix pin (`fix_agent` plus `fix_backup_agent`); unknown agent.
   - The PATH sets are combinations of failing `codex`, `claude` and `gemini` stubs.
   - For each pair it records, for the review and fix workflows: the workflow resolution, the strict, non-strict and registry lookups (agent, model or error text), and the listed names.
   - It also records the CI matrix pin for members `""`, `codex` and `claude`, with agent, model, backup and backup model.
   - Last, it records failover and retry outcomes for a failing `codex` job with stored backup `""`, `claude` and `gemini`: final status, agent and error text.
   - Its 912-line transcript was generated on `00316ee7`, by copying the test file into a clean base worktree, and is embedded as the golden. I regenerated it at the end on both trees: base and `8964d812` are byte-identical (md5 `3ce4dd36530e03df293036a7d349a491`). The test passes on head in all 4 suite modes, since it clears the variables itself.
3. **CLI golden.** `/tmp/golden_cli.sh` drives a real daemon and CLI with the marker unset, Cursor and `ROBOREV_*` variables removed, and stub agents on PATH.
   - The configs: default (no agents, and failing `codex`+`claude`); named `claude`; backup with model; backup with nothing installed; fix pin; unknown agent; broken TOML.
   - The commands: `doctor`, `check-agents`, `check-agents --agent bogus`, `--agent` completion, `/api/enqueue`, `review --agent bogus`.
   - I built each commit and normalized version strings, git SHAs, machine IDs, pids, ids and timestamps. The 585-line output of `c3c7c0b5`, `4c17b8cf` and `8964d812` is each identical to `00316ee7` (`/tmp/r5-cli-base-final.norm.txt`, `/tmp/r5-cli-final-r5-c{1,2,3}.norm.txt`).
4. **Help text.** `--help` for the root command and 48 subcommands is identical between the `00316ee7` and `8964d812` binaries (`/tmp/r5-help-roborev-base.txt`, `/tmp/r5-help-final.txt`).
5. **Mutants that pin the default path.** Each one below is red in the sweep:
   - listings leak host;
   - completion fallback leaks host;
   - a skipped-host note on every failure;
   - non-host get-agent errors not retried;
   - doctor mismatch wording;
   - doctor review summary format;
   - CI pins a non-auto backup;
   - the auto model rule applied to every agent.

### Hunk audit

Every prod hunk falls into one of three groups. I read `git diff 00316ee7 8964d812 -- . ':!*_test.go'` file by file.

**Reachable only with opt-in.** These need `ROBOREV_HOST_AGENT=1`, host named in config or on the command line, or `auto` in config or on the command line. Base rejects each of those configs or flags.
- `host.go` (new). `auto.go` (new).
- `IsAvailable`/`isAvailableWithConfig` host branch (`agent.go:261`, `acp_resolution.go:164`).
- `requestedHostError` calls (return nil unless the name is host).
- `splitAuto`/`withoutAuto`/`strictAutoBackupOr`. Without `auto`, the strict error is `withSkippedHostNote(unavailablePreferredBackupError(...))`, and the note is empty unless a backup is host.
- `hostLastResortOr`. Without the marker it returns the base error unchanged, unless a backup is host.
- `ModelForSelectedAgent`/`ResolveWorkflowModelForAgentFromConfig` `auto` branches (F03).
- `schema_agent.go` `auto` cases. `budget.go`/`config.go` host and `auto` validation.
- `resolveRerunOpts` `auto` pin. `pinnedAutoBackup` (returns `""` unless the backup is `auto`, and base enqueued fix and goal jobs with an empty backup). `backupIsAuto` (false, stored as 0).
- `resolveBackupAgent`/`resolveBackupModel` `auto` and host branches.
- `check_agents.go` host branches (`--agent host`). `doctor_agents.go` host branches (`doctorIsHost`, `doctorHostKeys`, `doctorAgentFix`, the configured-agents fix suffix).
- `doctor_checks.go:102`, `HostAllowed(HostListed)`, which is explicit `=1` only.
- `diagnose.go` host `Command`. `synthesis_worker.go:447-451` host-only branch.

**Same output on the default path (refactors).** Each item lists what pins it.
- `completions.go` and `AvailableNamesFromConfig`/`GetAvailable` unknown-agent lists: `ListedAgentNames()` equals the base `Available()` without host. Pinned by the golden (completion, `--agent bogus`), `TestDefaultPathUnchanged` names, and `TestAgentFlagCompletionListsHostOnlyWithMarker`.
- Fallback loops skip host: host was never in base's list.
- `worker.go` `failGetAgentContext` (and the synthesis get-agent call, `synthesis_worker.go:476`): for non-host errors it is the same `failOrRetryAgentContext(..., "get agent: %v")` call as base. Pinned by `TestWorkerNonHostGetAgentErrorIsStillRetried` (added in r5 after the sweep found this mutant green) and `TestSynthesisNonHostErrorIsStillRetried`.
- `failWithoutRetryLocked`: an extracted tail of `failoverOrFailNonRetryableAgentLocked`.
- `skippedHostBackupNote` appended to final errors: empty unless the backup is host or `auto`. Pinned by the golden failover and retry rows and the "note on every failure" mutant. It does one extra config read (conflict 5).
- `doctor_agents.go` path mismatch: without host every missing entry is a PATH entry, so `what = "installed"`, the details and the fix equal base. Pinned by `TestDoctorAgentsDaemonPathMismatch` (exact Summary and Fix) and the golden.
- Doctor review summary: the host-only `Command` form. Pinned by `TestDoctorReviewSummaryKeepsPathForm`.
- `budget.go` `canonical` hoist: same result.
- `ci_poller.go`/`panel_enqueue.go`/`server.go` `backupExecutionForSelectedAgent(reviewType, ...)`: the new parameter is used only in the `auto` branch. Pinned by golden CI rows.

**Unconditional, listed as conflicts.** The `backup_auto` migration and the scan/insert column; host in the registry; config acceptance of `auto`/`host`.

### PR-body-ready text

> **Default behavior is unchanged.** Everything in this PR is opt-in. With `ROBOREV_HOST_AGENT` unset and no config naming `host` or `auto`, which `00316ee7` rejects as unknown agents, roborev behaves as it does on `personal-reviews` at `00316ee7`. Defaults, agent resolution, retries, failover and output are all the same. There are no new required env vars, secrets or dependencies, the built-in default agent is still `codex`, and nothing defaults to a cloud environment.
>
> Evidence:
> - The upstream test suite, run against this PR's code, has 11650 passing on base and 11651 on head, with 0 failures on either. The only difference is one added subtest for the new `host` registry entry.
> - The new `TestDefaultPathUnchanged` covers default, named-agent, backup-agent, workflow, fix-pin and unknown-agent configs, with several PATH sets. It records resolution, CI pins, retry and failover outcomes, and error text against a golden transcript generated on `00316ee7`. The transcript is byte-identical on head.
> - A CLI golden runs `doctor`, `check-agents`, completion, enqueue and the error paths on a real daemon, under 8 configs. Help text for every command is also identical.
>
> Two things do change for everyone:
> - an additive `backup_auto INTEGER NOT NULL DEFAULT 0` column on `review_jobs` (older binaries still open the DB);
> - a `host` entry in the internal agent registry, hidden from every listing unless `ROBOREV_HOST_AGENT=1`.
>
> To opt in, set `ROBOREV_HOST_AGENT=1`, or name `host` (or `auto`) in config or with `--agent`. A Cursor cloud agent shell lets a named `host` run without the variable, but never selects host by itself.

## Findings

"Red test" is the test that fails when the fix is reverted, shown by the mutant in the last column. The mutants come from `python3 /tmp/mutate_r5.py` on a detached worktree at `8964d812` (`/tmp/roborev-mut`, clean before and after), with `GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig`. The full log is `/tmp/r5-mutate-final.txt`.

The sweep removes `ROBOREV_HOST_AGENT` but keeps this VM's `CURSOR_AGENT` and `CLOUD_AGENT_*` variables. So the sweep process runs in detected mode, and tests that need another mode set it themselves (each table test sets all 4 modes per subtest). Commit 1 and 2 code was mutated in the final tree, where those lines still exist.

### P2

| Finding | Commit | Where | Red test(s) | Mutant → result |
|---------|--------|-------|-------------|-----------------|
| **P2-1:** a host auto-pinned under `=1` ran after a restart in detection-only mode | c3 | Provenance is stored as `backup_auto` (`storage/db.go:1132`, `jobs.go:314`, `job_columns.go:48`). It is set at every pin: `server.go:3182`, `:3696`, `goal_review.go:89`, `ci_poller.go:1709`, `panel_enqueue.go:700`. It is re-checked at failover: `worker.go:1908-1914` (an auto-pinned host needs `HostAllowed(HostAuto)`) | `TestAutoPinnedHostIsRecheckedOnRestart` (`daemon/auto_backup_test.go:436`) | Pinned host dropped even with `=1` → RED. Re-checked against the named rule (detection runs it) → RED. Not re-checked → RED. `backup_auto` not stored → RED (4 tests). Not read back → RED (4 tests). |
| **P2-2:** host showed up under detection in check-agents, completion, doctor and the unknown-agent list; doctor gave a false mismatch warning | c1 (`HostUse`), c2 (doctor) | `acp_resolution.go:165` and `agent.go:262` use `HostAllowed(HostNamed)` (named lookups only). Listings use `ListedAgentNames` (`host.go:425`, `HostListed` = explicit only). `doctor_checks.go:102`. Doctor asks the daemon about a named host: `doctor_agents.go:84-90`, `:133-138`. The `=0` pin in `TestDoctorAgentsDaemonPathMismatch` is gone; it now runs in all modes (`doctor_test.go:112`) | `TestHostHiddenFromSelectionAndListings`, `TestRerunAgentPickerListsHostOnlyWithMarker`, `TestCheckAgentsNeverSmokeTestsHost`, `TestDoctorAgentsDaemonPathMismatch`, `TestDoctorAgentsHostUnderDetectionOnly`, `TestDoctorAgentsOnlyNamedHostUnderDetection`, `TestHostAvailabilityIsMarkerGated`, `TestBackupHostWithoutMarkerIsSkipped` | Listing gate accepts detection → RED (6 tests). Doctor lists host under detection → RED. Availability gate removed → RED (3 tests). Doctor counts no named host / counts unnamed host → RED. |
| **P2-3 / F07:** synthesis changed retry behavior for non-host agents | c2 | `synthesis_worker.go:447-451`: only host takes the typed path (no same-agent retry on a handoff timeout). Every other run error keeps base's `failOrRetryAgentContext(... "agent: %v")`. The get-agent error (`:476`) goes through `failGetAgentContext`, which for non-host errors is base's call with base's text (`worker.go:1652`) | `TestSynthesisNonHostErrorIsStillRetried` (`host_agent_test.go:189`), `TestSynthesisHostTimeoutSkipsRetries` (`:163`), `TestWorkerNonHostGetAgentErrorIsStillRetried` (`:87`) | Non-host errors take the host path → RED. Host timeout retried → RED. All get-agent errors fail without retry → RED. |
| **P2-4 / F02:** freeze the empty pin at enqueue | c3 | `worker.go:1908` (an `auto` pin is final, even an empty one), `:1971` (note) | `TestEmptyAutoPinIsFrozen` (`auto_backup_test.go:414`), `TestExperimentReviewRecordsAutoPin` (`:461`), `TestPanelMembersRecordAutoPin` (`:498`), `TestFixJobPinsAutoBackup` (`:156`), `TestGoalReviewPinsAutoBackup` (`:177`) | Empty pin re-resolved at failover → RED. Note dropped → RED. Each enqueue path (fix, goal, review/experiment, panel, CI) not recording `auto` → RED. |
| **P2-5 / F06:** the `auto` backup must respect review-type eligibility | c3 | `autoBackupEligible` (`worker.go:2015`; goal reviews need `goalreview.ValidateAgent`, custom types need structured review), passed from worker failover (`:2010`) and CI/panel/enqueue pins (`ci_poller.go:1306-1311`) | `TestWorkerAutoBackupHonorsReviewType` (`auto_backup_test.go:527`), `TestGoalReviewPinsAutoBackup` | CI pin ignores eligibility → RED. Worker ignores eligibility → RED. Goal check removed → RED. Structured check removed → RED. |

### Ruling and the 11:04 correction

| Item | Where | Red test(s) | Mutant → result |
|------|-------|-------------|-----------------|
| `=1` makes host an automatic fallback (non-strict last resort) | `host.go:410` `hostLastResortOr` | `TestHostIsLastResortOnlyWithExplicitMarker` (`host_test.go:810`), `TestModelSourceSelection`, `TestHostRulingByMarkerMode` | Never host, even with `=1` → RED |
| `=1` lets an `auto` backup reach host (enqueue pin and runtime failover) | `auto.go:45` | `TestResolveAutoBackup` (`model_source_test.go:170`), `TestHostFailoverByMarkerMode`, `TestHostRulingByMarkerMode` | `if false` → RED. Accepts detection (`HostNamed`) → RED. Order loop stops skipping host → RED. |
| Detection is never an automatic pick or a listing | `host.go:353` | `TestHostMarkerAllows`, `TestHostIsLastResortOnlyWithExplicitMarker`, `TestResolveAutoBackup`, `TestHostRulingByMarkerMode`, `TestHostFailoverByMarkerMode`, `TestAutoPinnedHostIsRecheckedOnRestart` | `m.Explicit \|\| m.Detected` → RED in all 6 |
| Detection lets a named host run | `host.go:351` | `TestHostMarkerAllows`, `TestExplicitHostAgent`, `TestBackupHostWithMarkerIsUsed`, `TestHostRulingByMarkerMode`, `TestHostFailoverByMarkerMode`, `TestAutoPinnedHostIsRecheckedOnRestart` | `m.Explicit` → RED in all 6 |
| Strict enqueue with an `auto` backup never takes host | `acp_resolution.go:267` (`lastResort=false`) | `TestModelSourceSelection`, `TestHostRulingByMarkerMode` | `true` → RED |
| No marker: failover skips host; with nothing left the error names `ROBOREV_HOST_AGENT`; no handoff dir | `auto.go:52` `AutoBackupNotFound`, `worker.go:1970` `skippedHostBackupNote`, `host.go:396` | `TestHostFailoverByMarkerMode`, `TestResolveAutoBackup`, `TestBackupHostWithoutMarkerNothingLeftNamesMarker` (`host_test.go:760`) | Error no longer names the marker (auto) → RED. Same for a named host backup → RED. Worker drops the auto note → RED. |
| Docs | `docs/configuration.md:1098-1099`, `:1156` | none (docs) | not mutated |

### P3

| Finding | Commit | Where | Red test / probe | Mutant → result |
|---------|--------|-------|------------------|-----------------|
| **F03:** `--agent auto` passed `review_model` through | c3 | `model_resolution.go:193-199`, `:259-261`, `autoSelectionModel` `:368`. `default_model` pairs only when auto lands on `default_agent`; a workflow model beside `auto` never pairs | `TestModelSourceSelection` (`model_source_test.go:31`) | `--agent auto` passes the review model → RED. Workflow auto passes the workflow model → RED. Auto pairs `default_model` with any agent → RED. |
| **F04** | — | not addressed | — | The r5 brief lists "F04." with no description, and I could not find its definition in anything available to me. No change was made for it. |
| **F08:** doctor wording | c2 | `doctor_agents.go:152-160`: "N agent(s) **available** here cannot be run by the daemon" for host, base's "installed" wording and fix for PATH; the host line names `ROBOREV_HOST_AGENT` and the Cursor cloud agent | `TestDoctorAgentsDaemonPathMismatch`, `TestDoctorAgentsHostWithMarker` (`doctor_test.go:793`), `TestDoctorAgentsNoHostMismatchWhenDaemonRunsHost` (`:742`) | Never reports host → RED. Always reports host (false warning) → RED. Default wording changed → RED. |
| Docs rows `configuration.md:1098-1099` | c3 (c1 for `host`) | as in the ruling | none | not mutated |
| `noHostMarker` means unset, not `=0` | c1 | `daemon/host_agent_test.go:27` removes `ROBOREV_HOST_AGENT`, `CURSOR_AGENT*` and `CLOUD_AGENT_*`; `=0` is a separate mode (`daemonHostModes`, `auto_backup_test.go:230`) | used by every daemon host test | n/a (test helper) |
| Wrapped `HostUnavailableError` | c1 | `IsHostUnavailable` uses `errors.As` (`host.go:368`) | `TestHostUnavailableSurvivesWrapping` (`host_test.go:111`) | Matched only unwrapped → RED |
| `=0` turns detection off; detection needs `CLOUD_AGENT_*` | c2 | `ReadHostMarker` (`host.go:315`) | `TestReadHostMarker` (`host_test.go:121`), `TestHostMarkerAllows` (`:166`) | `=0` no longer turns detection off → RED (4 tests). Detection without `CLOUD_AGENT_*` → RED. |
| Named host without a marker fails fast | c1 | `requestedHostError` (`host.go:374`) | `TestExplicitHostWithoutMarkerErrors`, `TestEnqueueExplicitHostWithoutMarkerFailsFast`, `TestWorkerExplicitHostWithoutMarkerFailsWithoutRetry` | Removed → RED in all 3 |
| Commit-1 bisect hazard | c1 | `check_agents.go` host branch, `config/budget.go:49`, `daemon/budget.go:71`, `:98` | `TestCheckAgentsNeverSmokeTestsHost`, `TestBudgetValidation`, `TestBudgetRoutingNeverSelectsHost`; commit 1's 4-mode suite | Each guard removed → RED |
| Reviewer mutants X01-X06, O04, O05, O10, O19 | — | — | — | I don't have the reviewer's mutant definitions, so I can't map those IDs. My own sweep covers the same guards (all 53 mutants above). Whether it matches the reviewer's set one-to-one is unknown. |

### Mutation totals

- **Counts.** There are 53 mutants, all applied, and all 53 are RED on `8964d812`. Groups: RULING 7, NO-MARKER 3, P2-1 5, F02 7, F06 4, F03 3, F07 2, BISECT 3, P2-2 5, F08 2, P3 4, DEFAULT 8.
- **Two mutants were weak on the first full run (on `f0192807`):**
  - "DEFAULT get-agent errors all fail without retry" stayed **green**. No test covered a non-host get-agent error. I added `TestWorkerNonHostGetAgentErrorIsStillRetried` to commit 2 (now `4c17b8cf`; commit 3 was rebased onto it as `8964d812`). It goes red on that mutant.
  - "DEFAULT completion fallback leaks host" was **not applied**, because its target string had changed. I retargeted it to `agent.ListedAgentNames(), cobra` → `agent.Available(), cobra`. It goes red in `TestAgentFlagCompletionListsHostOnlyWithMarker`.
- Both fixes were then confirmed in the full rerun on `8964d812` reported above.

## Probes (real binary built from `8964d812`)

`/tmp/probe5.sh` starts a daemon with its own data dir, `PATH=<stubs>:/usr/bin:/bin` (no real agent CLI) and `ROBOREV_HOST_TIMEOUT=3s`. Then it runs one action and checks for a host handoff dir. Outputs are in `/tmp/r5-probe-fin-*.txt`, and every probe daemon was stopped.

| Probe | Daemon / shell mode | Config | Result | Handoff dir |
|-------|---------------------|--------|--------|-------------|
| enqueue | neither | `auto` | HTTP 503, "no agents available …" (no host) | none |
| enqueue | detected | `auto` | HTTP 503, same | none |
| enqueue | `=1` | `auto` | HTTP 201, `agent: host` | none (not run) |
| enqueue | `=1` | `gemini` + backup `auto` | HTTP 503, `…(preferred: "gemini", backups: auto)`, nothing queued | none |
| failover, `codex` stub fails | neither | `codex` + `auto` | failed: `…backup "auto" found no available agent; host is an automatic pick only with ROBOREV_HOST_AGENT=1` | none |
| failover | detected | `codex` + `auto` | failed, same text | none |
| failover | `=1` | `codex` + `auto` | failed over to host (`running`) | created |
| failover | neither | `codex` + `host` | failed: `backup "host" was skipped: no host model is attached (set ROBOREV_HOST_AGENT=1 to use it)` | none |
| failover | detected | `codex` + `host` | failed over to host (named) | created |
| failover, `codex` and `claude` stubs fail | neither | `codex` + `auto` | host skipped, failed over to `claude-code`, which then failed after 3 retries | none |
| `check-agents` | neither | `codex` | 12 skipped, no host line, exit 0 | none |
| `check-agents` | detected | `host` | 12 skipped, no host line (not listed under detection), exit 0 | none |
| `check-agents --agent host` | neither | `codex` | `x host FAIL: … set ROBOREV_HOST_AGENT=1 …`, exit 1 | none |
| `check-agents --agent host` | detected | `codex` | `host not smoke-tested`, exit 0 | none |
| `doctor` | detected / detected | `host` | `agents available to the daemon: host`, `reviews use host (file handoff in …)`, no mismatch warning, exit 0 | none |
| `doctor` | detected / detected | `codex` | no host anywhere; "the daemon can run every agent this shell can" | none |
| `doctor` | neither / neither | `host` | FAIL "agent host needs ROBOREV_HOST_AGENT=1 or a Cursor cloud agent in the daemon"; no mismatch warning | none |
| `doctor` | `=0` / `=1` | `host` | warn "1 agent available here cannot be run by the daemon", `host: ROBOREV_HOST_AGENT=1 is set in this shell, but the daemon's environment has neither it nor a Cursor cloud agent…`, fix "run 'roborev daemon restart' from this shell so the daemon inherits ROBOREV_HOST_AGENT" | none |

The two "created" rows are expected: the ruling lets host run there, so the daemon writes a handoff and waits for the host model.

## Flake runs

Each test ran as `<pkg>.test -test.run '^Name$' -test.count=3000 -test.v`. The test binaries were built from `8964d812`, and the 4 modes ran in parallel on one 8-CPU VM (modes as in "Verification per commit", plus `GIT_CONFIG_GLOBAL=/tmp/r5-gitconfig`). A cell is pass/fail of top-level `--- PASS` / `--- FAIL` lines.

| Package | Test | neither | detected | `=1` | `=0` |
|---------|------|---------|----------|------|------|
| internal/agent | TestHostAgentRoundTrip | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostAgentPlainReviewPassesRepoPath | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostHandoffDirsAreUniqueAndContained | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostModelHint | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHandoffFilesArePublishedAtomically | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostAgentRefusesPlantedHandoffFile | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostAnswerReplacedBeforeOpenIsRefused | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostAnswerFIFOIsRefusedWithoutBlocking | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostAnswerErrorFailsBeforeDeadline | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostAnswerErrorAtDeadlineIsReported | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostAgentCancel | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostIsLastResortOnlyWithExplicitMarker | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestBackupHostWithMarkerIsUsed | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestResolveAutoBackup | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestReadHostMarker | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/agent | TestHostMarkerAllows | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/review | TestRunAgentReviewHostRoundTrip | 3000/0 | 3000/0 | 3000/0 | 3000/0 |
| internal/review | TestRunAgentReviewHostTimeoutIsNonRetryable | 3000/0 | 3000/0 | 3000/0 | 3000/0 |

**Done.** 18 tests × 4 modes × 3000 runs = 216,000 runs, 0 failures.

**Not finished.** None of the 8 `internal/daemon` flake tests finished in any mode when the run was handed off:
- `TestHostRulingByMarkerMode`
- `TestHostFailoverByMarkerMode`
- `TestEmptyAutoPinIsFrozen`
- `TestAutoPinnedHostIsRecheckedOnRestart`
- `TestWorkerExplicitHostWithoutMarkerFailsWithoutRetry`
- `TestSynthesisHostTimeoutSkipsRetries`
- `TestSynthesisNonHostErrorIsStillRetried`
- `TestWorkerNonHostGetAgentErrorIsStillRetried`

`TestHostRulingByMarkerMode` was in progress. Its partial counts at hand-off were neither: 838/0, detected: 823/0, 1: 829/0, 0: 826/0 (pass/fail). These daemon tests are being fanned out to other VMs with `.r5-harness/flake-daemon.sh` on branch `r5-flake-harness`; its README has the test names, modes and pass criterion. The tmux session `r5-flake` on this VM was left running, and its summaries go to `/tmp/r5-flake-<mode>.txt`.

## Known gaps

- **F04** is not addressed; see the P3 table.
- **The reviewer's mutant IDs** X01-X06, O04, O05, O10 and O19 are not mapped; I used my own sweep of 53 mutants.
- **Mutation mode.** The sweep runs in a detected-mode process, as described under "Findings". The table tests set each mode themselves, but mutants were not rerun with the process in each of the 4 modes.
- **Flake runs are partial.** The 8 `internal/daemon` tests have no completed 3000-run result yet; see "Flake runs".
- **Conflicts 1-5** at the top of this report are open decisions, not fixes.
- **`failGetAgentContext`** is a non-host code path that changed shape. Behavior and text are the same as base for non-host errors, which is pinned by `TestWorkerNonHostGetAgentErrorIsStillRetried` and `TestSynthesisNonHostErrorIsStillRetried`. The code is still not the literal base code.
- **Environment.** The `TestAutoInstallHooks` signing timeout, the `TestHandleJobOutput` failure on an earlier commit-1 iteration, and the dbus/inotify leak are all described under "Verification per commit". None was traced to this series. I did not check whether base shows the dbus leak.

## Artifacts

| File | md5 |
|------|-----|
| `r5-part1.patch` (`git format-patch -1 c3c7c0b5`) | `576d9feb503ab445e771cfbc45958e84` |
| `r5-part2.patch` (`4c17b8cf`) | `888fdda105b8a9c08b91a1e1b3648443` |
| `r5-part3.patch` (`8964d812`) | `2a3633c96f9613aae81182af262013de` |
| `r5-combined.patch` (`git diff 00316ee7 8964d812`) | `d363f363542e25236a80fb3a6846cd07` |

I checked the patches two ways on a fresh worktree at `00316ee7`:
- `git am r5-part{1,2,3}.patch` produces tree `2ab06ccb`, the same as `host-r5`;
- `git apply r5-combined.patch` also produces tree `2ab06ccb`.

A token grep over the patches and this report finds no match (see "No-push evidence").

## No-push evidence

```
$ git -C /tmp/roborev remote -v
origin  https://github.com/DannyAziz/roborev (fetch)
origin  https://github.com/DannyAziz/roborev (push)
$ git -C /tmp/roborev branch -vv | grep -E 'host-r5|r5-c'
  host-r5 8964d812 Add the auto agent keyword; host is its last resort only with the marker
+ r5-c1   c3c7c0b5 (/tmp/r5-c1) Add host review backend gated on ROBOREV_HOST_AGENT=1
+ r5-c2   4c17b8cf (/tmp/r5-c2) Detect Cursor cloud hosts; host is an automatic pick only with the marker
+ r5-c3   8964d812 (/tmp/r5-c3) Add the auto agent keyword; host is its last resort only with the marker
$ git -C /tmp/roborev for-each-ref --format='%(refname:short) %(upstream)' refs/heads/host-r5 refs/heads/r5-c1 refs/heads/r5-c2 refs/heads/r5-c3
host-r5
r5-c1
r5-c2
r5-c3                       (no upstream on any of them)
$ git -C /tmp/roborev for-each-ref refs/remotes | grep -c r5
0
$ git -C /workspace status -sb
## main...origin/main [behind 2]      (no local changes; the workspace was not modified)
```

- The origin URL above, as git prints it, contains no credentials. If a credential were embedded, it would be shown as `https://<redacted>@github.com/DannyAziz/roborev`.
- Once Ethan authorized it (13:36 UTC), I tried one push to the fork `https://github.com/ethanproia/roborev`. I used a temporary `fork` remote, removed afterwards. The refs were `host-backend-r5`, `host-backend-r5-c1..c3` and `r5-flake-harness`. The push failed on auth (`could not read Username for 'https://github.com'`), so **nothing was pushed**, and no other credentials were tried.
- The refs are in `/opt/cursor/artifacts/r5-refs.bundle` instead (`git bundle verify`: okay; it needs `00316ee7`):
  - `host-backend-r5` and `host-backend-r5-c3` = `8964d812`
  - `host-backend-r5-c1` = `c3c7c0b5`
  - `host-backend-r5-c2` = `4c17b8cf`
  - `r5-flake-harness` = `ac47f42a` (host-r5 plus `.r5-harness/`)
- The harness files are also in `/opt/cursor/artifacts/r5-harness/`.
- Nothing was pushed to `DannyAziz/roborev`, and no PR, issue, comment or other GitHub write was made anywhere.
- Every command ran with `ROBOREV_TELEMETRY_ENABLED=0`.
- `roborev update` was never run, and roborev was never installed into PATH. Binaries live only in `/tmp/r5-bin`.
- Environment variables are referred to by name only.
