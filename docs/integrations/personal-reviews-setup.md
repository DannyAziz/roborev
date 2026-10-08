# Personal requested-review setup

This fork adds background reviews for PRs that directly request your GitHub
user. It supports personal instructions, models and existing RoboRev harnesses.
Each person runs a separate instance with their own GitHub and model
credentials.

The implementation is on the `personal-reviews` branch. Do not install the
upstream release binary: it does not include this feature.

## Ask an agent to set it up

Point your agent at this fork's `personal-reviews` branch and say:

> Get this ready for me. Read `AGENTS.md` and follow
> `docs/integrations/personal-reviews-agent-setup.md`. Ask for any missing
> choices and verify a dry run before enabling live publishing or a background
> service.

The [agent runbook](personal-reviews-agent-setup.md) defines the setup sequence,
authorization gates, verification and handoff. Supply your repository allowlist
and preferred harness/rubric if you already know them.

## Clone and build

Install Go 1.27 or later, GitHub CLI, and your preferred review harness first.
This example uses Claude Code; Pi, Codex and Cursor are also supported.

```bash
git clone --branch personal-reviews https://github.com/DannyAziz/roborev.git
cd roborev
mkdir -p bin
CGO_ENABLED=0 go build \
  -ldflags '-X go.kenn.io/roborev/internal/update.selfUpdatesDisabled=true' \
  -o bin/personal-review ./cmd/roborev
```

Keep this binary distinct from an existing RoboRev installation. The build flag
blocks upstream update checks and installation, including forced updates. Update
this binary by rebuilding from reviewed fork source, never with
`roborev update`.

## Configure your identity and rubric

Authenticate GitHub CLI and the chosen harness using their own supported login
flows. Verify `gh api user --jq .login` returns the user you intend to review
as. Never copy another person's tokens or authentication files into this
repository.

Choose a fresh data directory outside the source checkout:

```bash
export ROBOREV_DATA_DIR="$HOME/.local/share/personal-review"
mkdir -p "$ROBOREV_DATA_DIR"
chmod 700 "$ROBOREV_DATA_DIR"
```

Create `config.toml` in that directory with the following contents. Replace the
repository and reviewer placeholders. Omit `model` to use the harness default,
or set it on the subagent to a model supported by that harness.

```toml
max_workers = 1

[ci]
enabled = true
repos = ["example/project"]
poll_interval = "1m"
throttle_interval = "0"

[ci.personal]
enabled = true
reviewer = "your-github-login"
panel = "personal"
mode = "dry-run"
approve = false
request_changes = false

[review.subagents.personal]
agent = "claude-code"
instructions = "Adversarial review."

[review.panels.personal]
members = ["personal"]
synthesis_agent = "claude-code"
```

```bash
chmod 600 "$ROBOREV_DATA_DIR/config.toml"
bin/personal-review daemon run \
  --config "$ROBOREV_DATA_DIR/config.toml" --addr 127.0.0.1:0
```

Run in the foreground first and stop with Ctrl-C. A dry run executes real model
calls, which may incur costs, but writes no GitHub reviews, comments or checks.
Only directly requested, non-draft, non-self-authored PRs in the allowlist
qualify.

## Enable full automation deliberately

After inspecting a dry run, stop the evaluation instance. For a first live test,
use a separate fresh data directory and config, changing only these policy keys:

```toml
mode = "publish"
approve = true
request_changes = true
```

These keys belong under `[ci.personal]`. This enables real writes as the
verified GitHub user. Approval requires a complete panel, explicit passes and
zero findings. Actionable explicit failures may request changes; other usable
results comment. Previously queued dry runs cannot be upgraded into live
approvals.

Confirm stale-approval dismissal is enabled in repository rules. Submission is
pinned to the reviewed commit, but GitHub provides no atomic current-head gate.
Use trusted contributors initially: CI credential filtering is not a filesystem
sandbox. Ambiguous submissions are reconciled, never blindly resent.

Once verified, run the same foreground command under your own user-level process
manager, with absolute binary/config paths, the data directory environment, and
a PATH containing your harness and GitHub CLI. Do not share databases between
instances, or run multiple live instances for the same reviewer and repository.
On a laptop, polling runs only while the machine is awake and the process is
active.

To pause new intake and pending publication, set `ci.enabled = false`. To stop
the process entirely, stop it through your process manager as well; otherwise a
configured restart policy can restart it.

## What stays private

Keep these outside Git and do not attach them to issues or public PRs:

- Your real repository allowlist and runtime config.
- GitHub tokens, harness authentication files, API keys and environment files.
- SQLite databases and their WAL/SHM files, runtime metadata and service logs.
- Review reports, prompts, patches, isolated clones and snapshots of private
    code.
- Machine-specific service definitions that contain local paths or credentials.

Sharing a rubric does not share an identity. Running someone else's instructions
still publishes as the authenticated, configured GitHub user. Delegation
requires separate explicit credential authorization, not just a copied prompt.

See [Personal review behavior](github.md#personal-requested-reviews-fork) for
selection, publication and recovery details.
