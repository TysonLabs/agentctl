# agentctl

A safe, **read-only** CLI for querying `/agent` observability surfaces — built for AI agents
(Claude Code and friends) that need to ask a running service "what are you doing right now?"
without SSH, database clients, or the ability to break anything.

## What is a /agent surface?

A convention, not a framework: a service mounts a token-gated, **read-only** observability API
at `/agent`. `GET /agent` is a self-describing index of the endpoints it offers:

```json
{
  "endpoints": [
    { "path": "/agent/version", "description": "build SHA and start time" },
    { "path": "/agent/health",  "description": "liveness and dependency checks" },
    { "path": "/agent/queues",  "description": "in-memory queue depths" }
  ]
}
```

Response shapes are **not stable contracts** — the consumers are your own agents, updated
alongside the services. agentctl is therefore deliberately dumb transport: fetch, pretty-print
JSON, exit codes. No per-service schema parsing.

## Why not just curl?

agentctl is what curl looks like after you delete everything an agent could misuse:

- **GET-only by construction.** There is no method flag. The only HTTP verb in the codebase is
  a single `http.MethodGet` literal (enforced by a test that greps the sources).
- **Path confinement.** Requests can only go to paths under `/agent/` on base URLs registered
  in the config file. No `--url` flag exists. Traversal (`..`, encoded variants) is rejected.
- **Token hygiene.** Tokens live in one config file, never on the command line. Internally they
  are wrapped in a `Secret` type whose every formatting path (fmt verbs, JSON) yields a
  fingerprint (`tok:1a2b3c4d`), and all diagnostics are scrubbed before printing.
- **Redirect pinning.** Max 3 hops, same scheme+host+port as the registered base URL, and the
  target path must stay under `/agent`. Anything else is a transport error.
- Sane timeouts (10s default, 8s for `status`), 10 MiB response cap, no prompts, no color.

## Install

```sh
go install github.com/TysonLabs/agentctl@latest
```

## Quick start

1. Write `~/.config/agentctl/services.toml` (chmod 600):

```toml
[payments.dev]
base_url = "https://dev.example.com"
token    = "at_xxxxxxxxxxxx"

[payments.prod]
base_url = "https://pay.example.com"
token    = "REPLACE_ME"          # placeholder → listed as "not wired", never called

[payments.meta]                  # informational only — shown by ls, never fetched
repo = "org/payments"
unit = "payments.service"
```

2. Explore:

```sh
agentctl ls                          # what's registered, what's wired
agentctl endpoints payments.dev      # what the service offers
agentctl get payments.dev version    # any of: version, /version, /agent/version
agentctl get payments.dev "logs?limit=20"
agentctl status                      # /agent/version + /agent/health across all wired services
```

## In practice

agentctl exists so that coding agents can operate a fleet of services with curl on their deny
list. The workflows below are the ones it was built around; the service names and endpoints are
illustrative.

### Symptom triage

An agent is handed "checkouts are slow since noon" and has to come back with evidence, not a
restart. The loop:

1. `agentctl endpoints shop.prod` to see what the service can answer. Trust the live index over
   any runbook; surfaces evolve.
2. Baseline with `version` (did it just restart? does the SHA match the last deploy?) and
   `health` (pools, scheduler last-runs, per-dependency last success/error).
3. Sample in-memory state twice, a minute apart, to tell a growing queue from a stable one.
4. Pull logs filtered server-side: a `since` bounded to the symptom window and `q=` terms taken
   from the symptom. Start narrow, widen only if empty.
5. Report a diagnosis and a recommended action for a human to take. Triage never mutates.

If `/agent` itself is unreachable, exit code 3 is the finding: the process is down or the network
path is broken, and the agent says so instead of guessing.

### Deploy verification

After a push, nothing counts as verified until `agentctl get shop.prod version` reports the
deployed SHA. Then `health` is checked for anything the deploy degraded, and only then does the
agent exercise the changed behaviour. A health payload can also carry "restart owed" style
fields, so a config change the process could not hot-apply shows up here rather than a week later.

### Fleet sweep

`agentctl status` gives one line per wired service and environment, hitting `version` and
`health` with an 8s timeout. Exit 2 means at least one service answered with an HTTP error, exit
3 means at least one was unreachable. A 502 from a reverse proxy in front of a dead process
shows up as `FAIL HTTP 502 on /agent/version` rather than as a hung command.

### Joining across services

Two services that talk to each other in production do not need to talk to each other for
observability. When one records a problem report, it stores only a masked correlation id and
prints the follow-up for the triager:

```
agentctl get upstream.prod "logs?q=<correlation_id>"
```

The human or agent doing triage performs the join by hand through agentctl. The services stay
decoupled, sensitive data stays out of the reporting service's database, and the observability
path never becomes a runtime dependency.

### What a surface tends to grow

Surfaces built for this workflow have converged on roughly the same set, whatever the language:

| Endpoint | Answers |
|---|---|
| `/agent/version` | What build is running, since when, on which host? |
| `/agent/health` | Are dependencies healthy? Pools, schedulers, last success/error per integration. |
| `/agent/state` | What is in memory right now? Queue depths, connection counts, oldest-entry age. |
| `/agent/config` | What is the resolved effective configuration, with every secret redacted? |
| `/agent/logs` | What was logged recently? An in-memory ring, filterable server-side. |
| `/agent/<thing>/<id>` | Detail on one object, addressable only by an id the caller already holds. |

The last row matters: detail endpoints keyed by an unguessable reference (an incident number a
user was shown, a capability id a session owner minted) let a surface expose depth without
letting anyone enumerate users or sessions.

## Configuration

- Location: `--config PATH` > `$AGENTCTL_CONFIG` > `~/.config/agentctl/services.toml`.
- Each `[service.env]` table needs `base_url` (http/https, no userinfo/query/fragment) and `token`.
- A `[service.meta]` table is informational (repo, unit, owner, …) — shown by `ls`, never fetched.
- Placeholder tokens (`REPLACE_ME`, `CHANGEME`, `TODO`, `…`, `<...>`, all-`x`, anything under
  8 chars) mark a service **not wired**: `ls` shows it with the reason, `get`/`endpoints` refuse
  it, `status` skips it.
- Keep the file `chmod 600`; agentctl warns (but proceeds) if group/other bits are set.

## Commands

| Command | Behavior |
|---|---|
| `agentctl ls` | list services/envs, wiring status, base URLs (never token material) |
| `agentctl get <svc.env> <path> [--raw]` | GET under `/agent/`; pretty-print JSON, `--raw` for bytes |
| `agentctl endpoints <svc.env>` | fetch `GET /agent` and render the descriptor table |
| `agentctl logs <svc.env> [log flags]` | `GET /agent/logs` as one readable line per entry (see below) |
| `agentctl status [svc.env ...]` | fan out `/agent/version` + `/agent/health`, one line per service |
| `agentctl version` | print agentctl's own version |

Global flags: `--config PATH`, `--timeout DUR` (default 10s; `status` default 8s), `--help`.

### Exit codes (stable API)

| Code | Meaning |
|---|---|
| 0 | success (`status`: all wired queried services 2xx on both endpoints) |
| 1 | usage error, config error, unknown service.env, not-wired target, rejected path |
| 2 | HTTP status ≥ 400 (body still printed to stdout) |
| 3 | transport: DNS/dial/TLS/timeout, refused redirect, body over cap |
| 4 | `logs --wait`: no matching entry before the deadline |

Output is designed for LLM agents: stdout is the answer only; stderr carries one-line
`agentctl:`-prefixed diagnostics. No color, no spinners, no prompts.

### `agentctl logs`

```sh
agentctl logs recursivecx.prod --level warn --since 30m       # recent warnings, readable
agentctl logs vector-dialer.prod --q "dial-customer" --limit 50
agentctl logs recursivecx.prod --q "E911 link" --wait 20m     # exit 0 when it shows up, 4 if it doesn't
```

It reads the `entries` list from `/agent/logs` and prints one line per entry:
`time LEVEL [source]  message | key=value ...`. The three shapes services emit today
(`ts`/`target`/`message`, `time`/`message`/`fields`, `dt`/`msg`/`fields`) render the same
way, and `--json` prints the normalized entries instead. Newlines are flattened and terminal
controls (C0, DEL, C1, and Unicode bidirectional controls) are dropped, so log content can't
inject escapes or visually reorder an entry. Configured bearer-token values are scrubbed.

`--q` (substring), `--level` (minimum severity) and `--limit` are passed to the service,
which does the filtering. `--since` takes a duration back from now (`30m`) or an RFC 3339
time. `--wait DUR` polls every `--interval` (default 15s) until an entry matches, counting only
entries newer than the start of the wait (or `--since`). Without an explicit `--since`, an
initial read uses the service's HTTP `Date` to account for clock skew; subsequent polls keep
that resolved `since` fixed. It retries transport errors and 5xx, which happen mid-deploy, and
fails fast on any other HTTP error. The wait duration is an overall deadline, including time
spent in requests. It's still only GET requests under `/agent/`.

## Security model

Capabilities that **do not exist**: non-GET methods, arbitrary URLs, custom headers,
`--insecure`, request bodies, tokens on the CLI, config-write commands (a `wire`/`add`
command would put tokens in shell history).

What does exist: bearer auth from a 600-mode file, fingerprint-only token rendering,
output scrubbing, host+path-pinned redirects, timeouts, and a response size cap.

## For agents (CLAUDE.md snippet)

```markdown
## Observability via agentctl
- `agentctl ls` — services you can query; only "wired" ones are callable.
- `agentctl endpoints <svc.env>` — discover what a service exposes.
- `agentctl get <svc.env> <path>` — read-only GET under /agent; pretty JSON on stdout.
- `agentctl status` — quick fleet health; exit 0 = all good, 2 = HTTP errors, 3 = unreachable.
- It cannot mutate anything: GET-only, /agent-only, registered hosts only.
```

## Building a /agent surface in your service

Non-normative conventions that make a surface pleasant to consume:

- Bearer-token auth; the surface only registers when a token is configured.
- Read-only forever; mutations belong elsewhere with their own auth.
- Cheap by construction: in-memory state, O(1) lookups — nothing a caller could use to load you.
- No secrets, no customer PII in responses or logs.
- `GET /agent` returns `{"endpoints":[{"path":...,"description":...}]}` so tools and agents
  can discover everything else. Write each description as the question it answers
  ("What build is running?") — that is what an agent reads when deciding where to look.
- Off by default: an empty token means the route group is never mounted, not "mounted but 401".
- Make index drift impossible: either drive the router from the same table that renders the
  index, or add a test that fails when a registered route lacks a descriptor.
- Sanitize at the point of capture (log ring, event cache) rather than at serve time, so a
  query parameter can never become a search oracle for the raw value. Let opaque ids survive
  masking; they are the join keys triage depends on.
- When a field can legitimately be unknown, return *why* (`"not_configured"`, `"bypassed"`,
  `{"configured": false}`) instead of `null`. A bare null costs someone an hour later.
- Bound every response: default and maximum `limit`, per-entry byte caps, and a total that stays
  well under agentctl's 10 MiB ceiling.

## agentflow (companion binary)

`agentflow` lives in the same repo as a **separate binary** (`cmd/agentflow`), so agentctl
keeps its read-only guarantee and permission allowlists stay per tool. An import-boundary
test keeps the two apart. Each agentflow command does one job and reports a JSON result
and an exit code. It is a set of tools, not a harness: the workflow itself stays in prose,
in [AGENTS.md](AGENTS.md).

### `agentflow codex`: run Codex without hangs or false greens

```sh
agentflow codex --base main                                  # codex's built-in reviewer over main...HEAD
agentflow codex --base main --prompt-file brief.md           # your brief, with the scoped diff inlined
agentflow codex --uncommitted --prompt-file brief.md --path internal/flow   # one area at a time
agentflow codex --prompt-file plan-review.md --dir ~/src/repo                # any read-only task
agentflow codex --base main --prompt-file fix.md --write     # fix mode (workspace-write)
```

What it guarantees, each one a way a hand-typed Codex invocation has failed:

| Failure | What agentflow does |
|---|---|
| Hangs forever on `Reading additional input from stdin...` | stdin is never inherited: it is `/dev/null`, or the prompt file (read to EOF) |
| The review subcommand has no `--sandbox` flag and inherits workspace-write | sandbox always pinned via `-c sandbox_mode=...`; read-only unless `--write` |
| Scope flags can't be combined with a custom prompt (clap error) | a scope plus a prompt inlines the scoped diff (`git diff -M`, untracked files included) |
| Runs for an hour, or wedges silently | `--timeout` (default 40m) and `--stall` (default 10m with no `--json` events *and* no growth of codex's session log) kill the whole process group |
| Exit 0 with no answer read as "clean review" | `ok` requires exit 0, no failed turn, and a non-empty final answer |
| Giant prompts stall in reasoning | prompts over `--max-prompt-bytes` (default 80000) are refused with a hint to split by `--path` |
| `-C` outside a git repo dies on the trust check | `--skip-git-repo-check` is added only when `--dir` is not a git work tree |
| An empty scope "passes" | an empty diff is an error, never a review |

Output: JSON on stdout (also `<out>/result.json`), with `status`, `codex_exit`, `duration_s`,
`thread_id`, `usage`, `error`, and paths to `final.md` (the answer), `prompt.md`,
`events.jsonl`, `stderr.log` and codex's session rollout. Runs take minutes, so agents
should start it in the background and read `final` when it exits.

| Exit | Status | Meaning |
|---|---|---|
| 0 | `ok` | final answer written |
| 1 | — | usage or precondition error (bad flags, empty diff, prompt too large, agent missing) |
| 3 | `codex_failed` / `claude_failed` | the agent exited non-zero or reported a failed turn |
| 4 | `no_answer` | the agent exited 0 without a final answer |
| 5 | `rate_limited` | usage or rate limit: wait, then retry |
| 124 | `timeout` | killed at `--timeout` |
| 125 | `stalled` | killed after `--stall` with no activity |
| 130 | `interrupted` | agentflow was interrupted; the agent was killed |

Set `AGENTFLOW_CODEX` to use a codex binary other than the one on `PATH`.

Every prompt-driven agent is told it is a sub-agent: do the one task, report, and stop,
and don't start other agents or reviews, commit, push or open PRs. Codex's native
`exec review` mode has no prompt file; it uses its built-in reviewer instructions.
Without the note, a prompted reviewer that reads the repo's `AGENTS.md` may try to
start a review of its own fixes.

### `agentflow claude`: the same runner for Claude Code

```sh
agentflow claude --base main                                 # default review brief, diff inlined
agentflow claude --base main --prompt-file brief.md --write  # fix mode
agentflow claude --uncommitted --prompt-file brief.md --max-budget-usd 3
```

It takes the shared flags, guarantees and exit codes of `agentflow codex`, so a change
Codex wrote can be reviewed by Claude. The differences:

- **Sandbox by tool list.** Claude Code has no flag like codex's `sandbox_mode`, and in
  headless mode it inherits your settings' permission mode, which can be
  `bypassPermissions`. agentflow runs it with `--restricted` (no shell or code-running
  tools; user, project and local settings and hooks ignored; file tools confined to
  `--dir`) and `--strict-mcp-config` (no MCP servers). Read-only gets exactly `Read`,
  `Grep` and `Glob`. That's an allowlist, because the default set also has tools that
  create worktrees, message other sessions or upload content. Fix mode (`--write`) adds
  `Edit`, `Write` and `Bash`, with Bash in Claude Code's sandbox: writes only under
  `--dir`, no network, and no unsandboxed escape. (A full `go test` may still fail
  there, because Go's build cache is outside `--dir`; Codex's workspace-write has the
  same limit.)
- **No built-in reviewer.** A scope with no prompt gets a short default review brief,
  with the diff inlined.
- **The answer comes from the stream.** `ok` requires exit 0 and a final `result` event
  with `subtype: "success"` and `is_error: false`; its text becomes `final.md`. An error
  result (`error_max_turns`, a budget stop, an API error) is `claude_failed` (exit 3), or
  `rate_limited` (exit 5).
- **Cost.** The JSON adds `cost_usd`, and `--max-budget-usd` stops a run at that spend.
  The exit code is reported as `claude_exit`, and `rollout` is the session transcript
  under `~/.claude/projects/` (or `$CLAUDE_CONFIG_DIR`).

Set `AGENTFLOW_CLAUDE` to use a claude binary other than the one on `PATH`.

### `agentflow ship verify`: wait until a deploy is really live

```sh
agentflow ship verify recursivecx.prod --sha "$MERGE_SHA"    # safest: wait for that exact build
agentflow ship verify recursivecx.prod --sha "$MERGE_SHA" --contains --repo . # accept a later forward deploy
agentflow ship verify vector-dialer.dev --sha d63a514 --once # one check, no waiting
```

It reads `/agent/version` **through agentctl** (`$AGENTCTL`, then `PATH`, then `~/bin`), so
agentflow never holds service tokens and makes no HTTP calls itself. It replaces the
hand-written `until …; sleep` loops agents write after every merge.

- The commit comes from `git_commit`, `commit`, or `git_sha`, or from a
  `Git Commit: <sha>` line in the `version` build banner. Generic `sha`, `revision`, and
  unrelated string fields are not trusted. Conflicting recognized values fail closed.
- A short SHA matches a full one (prefix either way, at least 7 hex digits), because
  builds usually stamp short SHAs.
- `--sha` takes a hex SHA (compared as given, so it works from any directory, even for
  a commit not fetched yet) or a git rev (`origin/main`, `HEAD`, a tag) resolved in the
  local `--repo` checkout (default: the current directory). Nothing is fetched, so after
  a merge prefer the merge's SHA over a remote-tracking ref that may be stale.
- Exact matching is the default. With an explicit `--repo`, `--contains` also accepts a
  newer deploy that contains the expected commit (`"match": "contains"`). It is
  intentionally opt-in because a pre-rollback build also descends from the older commit
  being restored.
- Transport failures and rollout-like HTTP errors (404, 408, 425, 429, and 5xx) are
  retried until `--timeout` (default 40m, checking every `--interval`, default 30s).
  Other HTTP errors and a version with no recognizable commit fail at once.

| Exit | Status | Meaning |
|---|---|---|
| 0 | `deployed` | running the expected commit, or (with `--contains`) a descendant |
| 1 | `agentctl_error` / — | usage error, or a non-retryable agentctl/HTTP failure |
| 2 | `not_deployed` | `--once` only: not yet |
| 3 | `unreadable` | `/agent/version` has no recognizable commit |
| 124 | `timeout` | never matched before `--timeout`; `running`/`error` show the last state |
| 130 | `interrupted` | interrupted |

### `agentflow worktree done` / `sweep`: remove finished worktrees, never by force

```sh
agentflow worktree done feat/my-change          # by branch, path, or worktree dir name
agentflow worktree done feat/my-change --dry-run
agentflow worktree sweep                        # list every removable worktree
agentflow worktree sweep --yes                  # remove them, prune stale records
```

Agents tend to clean up with `git worktree remove --force` and `git branch -D`. That
works, and it also deletes uncommitted work and unmerged branches without a word.
`done` proves removal is safe first, and refuses with every reason if it isn't:

- **Merged:** the tip is contained in the freshly fetched target (`--into`, default
  `origin/HEAD`), or GitHub shows a PR merged into that target whose branch and head
  are exact matches, so squash and rebase merges count.
- **Clean:** no modified or untracked files. Ignored build output (`target/`,
  `node_modules/`) goes with the worktree, so `--force` is never needed.
- **Unused:** not locked (the lock reason and whether its owner pid is alive are
  shown), no process has its working directory inside (via `lsof`), and no
  registered worktree or other Git repository is nested beneath it. Initialized
  submodules are also refused because Git cannot remove their worktree without
  `--force`.
- **Not** the repository's main working tree.

Then it removes the worktree, deletes the local branch only if it still points at the
proven head, and deletes the remote branch only when a merged PR from that same
repository had exactly that branch and head. Long-lived branches (the target, `main`,
`master`, `develop`, `development`, `staging`, `production`, `release/*`, `hotfix/*`)
are never deleted. `sweep` runs the same checks on every worktree; with `--yes` it
removes those that pass and prunes the records of worktrees whose directories are gone.
`--keep-remote` leaves remote branches alone.

JSON on stdout (per worktree: `ok`, `merged_via`, `refusals`, `keep_branch`, and after
removal `freed_bytes`, `branch_deleted`, `remote_deleted`). Sweep continues after an
individual worktree error, records it in that entry's `error`, prints the complete
JSON result, and exits 3. Exit codes: 0 removed (or would be, or sweep finished) ·
1 usage · 2 refused · 3 git/gh error. A missing or incomplete `lsof` check is a
safety refusal.

## Non-goals

Color/TTY niceties, retries, response caching, keychain integration, `--json` listing output,
shell completions, config-write commands, per-service schema rendering. PRs adding request
capabilities beyond GET-under-/agent will be declined on principle.

## Contributing

`make all` runs vet, race-enabled tests, and the build. The test suite includes a source guard
that fails if any mutating HTTP verb appears in non-test code — keep it that way.

## License

MIT © Tyson George
