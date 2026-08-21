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
- [Safety boundaries](#safety-boundaries)
- [Deploying with systemd](#deploying-with-systemd)
- [Layout](#layout)
- [Tests](#tests)
- [Troubleshooting](#troubleshooting)

## What this is

You label a GitHub issue `agent-ready`. The daemon finds it, checks out a fresh git worktree, hands
the issue to Claude Code running headless, runs the repository's own test suite against whatever
Claude wrote, pushes a branch, and opens a **draft** PR that says what happened — including if the
tests failed. A human reviews and merges (or doesn't). The daemon never pushes to `main`, never
merges, and never touches an issue that isn't opted in.

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
```

Label an issue `agent-ready` in one of the repositories you own, and the next poll picks it up.

## CLI flags

All flags on the built binary (`bin/coding-agent-loop`, or via `make run` / `make once` / `make dry-run`):

| Flag | Default | Effect |
|---|---|---|
| `--config` | `config.json` | path to the configuration file |
| `--once` | off | run a single discovery pass, then exit (no server) |
| `--dry-run` | off | do everything except push branches, open PRs, or edit issues |
| `--log-level` | `info` | `debug`, `info`, `warn`, or `error` |
| `--no-server` | off | do not start the control API |
| `--check` | off | run start-up checks (binaries, auth, config) and exit |
| `--install` | off | install + enable + start the systemd unit; **must run as root** |
| `--print-service` | off | print the embedded systemd unit to stdout and exit; no privileges needed |

## How work is selected

An issue is worked only if **all** of these hold:

- it carries the trigger label (`github.label`, default `agent-ready`) and is open
- its repository is not in `github.exclude_repos`
- no other issue in that repository is currently in flight
- no previous run delivered a PR for it, and it has attempts remaining (`run.max_attempts`)
- no open pull request already closes it

Labels mirror the state (`agent-working` → `agent-done` / `agent-failed`), but the SQLite claim
table is the source of truth — labels can be edited by humans mid-run, leases cannot.

Discovery is parallel across repositories; within one repository, work is serial (one issue in
flight at a time), controlled by `run.max_concurrent_repos`.

## Lifecycle of one issue

1. **Discover** — `gh search issues --label <label> --state open` scoped to `github.owners`, minus
   `exclude_repos`.
2. **Claim** — an atomic SQLite insert with a lease (`run.lease`); losing the race means another
   worker already has it. The issue gets the `agent-working` label and a `runs` row.
3. **Workspace** — the repo is cloned once (checkout-less) into `workspace.repos_root`, then a
   `git worktree` is added under `workspace.root` on a fresh `agent/issue-<n>` branch off the
   default branch.
4. **Run Claude** — `claude -p --output-format stream-json --permission-mode bypassPermissions`,
   scoped to the worktree, with a system prompt that hands over the issue and forbids git/GitHub
   mutation. The lease is renewed periodically while the run is in flight.
5. **Verify** — the repository's own test command runs (auto-detected, or from `verify.commands`).
   A failing suite does **not** block the PR — it's a draft either way, and the failure is reported
   in the PR body so a human sees it immediately.
6. **Deliver** — the remote is re-checked, then pushed; a draft PR is opened with `Closes #<n>`,
   verification result, model used, and cost; the issue is commented with the PR link;
   `agent-working` is swapped for `agent-done` or `agent-failed`.
7. **Cleanup** — the worktree is removed (kept on disk if `workspace.keep_failed` and the run
   failed, for post-mortem). The claim is always released, on every exit path.

Failures retry up to `run.max_attempts`, starting one rung lower on the `models.json` ladder each
time. A usage limit hit mid-run does **not** consume an attempt.

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
    "max_attempts": 2,
    "timeout": "45m",
    "lease": "90m",
    "verify_timeout": "10m"
  },
  "claude": {
    "binary": "claude",
    "permission_mode": "bypassPermissions",
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
  "models_path": "models.json"
}
```

| Field | Meaning |
|---|---|
| `github.label` | trigger label; **must not be empty**, or every open issue would match |
| `github.working_label` / `done_label` / `failed_label` | status labels the daemon swaps `label` for |
| `github.owners` | users/orgs to search; empty means every repo the `gh` token can see (a boot-time warning fires if left empty) |
| `github.exclude_repos` | `owner/name` repos to never touch, even if labelled |
| `github.search_limit` | max issues fetched per discovery pass |
| `github.poll_interval` | how often discovery runs |
| `workspace.root` | where per-issue worktrees live |
| `workspace.repos_root` | where the one-per-repo checkout-less clones live |
| `workspace.logs_root` | where JSONL run transcripts are written |
| `workspace.keep_failed` | keep a failed run's worktree on disk for inspection instead of deleting it |
| `run.max_concurrent_repos` | how many repos can have work in flight simultaneously |
| `run.max_attempts` | retries per issue before it's marked `agent-failed` and abandoned |
| `run.timeout` | wall-clock limit for one Claude invocation |
| `run.lease` | how long a claim is held before it's considered abandoned; **must exceed `run.timeout`**, or a still-running claim could be stolen |
| `run.verify_timeout` | wall-clock limit for the test command |
| `claude.binary` | executable name/path for the Claude Code CLI |
| `claude.permission_mode` | passed through as `--permission-mode` |
| `claude.usage_poll_interval` / `usage_backoff` | advisory OAuth usage poll cadence and 429 backoff; **must be ≥ 1m** |
| `claude.credentials_path` | where the CLI's OAuth token lives, read for the advisory usage snapshot |
| `verify.auto_detect` | try `Makefile` → `go.mod` → `package.json` → `Cargo.toml` → `pyproject.toml`, in that order |
| `verify.commands` | per-repo override, keyed `"owner/name": "shell command"` |
| `server.addr` | control API bind address; keep loopback-only |
| `store.path` | SQLite database path |
| `models_path` | path to `models.json` |

### models.json

The ladder the loop climbs down when a model is rate-limited or fails. No model identifier is
hardcoded in Go — this file is the only place to update one.

```json
{
  "models": [
    { "id": "claude-opus-5",    "alias": "opus",   "roles": ["implement"], "priority": 1 },
    { "id": "claude-sonnet-5",  "alias": "sonnet", "roles": ["implement"], "priority": 2 },
    { "id": "claude-haiku-4-5", "alias": "haiku",  "roles": ["triage"],    "priority": 1 }
  ]
}
```

- `id` is the canonical model identifier, recorded in the `runs` table so history stays meaningful
  even if an alias's meaning shifts later.
- `alias` is what's passed on the CLI.
- `roles` — currently `implement` (does the work); `triage` is reserved for a future pre-pass.
- `priority` — lower runs first within a role. The head of the priority-ordered list for a role
  becomes `--model`; the rest become `--fallback-model a,b,...`.
- A model that fails or hits a limit is put on a 30-minute cooldown, so the next attempt starts
  lower on the ladder. Retries also start one rung lower than the attempt before them. If every
  candidate is cooled down, the full ladder is used anyway — refusing to run at all is worse; the
  usage gate is the real brake.

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

| Route | Purpose |
|---|---|
| `GET /healthz` | liveness |
| `GET /status` | gate state, in-flight runs, claims, model cooldowns, usage snapshot |
| `GET /runs?limit=&repo=` | recent runs with outcome, model, cost, PR link |
| `GET /runs/{id}` | one run plus its event timeline |
| `GET /runs/{id}/log` | the raw JSONL transcript of the Claude run |
| `POST /pause` `POST /resume` | stop / resume claiming new work |
| `POST /runs/{id}/cancel` | cancel an in-flight run |

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

Run it as a dedicated unix user, not your own — `--install` below does this for you.

## Deploying with systemd

The unit file is embedded in the binary — `internal/install/coding-agent-loop.service` is its only source,
compiled in via `go:embed`. Two ways to use it:

```sh
sudo bin/coding-agent-loop --install --config config.json
```

`--install` (root required):

1. creates the `coding-agent-loop` system user if it doesn't exist (no login shell, its own home);
2. copies the running binary to `/opt/coding-agent-loop/bin/coding-agent-loop`;
3. copies `--config` to `/opt/coding-agent-loop/config.json` — **never** overwriting one already there;
4. writes `/etc/systemd/system/coding-agent-loop.service`;
5. runs `systemctl daemon-reload` then `enable --now coding-agent-loop.service`;
6. confirms the unit reached `active`.

Re-running it is safe: it replaces the binary but leaves an existing config untouched.

```sh
bin/coding-agent-loop --print-service   # print the embedded unit, e.g. to review before installing
```

`--print-service` needs no privileges; it just writes the exact unit `--install` would use to stdout.

The unit itself: `Type=simple`, restarts on failure, `KillSignal=SIGTERM` with
`TimeoutStopSec=50m` (longer than one run timeout, so a SIGTERM drains in-flight work instead of
killing it mid-push), and hardened with `ProtectSystem=strict`, `ProtectHome=read-only`,
`NoNewPrivileges=true`, and `ReadWritePaths` scoped to `/opt/coding-agent-loop` and the service user's
`~/.agent-loop`.

Common operations once installed:

```sh
sudo systemctl status coding-agent-loop      # is it running
journalctl -u coding-agent-loop -f           # tail logs
sudo systemctl stop coding-agent-loop        # drains in-flight work, then stops
sudo systemctl restart coding-agent-loop
curl localhost:8787/status            # gate/run state (from the host, not the service user)
```

## Layout

| Path | Role |
|---|---|
| `cmd/agent.go` | flags, boot checks, wiring |
| `internal/config` | configuration load and validation |
| `internal/models` | models.json, ladder selection |
| `internal/store` | SQLite: claims, runs, gates, events |
| `internal/gh` | GitHub CLI wrapper |
| `internal/git` | clones and worktrees |
| `internal/claude` | headless Claude runs, stream-json parsing |
| `internal/gate` | usage-limit detection and pausing |
| `internal/verify` | detect and run the repo's tests |
| `internal/orchestrator` | the loop, prompts, PR reports |
| `internal/server` | Fiber v3 control API |
| `internal/proc` | process-group isolation |
| `internal/install` | embedded systemd unit, `--install` |

## Tests

```sh
make test    # go test -race ./...
```

External binaries are stubbed with shell scripts on disk, so the whole pipeline is exercised
without network access or subscription usage.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `start-up checks failed: gh is not authenticated` | run `gh auth login` with `repo` + `read:org` scopes |
| `github.owners is empty: discovery will scan every repository this token can see` | expected if `owners` is unset; narrow it in `config.json` if that's not what you want |
| Nothing ever gets picked up | confirm an issue actually carries the exact `github.label` value and is open, and its repo isn't in `exclude_repos` |
| `/status` shows `claiming_work: false` | check `gates` in the response — either a usage limit is active (wait for `blocked_until`) or someone called `POST /pause` |
| A PR opened but tests show as failed | expected behavior, not a bug — verification failures are reported in the draft PR body rather than blocking it |
| `--install` fails with a permissions error | it must run as root (`sudo`); it writes to `/etc/systemd/system` and `/opt` |
| Service won't start after `--install` | `journalctl -u coding-agent-loop -e`; most often a missing/invalid `/opt/coding-agent-loop/config.json` |
