# Agent setup runbook

Follow this runbook when a user says “get this ready for me”, asks to install
personal requested-review automation, or wants to change its publishing mode.
Success means a verified local setup and a clear handoff, not merely a config
file. Execute the steps you can perform; pause for human login or authorization
where required.

Use [the setup guide](personal-reviews-setup.md) for build commands, the config
schema and publication behavior. Keep credentials, runtime data and private
review content outside this checkout. Setup is an operator task: it does not
require source edits, a new branch, commits or a push.

## 1. Establish the user's choices

Collect missing choices in one short question. Reuse answers already provided:

- Explicit repository allowlist, in `owner/repo` form.
- Review harness; default to Claude Code if the user has no preference.
- Optional model; omit it to use that harness's default.
- Instructions; default to “Adversarial review.”
- Desired mode: dry-run, comments only, or full automation.
- Whether to run automatically in a user-level background service.

Identify the reviewer from the authenticated GitHub user, then confirm any
mismatch with the user's intended account. Keep the detected login as an
explicit string, never `@me`. Each person supplies their own authentication. A
borrowed rubric does not authorize publishing as its author.

“Get this ready” authorizes preparation, not live GitHub writes or installing a
persistent service. Default to dry-run. Record those two authorizations
separately before enabling them; an explicitly requested mode is a choice, not
evidence that credentials or results have been verified.

**Done:** repository scope, identity, harness and rubric are known; absent
publishing/service authorizations are recorded as absent.

## 2. Inspect and build safely

Confirm this checkout has `internal/config/personal.go` and is on the fork's
`personal-reviews` branch. If not, stop and ask permission before changing
branches, or offer the setup guide's explicit clone command in a new directory.
Preserve existing installations and working trees.

Check the OS, Go version required by `go.mod`, GitHub CLI and the chosen
harness. Install missing prerequisites only with permission. Build the distinct
binary using the setup guide's self-update-disable flag. Keep it outside the
user's production RoboRev binary path. Do not install hooks or call
`roborev init`.

Verify the built binary's `update --force --yes --no-restart` command exits with
“self-updates are disabled”. This is a safe verification only for the newly
built, disabled-update binary; never run it on an existing production binary.

**Done:** the fork binary exists at an absolute path, and forced upstream
updates are demonstrably blocked.

## 3. Verify access and prepare private state

Use `gh api user --jq .login` to verify identity and read-only GitHub API calls
to verify access to each allowed repository. Use the harness's supported auth
status check when available. A version check alone does not verify model access.
If login is needed, let the user complete the provider's login flow. Keep tokens
out of chat, commands, Git and service definitions.

For a service on Unix, also verify GitHub identity with `GH_TOKEN` and
`GITHUB_TOKEN` unset in that child process. A service may use stored GitHub CLI
credentials rather than the interactive shell's token overrides. If identities
differ, resolve that mismatch before installation; do not copy a token into a
plist or unit file to hide it.

Choose a dedicated absolute `ROBOREV_DATA_DIR` outside the checkout. Inspect
existing state before reusing it. Preserve evaluation reports and any uncertain
publication records. Use owner-only directory/config permissions where
supported. Generate the setup guide's config with the user's choices, initially
setting `ci.enabled = false`, `mode = "dry-run"`, `approve = false` and
`request_changes = false`. Keep telemetry disabled unless the user wants it.

**Done:** the intended identity can read every allowed repository; private
config and state paths are isolated from source and any production daemon.

## 4. Run a supervised dry run

Ask permission to execute real model calls if that permission is not already
explicit. Explain that they can incur costs and read the selected private PR
code. Find eligible open PRs through read-only API calls: direct user requests,
not team-only requests; exclude drafts and self-authored PRs. Report the
eligible count without copying private titles or code into repository documents.

If none qualify, finish preparation and report “live review path not exercised”.
Do not create a PR, assign reviewers, or use an unrelated PR to manufacture a
test.

For an authorized test, use a temporary config with CI enabled, dry-run mode and
a long poll interval so only the initial polling cycle runs during the test.
Launch `daemon run` with an ephemeral loopback address. Supervise its exact PID,
set a stated evaluation deadline, and guarantee shutdown on completion, error or
interruption. Preserve the persistent config's disabled CI setting.

Use the daemon HTTP API to inspect job completion and results; do not invoke the
`roborev review` command. Require a successful reviewer and usable authoritative
result, plus a dry-run publication log/proposal for the same reviewed SHA. Store
reports privately. A `COMMENT` proposal is expected while approval and
change-request permissions are disabled, even if the model reports a failure.

**Done:** either the review and proposal were verified or the limitation/failure
is reported; the temporary process is stopped and no GitHub review was
submitted.

## 5. Activate only the approved mode

Present a concise activation summary: reviewer, repositories, harness/model,
rubric, mode, data directory and whether a service will run. Obtain explicit
approval for real publishing and, separately, persistent background execution.

Use the setup guide's mode keys. Comments-only publishing keeps both verdict
permissions false. Full automation enables both permissions. Keep the explicit
allowlist and authenticated reviewer unchanged unless the user approves changes.
Old queued dry runs stay frozen. For the first live evaluation, use a fresh live
data directory rather than editing stored job snapshots or upgrading a proposal.

If a service is authorized, use a unique user-level service name, absolute
paths, the selected data directory and a PATH containing GitHub CLI and the
harness. Use a launchd LaunchAgent on macOS, or a user systemd unit on Linux
when available. If the user's OS/service manager is unsupported, offer
foreground execution and report the limitation instead of guessing installation
commands. Use stored provider credentials; keep secrets out of service
environment definitions.

Check for another live instance covering the same reviewer/repositories before
starting. Confirm stale-approval dismissal with read-only repository settings
when visible. If it cannot be checked, disclose that limitation; never change
repository rules without authorization. Environment filtering is not a
filesystem sandbox.

**Done:** the authorized service is reachable through its own runtime metadata,
has the correct effective config and identity, and is not restarting in an error
loop. A queued/running first review is not evidence of successful publication;
state its actual status. Uncertain submissions require reconciliation, not
retries by deleting state or inventing a new operation.

## 6. Hand off operation

Give the user:

- The active mode and scope, plus actual verification results and limitations.
- Absolute binary, config, data, log and service-definition paths.
- Exact commands to inspect and stop their specific service.
- The persistent pause method: set `ci.enabled = false` in the active config.
- Any human action still needed, and whether a real GitHub review was submitted.

Explain that laptop automation pauses while the machine sleeps or its user
service is inactive. Restart policies can relaunch a stopped process; use the
service manager's unload/disable operation when stopping it entirely.

Update by rebuilding reviewed fork source with self-updates disabled, stopping
only this instance, replacing only its dedicated binary and restarting it.
Preserve databases and publication journals during upgrades. Never install the
upstream release over this service or enable publishing merely to finish setup.
