# r5 flake harness

This is the test harness for the r5 host backend, `host-backend-r5` at `8964d812`. The rest of the report is in `r5-report.md`, which ships with the r5 artifacts.

## Daemon flake tests still to run

On the original VM, every mode finished the 16 `internal/agent` and 2 `internal/review` flake tests: 3000 runs each, 0 failures. None of the 8 `internal/daemon` tests finished in any mode. Each of the 8 tests below must run in each of the 4 modes, which is 32 jobs:

| `go test -run` | Package | Approx. time per 3000 runs, unloaded |
|----------------|---------|-------------------------------:|
| `^TestHostRulingByMarkerMode$` | `./internal/daemon` | 46 min |
| `^TestHostFailoverByMarkerMode$` | `./internal/daemon` | 17 min |
| `^TestEmptyAutoPinIsFrozen$` | `./internal/daemon` | 1 min |
| `^TestAutoPinnedHostIsRecheckedOnRestart$` | `./internal/daemon` | 2 min |
| `^TestWorkerExplicitHostWithoutMarkerFailsWithoutRetry$` | `./internal/daemon` | 3 min |
| `^TestSynthesisHostTimeoutSkipsRetries$` | `./internal/daemon` | 7 min |
| `^TestSynthesisNonHostErrorIsStillRetried$` | `./internal/daemon` | 1 min |
| `^TestWorkerNonHostGetAgentErrorIsStillRetried$` | `./internal/daemon` | 3 min |

The iteration count is **3000 per test per mode** (`-count=3000`).

## Marker modes

| Mode | Environment |
|------|-------------|
| `neither` | `ROBOREV_HOST_AGENT` unset; every `CURSOR_AGENT*` and `CLOUD_AGENT_*` variable removed (`env -u`) |
| `detected` | `ROBOREV_HOST_AGENT` unset; `CURSOR_AGENT=1`; `CLOUD_AGENT_FLAKE=1` |
| `1` | `ROBOREV_HOST_AGENT=1` |
| `0` | `ROBOREV_HOST_AGENT=0` |

Every mode also sets the following:
- `ROBOREV_TELEMETRY_ENABLED=0`.
- `GIT_CONFIG_GLOBAL=.r5-harness/r5-gitconfig`, a test name and email with commit signing off. A global git config that signs commits through an agent can stall test setup for 30 seconds.

## Running one job

From a checkout of `host-backend-r5` (or this branch), with `go` and `git` on PATH:

```bash
.r5-harness/flake-daemon.sh TestHostRulingByMarkerMode neither 3000
```

It builds `internal/daemon` once into `.r5-harness/bin/daemon.test`. It then runs the test with `-test.run '^Name$' -test.count=3000 -test.v` and writes the log to `.r5-harness/out/<mode>-<Name>.log`. It prints:

```
daemon TestHostRulingByMarkerMode mode=neither runs=3000 pass=3000 fail=0 exit=0
```

## Pass criterion

A job passes when all of the following hold:
- `pass=3000`, counting top-level `--- PASS: <Name> ` lines;
- `fail=0`, counting top-level `--- FAIL: <Name> ` lines;
- `exit=0`.

The flake run passes when all 32 jobs pass.

On Linux, daemon tests can leave `dbus-daemon` processes behind that use up inotify instances. `dbus-reaper.sh` is the loop that kills orphans on the original VM; run it alongside the jobs if `inotify` errors appear.

## Other scripts

These were used on the original VM. They have that VM's `/tmp` paths hard-coded (`/tmp/r5-c1..c3` worktrees, `/tmp/r5-bin`), so treat them as records of what ran.

| Script | Purpose |
|--------|---------|
| `flake5.sh <mode>` | all 27 flake tests in one mode, from prebuilt test binaries in `/tmp/r5-bin` |
| `flake-par.sh` | builds the agent, review and daemon test binaries and runs `flake5.sh` for the 4 modes in parallel |
| `suite5.sh <dir> <tag> [modes]` | `go test -count=1 ./...` in each marker mode |
| `allsuites.sh` | vet, gofmt and `suite5.sh` for each commit worktree |
| `basetests.sh <dir> <tag>` | the `00316ee7` test files against head prod code, marker unset |
| `golden_cli.sh <binary> <out>` | CLI golden: doctor, check-agents, completion, enqueue and error paths under 8 configs, marker unset |
| `mutate_r5.py [filter]` | the 53-mutant sweep; each mutant must turn its named tests red |
| `probe5.sh`, `probes-final.sh` | real-daemon probes per marker mode |
| `chain2.sh` | the order the steps above were chained in |
| `dbus-reaper.sh` | kills orphaned `dbus-daemon` processes |
