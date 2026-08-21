# coding-agent-loop

An autonomous daemon that picks up labelled GitHub issues, has Claude Code implement them in an
isolated git worktree, and opens a **draft** pull request for you to review. It never merges
anything.

```
gh search issues --label agent-ready
        │
        ▼
   claim (SQLite lease, one issue per repo at a time)
        │
        ▼
   git worktree ──▶ claude -p --output-format stream-json
        │
        ▼
   run the repo's tests ──▶ commit ──▶ push ──▶ gh pr create --draft
```

## Table of contents

- [What this is](#what-this-is)
- [Principles](#principles)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [CLI flags](#cli-flags)
- [How work is selected](#how-work-is-selected)
- [Lifecycle of one issue](#lifecycle-of-one-issue)
- [Configuration reference](#configuration-reference)
  - [config.json](#configjson)
  - [models.json](#modelsjson)
- [When it stops](#when-it-stops)
- [Control API](#control-api)
- [Discord notifications](#discord-notifications)
- [Safety boundaries](#safety-boundaries)
- [Deploying with systemd](#deploying-with-systemd)
- [Layout](#layout)
- [Tests](#tests)
- [Troubleshooting](#troubleshooting)

## What this is

You label a GitHub issue `agent-ready`. The daemon finds it, checks out a fresh git worktree, and
first has Claude Code produce a **plan** — posted as an issue comment, no edits made. A human reviews
the plan and replies `implement` to approve it (or anything else, which sends it back for a
revision). Only then does the daemon run the actual change, run the repository's own test suite
against whatever Claude wrote, push a branch, and open a **draft** PR that says what happened —
including if the tests failed. A human reviews and merges (or doesn't). The daemon never pushes to
`main`, never merges, and never touches an issue that isn't opted in.

**Use case:** a backlog of well-scoped, low-risk issues (bug fixes, small features, chores) across
repositories you own, that you'd rather review as a diff than write yourself — worked on
continuously, in the background, using Claude subscription usage that would otherwise sit idle.

**Not the use case:** anything you want merged without a human reading it; large, ambiguous, or
architecturally significant changes; anything security- or compliance-sensitive that shouldn't be
handed to an agent running `--permission-mode bypassPermissions`.

## Principles

- **Never merges.** Every deliverable is a draft PR. The daemon's authority ends at "opened for
  review."
- **Opt-in per issue, veto per repo.** Discovery is org/user-wide, but nothing is touched unless it
  carries the trigger label, and `exclude_repos` overrides even a labelled issue.
- **The harness owns git and GitHub, not the agent.** Claude is told — in its system prompt — not to
  push, open PRs, or comment. All mutation is done by Go code outside the model's control, after
  the fact, over commands the model doesn't have the ability to run.
- **Usage-limit-aware, not schedule-aware.** There is no time-of-day window and no cost budget — the
  loop runs continuously and stops only when Claude actually reports a rate/usage limit, or when you
  pause it. This is a deliberate trade-off: it uses subscription usage that would otherwise be idle,
  including while you're working. `POST /pause` is the brake.
- **State lives in SQLite, not in GitHub labels.** Labels are a human-readable mirror; the `claims`
  table with lease expiry is what actually prevents double-work and is what survives a crash.
  A label can be edited by a human mid-run without corrupting anything.
- **Everything autonomous is inspectable.** Every run is logged to a JSONL transcript and a SQLite
  row with cost, tokens, and outcome; the control API exposes all of it.

## Requirements

- Go 1.26+
- [Claude Code](https://claude.com/claude-code) authenticated with a subscription (`claude`)
- [GitHub CLI](https://cli.github.com) authenticated with `repo` and `read:org` (`gh auth login`)
- `git`

Verify all of it at once:

```sh
make check
```

## Quick start

```sh
cp config.example.json config.json   # then edit github.owners
make build
make dry-run                         # one pass; never pushes, never opens a PR
make run                             # start the daemon
make install                         # install as a system daemon
sudo systemctl status coding-agent-loop.service # check status of process
sudo journalctl -u coding-agent-loop # check system daemon logs (or at ~/.agent-loop/logs/)
```

Label an issue `agent-ready` in one of the repositories you own. The next poll posts a plan as a
comment on the issue; reply `implement` on that comment to have the daemon carry it out.

## CLI flags

All flags on the built binary (`bin/coding-agent-loop`, or via `make run` / `make once` / `make dry-run`):

| Flag              | Default       | Effect                                                                   |
| ----------------- | ------------- | ------------------------------------------------------------------------ |
| `--config`        | `config.json` | path to the configuration file                                           |
| `--once`          | off           | run a single discovery pass, then exit (no server)                       |
| `--dry-run`       | off           | do everything except push branches, open PRs, or edit issues             |
| `--log-level`     | `info`        | `debug`, `info`, `warn`, or `error`                                      |
| `--no-server`     | off           | do not start the control API                                             |
| `--check`         | off           | run start-up checks (binaries, auth, config) and exit                    |
| `--install`       | off           | install + enable + start the systemd unit; **must run as root**          |
| `--print-service` | off           | print the embedded systemd unit to stdout and exit; no privileges needed |

## How work is selected

An issue is worked only if **all** of these hold:

- it carries the trigger label (`github.label`, default `agent-ready`) and is open
- its repository is not in `github.exclude_repos`
- no other issue in that repository is currently in flight
- no previous run delivered a PR for it, and it is not currently backing off after a failure
  (`run.retry_backoff`)
- no open pull request already closes it
- the issue's comment history says it's this issue's turn to be worked (see below)

**The issue's own comments are what decide the phase**, not a label or a database column. Every
comment the daemon posts is tagged with an invisible HTML marker, so it can tell its own narration
apart from a human reply:

- No plan comment yet → **plan**: post a plan, then stop.
- A plan is posted and nothing from a human has followed it → **wait**: do nothing this poll. This
  costs one `gh issue view` per poll, never a claim, a worktree, or a Claude run.
- The newest human reply after the plan is exactly `implement` (trimmed, case-insensitive) →
  **implement**: run the actual change.
- The newest human reply after the plan is anything else → **plan**: treat it as feedback and post a
  revised plan.

Labels mirror this state (`agent-planned` while waiting on a reply, `agent-working` →
`agent-done` / `agent-failed` around a run), but the SQLite claim table and the issue's own comments
are the source of truth — labels can be edited by humans mid-run, leases and comment history cannot.

Label edits are reconciled rather than fired blindly: the issue's current labels are read first, the
edit is reduced to what actually changes, a status label the repository does not define yet is
created on the fly, and a rejected combined edit is retried label by label. `gh issue edit` fails
the *whole* call on one unknown or not-actually-present label, which would otherwise leave an issue
carrying a stale `agent-working` long after the run ended. Every failed edit is logged, recorded as
a `labels_failed` run event, and reported to Discord.

Discovery is parallel across repositories; within one repository, work is serial (one issue in
flight at a time), controlled by `run.max_concurrent_repos`.

## Lifecycle of one issue

1. **Discover** — `gh search issues --label <label> --state open` scoped to `github.owners`, minus
   `exclude_repos`. The issue is fetched and its comments decide the phase (plan, wait, or
   implement) before anything is claimed.
2. **Claim** — an atomic SQLite insert with a lease (`run.lease`); losing the race means another
   worker already has it. The issue gets the `agent-working` label and a `runs` row.
3. **Workspace** — the repo is cloned once (checkout-less) into `workspace.repos_root`, then a
   `git worktree` is added under `workspace.root` on a fresh `agent/issue-<n>` branch off the
   default branch. Both the plan and implement phases get this same worktree; the plan phase just
   never writes to it.
4. **Plan** (skipped once approved) — `claude -p --permission-mode plan` (`claude.plan_permission_mode`)
   is handed the issue and told to produce a plan, not a change. Its final message is saved to SQLite
   and posted as an issue comment naming the files and approach it would take. The issue gets the
   `agent-planned` label and the run ends there — no commit, no verify, no push, no PR. Any human
   reply that isn't exactly `implement` triggers another plan pass that revises it against that
   feedback.
5. **Run Claude** (once approved) — `claude -p --output-format stream-json --permission-mode
   bypassPermissions`, scoped to the worktree, with a system prompt that hands over the issue and the
   approved plan and forbids git/GitHub mutation. The lease is renewed periodically while the run is
   in flight. The CLI's session ID is recorded against the run and the issue as soon as it is
   announced — before any result, so a killed or timed-out run still leaves one behind — and is
   queryable later via `GET /sessions`.
6. **Verify** — the repository's own test command runs (auto-detected, or from `verify.commands`).
   A failing suite does **not** block the PR — it's a draft either way, and the failure is reported
   in the PR body so a human sees it immediately.
7. **Deliver** — the remote is re-checked, then pushed; a draft PR is opened with `Closes #<n>`,
   verification result, model used, and cost; the issue is commented with the PR link;
   `agent-working` and `agent-planned` are swapped for `agent-done` or `agent-failed`.
8. **Cleanup** — the worktree is removed (kept on disk if `workspace.keep_failed` and the run
   failed, for post-mortem). The claim is always released, on every exit path.

Failures are retried indefinitely behind an exponential back-off: `run.retry_backoff` after the
first failure, doubling with each consecutive one, capped at `run.retry_backoff_max`. Each attempt
starts one rung lower on the `models.json` ladder, wrapping back to the top once the ladder has been
walked. **Nothing is abandoned for failing too often** — an issue that keeps failing gets slower, not
dropped, so it is never left stale. The trigger label remains the only thing that decides whether an
issue is worked at all: remove `agent-ready` to stop the retries.

A usage limit hit mid-run is recorded as `deferred` — neither an attempt nor a failure, so it
neither extends the back-off nor drops the issue down the ladder.

## Configuration reference

### config.json

Copy `config.example.json` and edit. Durations are Go duration strings (`"5m"`, `"45m"`, `"90m"`).
`~` is expanded to your home directory.

```json
{
  "github": {
    "label": "agent-ready",
    "working_label": "agent-working",
    "done_label": "agent-done",
    "failed_label": "agent-failed",
    "plan_label": "agent-planned",
    "owners": ["ableinc"],
    "exclude_repos": [],
    "search_limit": 50,
    "poll_interval": "5m",
    "binary": "gh"
  },
  "workspace": {
    "root": "~/.agent-loop/work",
    "repos_root": "~/.agent-loop/repos",
    "logs_root": "~/.agent-loop/logs",
    "keep_failed": true,
    "branch_prefix": "agent/issue-"
  },
  "run": {
    "max_concurrent_repos": 3,
    "timeout": "45m",
    "lease": "90m",
    "verify_timeout": "10m",
    "retry_backoff": "15m",
    "retry_backoff_max": "24h"
  },
  "claude": {
    "binary": "claude",
    "permission_mode": "bypassPermissions",
    "plan_permission_mode": "plan",
    "extra_args": [],
    "usage_poll_interval": "15m",
    "usage_backoff": "15m",
    "credentials_path": "~/.claude/.credentials.json",
    "usage_cache_path": "~/.agent-loop/usage-cache.json"
  },
  "verify": {
    "auto_detect": true,
    "commands": {}
  },
  "server": {
    "addr": "127.0.0.1:8787"
  },
  "store": {
    "path": "~/.agent-loop/state.db"
  },
  "discord": {
    "enabled": false,
    "webhook_url": ""
  },
  "models_path": "models.json"
}
```

| Field                                                  | Meaning                                                                                                                            |
| ------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------- |
| `github.label`                                         | trigger label; **must not be empty**, or every open issue would match                                                              |
| `github.working_label` / `done_label` / `failed_label` | status labels the daemon swaps `label` for                                                                                         |
| `github.plan_label`                                    | label added while a posted plan awaits an `implement` reply, removed once the change is delivered                                  |
| `github.owners`                                        | users/orgs to search; empty means every repo the `gh` token can see (a boot-time warning fires if left empty)                      |
| `github.exclude_repos`                                 | `owner/name` repos to never touch, even if labelled                                                                                |
| `github.search_limit`                                  | max issues fetched per discovery pass                                                                                              |
| `github.poll_interval`                                 | how often discovery runs                                                                                                           |
| `workspace.root`                                       | where per-issue worktrees live                                                                                                     |
| `workspace.repos_root`                                 | where the one-per-repo checkout-less clones live                                                                                   |
| `workspace.logs_root`                                  | where JSONL run transcripts are written                                                                                            |
| `workspace.keep_failed`                                | keep a failed run's worktree on disk for inspection instead of deleting it                                                         |
| `run.max_concurrent_repos`                             | how many repos can have work in flight simultaneously                                                                              |
| `run.retry_backoff`                                    | wait before a failed issue may be claimed again; doubles with each consecutive failure                                             |
| `run.retry_backoff_max`                                | cap on that doubling; must be `>= run.retry_backoff`                                                                               |
| `run.timeout`                                          | wall-clock limit for one Claude invocation                                                                                         |
| `run.lease`                                            | how long a claim is held before it's considered abandoned; **must exceed `run.timeout`**, or a still-running claim could be stolen |
| `run.verify_timeout`                                   | wall-clock limit for the test command                                                                                              |
| `claude.binary`                                        | executable name/path for the Claude Code CLI                                                                                       |
| `claude.permission_mode`                               | passed through as `--permission-mode` for the implement run                                                                        |
| `claude.plan_permission_mode`                          | passed through as `--permission-mode` for the read-only planning run                                                               |
| `claude.usage_poll_interval` / `usage_backoff`         | advisory OAuth usage poll cadence and 429 backoff; **must be ≥ 1m**                                                                |
| `claude.credentials_path`                              | where the CLI's OAuth token lives, read for the advisory usage snapshot                                                            |
| `verify.auto_detect`                                   | try `Makefile` → `go.mod` → `package.json` → `Cargo.toml` → `pyproject.toml`, in that order                                        |
| `verify.commands`                                      | per-repo override, keyed `"owner/name": "shell command"`                                                                           |
| `server.addr`                                          | control API bind address; keep loopback-only                                                                                       |
| `store.path`                                           | SQLite database path                                                                                                               |
| `discord.enabled`                                      | turn on Discord status notifications (see [Discord notifications](#discord-notifications))                                         |
| `discord.webhook_url`                                  | the channel's incoming webhook URL; **required** if `discord.enabled` is true                                                      |
| `models_path`                                          | path to `models.json`                                                                                                              |

### models.json

The ladder the loop climbs down when a model is rate-limited or fails. No model identifier is
hardcoded in Go — this file is the only place to update one.

```json
{
  "models": [
    {
      "id": "claude-opus-5",
      "alias": "opus",
      "roles": ["plan", "implement"],
      "priority": 1
    },
    {
      "id": "claude-sonnet-5",
      "alias": "sonnet",
      "roles": ["plan", "implement"],
      "priority": 2
    },
    {
      "id": "claude-haiku-4-5",
      "alias": "haiku",
      "roles": ["triage"],
      "priority": 1
    }
  ]
}
```

- `id` is the canonical model identifier, recorded in the `runs` table so history stays meaningful
  even if an alias's meaning shifts later.
- `alias` is what's passed on the CLI.
- `roles` — `plan` (writes the plan comment) and `implement` (does the work) each have their own
  ladder, selected independently per phase; a registry needs at least one model serving each or the
  loop refuses to start. `triage` is reserved for a future pre-pass and currently unused.
- `priority` — lower runs first within a role. The head of the priority-ordered list for a role
  becomes `--model`; the rest become `--fallback-model a,b,...`.
- A model that fails or hits a limit is put on a 30-minute cooldown, so the next attempt starts
  lower on the ladder. Retries also start one rung lower for every failure recorded on the issue,
  wrapping back to the head once the ladder has been walked, since retries are unbounded — a
  successful plan run does not demote the implement run that follows it, since it isn't a failure.
  If every candidate is cooled down, the full ladder is used anyway — refusing to run at all is
  worse; the usage gate is the real brake.

The content above is also **embedded in the binary** (`internal/models/default.json`) as of build
time, so a `coding-agent-loop` binary copied to a host on its own — no repo checkout, no
`models.json` alongside it — still boots with a working ladder. A `models.json` at `models_path`
(default `./models.json`, next to wherever the binary runs from) always takes precedence when
present, so you can still customize the ladder on a given host without rebuilding.

## When it stops

The loop runs around the clock and stops for exactly two reasons:

1. **A usage limit.** When the Claude CLI reports one, the gate closes until the reset time it
   gave, or a 5m → 15m → 30m → 60m backoff when it gave none. In-flight runs finish; only new
   claims are blocked.
2. **You paused it** — `curl -XPOST localhost:8787/pause`.

The OAuth usage endpoint is also polled (throttled, with 429 backoff) but is **advisory only**: it
populates `/status` and never closes the gate, because it rate-limits hard and its payload is not
contractual.

## Control API

Loopback-only by default. It can pause and cancel work, so do not expose it.

| Route                        | Purpose                                                             |
| ---------------------------- | ------------------------------------------------------------------- |
| `GET /healthz`               | liveness                                                            |
| `GET /status`                | gate state, in-flight runs, claims, model cooldowns, usage snapshot |
| `GET /runs?limit=&repo=`     | recent runs with outcome, model, cost, PR link, created/started/ended timestamps |
| `GET /runs/{id}`             | one run plus its event timeline                                     |
| `GET /runs/{id}/log`         | the raw JSONL transcript of the Claude run                          |
| `GET /sessions?repo=&issue=&limit=` | Claude session IDs recorded per repo/issue, newest first     |
| `POST /pause` `POST /resume` | stop / resume claiming new work                                     |
| `POST /runs/{id}/cancel`     | cancel an in-flight run                                             |

## Discord notifications

Optional, **one-way** status updates posted to a Discord channel (e.g. `#coding-agent-loop`) via an
incoming webhook. The daemon only ever POSTs to it — it never reads the channel, never listens for
messages, and never takes requests from it. There is nothing to secure against on the receiving
end because a webhook URL is push-only by construction.

To enable it:

1. In Discord: channel settings → Integrations → Webhooks → New Webhook, then copy its URL.
2. In `config.json`:

   ```json
   "discord": {
     "enabled": true,
     "webhook_url": "https://discord.com/api/webhooks/..."
   }
   ```

Once enabled, every one of these posts an embed. Run notifications lead with the issue's own title,
linked, and carry the run ID and attempt number:

- **Run claimed** — plus how many earlier attempts on that issue failed, when it is a retry.
- **Claude finished** — model, session ID, turns, cost, tokens in/out, wall-clock duration.
- **Verification** — passed, failed, or skipped, with the command and, on failure, the tail of the
  test output, so the channel says what broke without opening the PR.
- **Draft PR opened** — link, model, session ID, cost, verification status, total run duration,
  diffstat.
- **Run failed** — the cause and **when the next attempt is due** (retries are unbounded, so "when"
  is the useful number).
- **Run abandoned** — a run that was skipped rather than attempted: the issue closed, lost its
  label, or is already covered by a PR.
- **Run deferred** — a run the usage gate stopped. Neither an attempt nor a failure.
- **Run cancelled** — `POST /runs/{id}/cancel`.
- **Label update failed** — the labels on GitHub now disagree with the store, and which edit was
  refused. Otherwise invisible.
- **Model cooled down** — which model, until when, and why, so a run served from lower on the ladder
  than expected is explainable.
- **Usage gate** — closes (with reason and until-when) and clears.
- **Pause / resume** — whenever `POST /pause` or `POST /resume` is called.
- **Daemon start / stop** — startup states the trigger label, owners, poll interval, concurrency,
  retry back-off, and whether it is a dry run; shutdown says graceful or crash. (Skipped for
  `--once` passes, which aren't really "the daemon.")

A Discord outage, timeout, or rate-limit is logged and dropped — it never blocks, delays, or fails
an actual run. Leave `discord.enabled` false (the default) to disable it entirely; no network calls
are made when it's off.

## Safety boundaries

Claude runs with `--permission-mode bypassPermissions`, so within a run it can do anything your
user can. What constrains it:

- **The label** is per-issue opt-in; **`exclude_repos`** is the per-repo veto.
- **The remote is re-checked immediately before every push** — a worktree whose `origin` is not the
  repository the run claimed is refused.
- **The harness owns git and GitHub.** The system prompt forbids the agent from pushing, opening
  PRs, commenting, or rewriting history; the daemon does all of it.
- **Nothing is ever merged**, and every PR is a draft.
- Child processes run in their own process group and are killed as a group on timeout, so a
  runaway grandchild (a stray build/test process) can't outlive the run.

`--install` below scopes the systemd unit's filesystem access to `/opt/coding-agent-loop` and its
own `~/.agent-loop` regardless of which account it runs as (see
[Deploying with systemd](#deploying-with-systemd) for why that's normally your own account, not a
separate one).

## Deploying with systemd

The unit is a template embedded in the binary — `internal/install/coding-agent-loop.service` is its
only source, compiled in via `go:embed`. `--install` renders it against whichever account will
actually run the service, then writes, enables, and starts it.

```sh
sudo bin/coding-agent-loop --install --config config.json
```

**Run this via `sudo` from your own already-authenticated account** (the one with `gh auth login`
and Claude Code already logged in) — not as a `root` login shell. `--install` reads `$SUDO_USER`,
which `sudo` sets to the account that invoked it, and runs the service as _that_ account. This
matters because gh and Claude Code store their auth per-user (`~/.config/gh`,
`~/.claude/.credentials.json`); a service running as anyone else has no credentials to use, which
surfaces as `claude binary "claude" is not on PATH` or `gh is not authenticated` in the journal even
when both work fine interactively.

`--install` (root required):

1. resolves the target account from `$SUDO_USER` (falling back to creating an isolated
   `coding-agent-loop` system user only if there is no `$SUDO_USER` — e.g. already logged in as
   root — in which case you must separately run `sudo -u coding-agent-loop gh auth login` and the
   Claude Code login flow before the service can do anything);
2. copies the running binary to `/opt/coding-agent-loop/bin/coding-agent-loop`;
3. copies `--config` to `/opt/coding-agent-loop/config.json` — **never** overwriting one already there;
4. writes `/etc/systemd/system/coding-agent-loop.service` with `User=`/`Group=` set to that account;
5. runs `systemctl daemon-reload` then `enable --now coding-agent-loop.service`;
6. confirms the unit reached `active`.

Re-running it is safe: it replaces the binary but leaves an existing config untouched.

```sh
bin/coding-agent-loop --print-service   # print the unit --install would write, e.g. run as yourself
```

`--print-service` needs no privileges; it renders the unit for whichever account would be resolved
right now (also honoring `$SUDO_USER` if set) and writes it to stdout, so you can check who it would
run as before committing to `--install`.

The unit itself: `Type=simple`, restarts on failure, `KillSignal=SIGTERM` with
`TimeoutStopSec=50m` (longer than one run timeout, so a SIGTERM drains in-flight work instead of
killing it mid-push), and hardened with `ProtectSystem=strict`, `ProtectHome=read-only`,
`NoNewPrivileges=true`, and `ReadWritePaths` scoped to `/opt/coding-agent-loop` and the service
account's own `~/.agent-loop`.

Common operations once installed:

```sh
sudo systemctl status coding-agent-loop      # is it running
journalctl -u coding-agent-loop -f           # tail logs
sudo systemctl stop coding-agent-loop        # drains in-flight work, then stops
sudo systemctl restart coding-agent-loop
curl localhost:8787/status            # gate/run state (from the host, not the service user)
```

## Layout

| Path                    | Role                                                   |
| ----------------------- | ------------------------------------------------------ |
| `cmd/agent.go`          | flags, boot checks, wiring                             |
| `internal/config`       | configuration load and validation                      |
| `internal/models`       | models.json, ladder selection, embedded default ladder |
| `internal/store`        | SQLite: claims, runs, gates, events, sessions          |
| `internal/gh`           | GitHub CLI wrapper                                     |
| `internal/git`          | clones and worktrees                                   |
| `internal/claude`       | headless Claude runs, stream-json parsing              |
| `internal/gate`         | usage-limit detection and pausing                      |
| `internal/verify`       | detect and run the repo's tests                        |
| `internal/orchestrator` | the loop, prompts, PR reports                          |
| `internal/server`       | Fiber v3 control API                                   |
| `internal/proc`         | process-group isolation                                |
| `internal/install`      | embedded systemd unit, `--install`                     |

## Tests

```sh
make test    # go test -race ./...
```

External binaries are stubbed with shell scripts on disk, so the whole pipeline is exercised
without network access or subscription usage.

## Troubleshooting

| Symptom                                                                           | Likely cause                                                                                                                                                                                                                                                                                                                                                                                                                   |
| --------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `start-up checks failed: gh is not authenticated`                                 | run `gh auth login` with `repo` + `read:org` scopes                                                                                                                                                                                                                                                                                                                                                                            |
| `github.owners is empty: discovery will scan every repository this token can see` | expected if `owners` is unset; narrow it in `config.json` if that's not what you want                                                                                                                                                                                                                                                                                                                                          |
| Nothing ever gets picked up                                                       | confirm an issue actually carries the exact `github.label` value and is open, and its repo isn't in `exclude_repos`                                                                                                                                                                                                                                                                                                            |
| `/status` shows `claiming_work: false`                                            | check `gates` in the response — either a usage limit is active (wait for `blocked_until`) or someone called `POST /pause`                                                                                                                                                                                                                                                                                                      |
| A PR opened but tests show as failed                                              | expected behavior, not a bug — verification failures are reported in the draft PR body rather than blocking it                                                                                                                                                                                                                                                                                                                 |
| `--install` fails with a permissions error                                        | it must run as root (`sudo`); it writes to `/etc/systemd/system` and `/opt`                                                                                                                                                                                                                                                                                                                                                    |
| Service won't start after `--install`                                             | `journalctl -u coding-agent-loop -e`; most often a missing/invalid `/opt/coding-agent-loop/config.json`                                                                                                                                                                                                                                                                                                                        |
| `git clone` fails with `could not read Username ... terminal prompts disabled`    | gh's git credential helper is normally found by name on `$PATH`, and a systemd service's `$PATH` is minimal — this should no longer happen since the daemon resolves `github.binary` to an absolute path and forces git to use it directly as the credential helper (`internal/git/workspace.go`); if you still see it, confirm `gh auth status` succeeds as the account the service runs as (`sudo -u <user> gh auth status`) |
