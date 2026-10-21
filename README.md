<p align="center"><img src="assets/logo_web.png" alt="Agent Tazzyronth" width="160"></p>

<h1 align="center">Agent Tazzyronth</h1>

Tazzyronth (`tazzy`) is a lightweight agent-swarm coordinator. You give your agent CLI a goal and a cap on workers; a lead agent breaks it into a task graph, and `tazzy` starts only as many workers as the work can actually use, keeps them off each other's files, passes messages between them, unblocks waiters, requeues failed or stalled work with its checkpoints, and shows every tool call and live diff in a dashboard. It is one Go binary with no runtime dependencies, and it speaks MCP, so it works from Claude Code, Codex, Gemini CLI, Antigravity and Cursor. The hub is deterministic: all reasoning is done by the agents.

## Features

- **Elastic swarm**: `max_agents` is a ceiling. In hub spawn mode the hub launches headless workers (`claude -p`, `codex exec`, `gemini -p`, `agy -p`) to match the parallel width of the task graph and retires idle ones.
- **Coordination**: shared task DAG with deps and caps, message bus (`agent:`, `role:`, `task:`, `topic:`, `broadcast`), shared board and decision log, and file leases (globs, read/write, TTL).
- **Clean finish**: once every task has ended (and no follow-up was proposed within `finish_grace_s`), the hub releases all workers, host subagents included; `tazzy_start` also releases idle workers left over from an earlier swarm, so they never count against the next `max_agents`.
- **Pause and stop**: pause the swarm (no new tasks handed out, nothing spawned; running tasks finish), resume it, or stop it (every open task cancelled, hub-spawned workers retired, all agents told to exit) from the lead (`tazzy_control`) or the dashboards.
- **Self-healing**: stalled or dead agents are detected, their leases released and their task requeued with checkpoints. Retries, escalation to the lead, and failure cascade to dependents.
- **Isolation**: `lease` (shared checkout, default) or `worktree` (one git worktree per agent; finished work is merged into a `tazzy/integration-*` branch, never into your checkout; conflicts become resolve tasks).
- **Observability**: per-agent activity (tool calls, commands, edits with live diffs, intent, token use and cost where the CLI reports it; host-mode Claude Code sessions and their subagents are counted from their transcripts), unleased-edit alerts, terminal dashboard, an in-Claude-Code pane, and a local web view.
- **Featherweight**: stdlib plus `golang.org/x/sys` and `golang.org/x/term`; release binaries are 7-8 MB.

## Install

The module path is `tazzyronth` until the repository has a public remote, so `go install <url>@latest` does not work yet. Use one of:

```sh
# From a clone (puts tazzy in $(go env GOPATH)/bin; make sure that is on PATH)
go install ./cmd/tazzy

# Or build release archives for every platform into dist/
go run ./tools/release -version 0.1.0
```

Prebuilt archives (`tazzy_<version>_<os>_<arch>.zip|.tar.gz` plus `SHA256SUMS`) will be published once there is a release page; unpack and put `tazzy` on your PATH. Requires Go 1.26 to build.

Then wire it into your agent hosts and check the setup:

```sh
tazzy init          # detects installed hosts, shows every change, asks y/N per host (-dry-run, -y, -host codex,cursor)
tazzy doctor        # hub, PATH, adapter CLIs, git, terminal, Claude Code plugin
```

`tazzy` must be on PATH: host configs and hooks start `tazzy mcp` and `tazzy hook report` by name (init writes the absolute path where a config allows it). Add `.tazzyronth/` to your project's `.gitignore`; it holds the event log, activity, logs and worktrees.

## Quick start

| Host | Setup (`tazzy init` does this) | Start a swarm |
|---|---|---|
| **Claude Code** | Plugin written to `~/.tazzyronth/claude-code/`. Install once: `claude plugin marketplace add ~/.tazzyronth/claude-code` then `claude plugin install tazzyronth@tazzyronth` (or `/plugin` inside Claude Code). Try without installing: `claude --plugin-dir integrations/claude-code` | `/tazzy <goal> [--max N]` (full name `/tazzyronth:tazzy`) |
| **Codex** | `[mcp_servers.tazzyronth]` in `~/.codex/config.toml` (or `$CODEX_HOME`), skill `~/.agents/skills/tazzy/SKILL.md` | `$tazzy <goal>` (or pick it from `/skills`) |
| **Gemini CLI** | `mcpServers` in `~/.gemini/settings.json`, command `~/.gemini/commands/tazzy.toml` | `/tazzy <goal>` |
| **Antigravity** (CLI + IDE) | `~/.gemini/config/mcp_config.json`, skills in `~/.gemini/antigravity-cli/skills/tazzy/` and `~/.gemini/config/skills/tazzy/`, and the `mcp(tazzyronth/*)` allow rule in `~/.gemini/antigravity-cli/settings.json` (headless `agy` workers need it) | `/tazzy <goal>` |
| **Cursor** | `~/.cursor/mcp.json`, skill `~/.agents/skills/tazzy/SKILL.md` (shared with Codex) | `/tazzy <goal>` |
| **Any MCP client** | Register the stdio server `tazzy mcp` (add `-dir <project>` to pin the project); manual snippets are in `integrations/` | Use the MCP prompt `tazzy` (arg `goal`), or call the `tazzy_start` tool |

The lead calls `tazzy_start`, gets a playbook back, plans tasks with `tazzy_plan`, and either starts workers itself (spawn mode `host`: Claude Code subagents call `tazzy_join`) or lets the hub spawn headless CLIs (spawn mode `hub`). The default `auto` picks `host` for Claude Code and `hub` for every other client.

There is no headless `tazzy run <goal>` yet; a swarm is always led from an agent host.

## Dashboards

- **Terminal**: `tazzy tui [-dir DIR] [-no-splash]`. `tazzy_start` opens it next to you (tmux / WezTerm / kitty / Windows Terminal split, else a new OS terminal window; config `tui.open`). It waits for the hub, so it can be opened any time.
  - Overview: `↑↓` select, `enter` agent detail, `k` retire (y/N), `m` messages.
  - Swarm: `p` pause/resume, `x` stop (y/N). The header shows a `⏸ PAUSED` or `■ STOPPED` badge.
  - Anywhere: `a` swarm activity, `F` files, `g` task graph, `b` board, `u` usage, `esc` back, `q`/Ctrl+C quit, `↑↓ PgUp PgDn Home End` scroll.
  - Agent detail: `↑↓` move (stops follow), `f` follow, `enter` expand event, `o` latest command output, `d` cycle diff file, `←→` previous/next agent, `m` message the agent (`tab` toggles directive/info), `k` retire.
  - `-frame 120x40 [-agent ID]` prints one frame without a terminal.
- **Inside Claude Code** (plugin): a status line and a band above the prompt while a swarm runs, toasts for failures/reassignments/deaths, and a `tazzyronth` pane with the agent table, the selected agent's feed and latest diff; paused/stopped shows in the band and status line, with a toast on each change. `/tazzy-pane` reopens it. Built on `tazzy watch --json`.
- **Web view**: read-only page on `127.0.0.1` (shows a PAUSED/STOPPED badge). Opens automatically from IDE terminals (VS Code, Cursor, Antigravity) or with `tui.open: "web"`; also `tazzy hub --web` or config `"web": true`. The URL (with token) is in `tazzy_start`'s `dashboard` field, or build it from `.tazzyronth/hub.json`: `http://127.0.0.1:<web>/?token=<token>`.
- **Plain**: `tazzy status [-dir DIR]` (summary), `tazzy status --line` (one line for status bars; ends in `paused`/`stopped` when not running), `tazzy watch [--json]` (event stream, NDJSON with `--json`).

## Commands

| Command | Purpose |
|---|---|
| `tazzy hub [-dir DIR] [-web]` | Run the hub in the foreground (normally auto-started by `tazzy mcp`). |
| `tazzy mcp [-dir DIR] [-agent ID]` | stdio MCP server that hosts launch. `-dir` defaults to `$TAZZY_PROJECT`, else the cwd; `-agent` to `$TAZZY_AGENT_ID` (set for hub-spawned workers). |
| `tazzy tui`, `status`, `watch` | Dashboards, see above. |
| `tazzy init [-y] [-dry-run] [-host LIST]` | Install host integrations. |
| `tazzy hook report [-dir DIR] [-agent ID] [-enforce]` | Host hook entry point (activity reporting; `-enforce` denies edits to files another agent write-leases). |
| `tazzy doctor [-dir DIR]` | Environment check; exits 1 on warnings. No network or LLM calls. |
| `tazzy version` | Print the version. |

MCP tools: `tazzy_start`, `tazzy_plan`, `tazzy_status`, `tazzy_control` (`cancel`, `spawn`, `retire`, `pause`, `resume`, `stop`), `tazzy_join`, `tazzy_next`, `tazzy_progress`, `tazzy_done`, `tazzy_fail`, `tazzy_propose`, `tazzy_lease`, `tazzy_send`, `tazzy_wait`, `tazzy_board`. MCP prompts: `tazzy`, `tazzy-worker`.

**Pause / resume / stop** (`tazzy_control` with no target, TUI `p`/`x`, hub RPC `swarm.control`): *paused* means `tazzy_next` hands out nothing (it returns `state: "paused"` and workers wait for a message) and the autoscaler spawns nothing, while claimed tasks keep running; *resume* goes back to running. *Stop* is final for the swarm: every task that hasn't ended is cancelled (claimed ones too), hub-spawned workers are retired, every agent gets a directive to stop and exit, and the lead is told; resume is refused, and the next `tazzy_start` runs again. Each change is broadcast to all agents, and `tazzy_status`/the snapshot carry `state` when it isn't running.

## Configuration

Optional, per project: `.tazzyronth/config.json`. Missing fields keep their defaults; `tazzy_start` arguments (`spawn`, `max_agents`, `adapter`, `isolation`) override per swarm.

```jsonc
{
  "max_agents": 5,              // hub-spawned workers alive at once
  "min_agents": 0,
  "default_adapter": "",        // "" = match the lead's host (codex/antigravity/gemini/claude), else claude
  "spawn_mode": "auto",         // auto | hub | host
  "isolation": "lease",         // lease | worktree (git 2.38+, project = repo top level with a commit)
  "timeouts": {
    "idle_retire_s": 90, "spawn_cooldown_s": 5, "scale_tick_s": 2,
    "hub_idle_s": 30,           // hub exits (killing its workers) after this long with no host connected
    "stall_s": 300, "stall_grace_s": 60,
    "finish_grace_s": 30,       // once every task has ended this long, all workers are released
    "quick_exit_s": 60          // a hub-spawned worker that dies within this long of its spawn died quickly
  },
  "budget":   { "max_total_spawns": 20,
                "max_quick_exits": 3 },  // quick deaths in a row before the hub pauses the swarm; 0 = never
  "retries":  { "max_attempts": 3, "escalate_after": 2 },
  "scheduler": { "weights": { } },   // see internal/sched (sched.Defaults)
  "activity": { "ring": 500, "poll_s": 1, "sweep_s": 5 },
  "tui":      { "open": "auto" },    // auto | split | tab | window | web | none
  "web": false,
  "adapters": {                      // merged by name over the built-ins claude, codex, gemini, antigravity
    "mycli": { "cmd": ["mycli", "--prompt-file", "{prompt_file}", "--mcp", "{mcp_config}"],
               "env": { "MYCLI_AGENT": "{agent_id}" }, "stream": "",
               "note": "" }          // extra advice appended to the worker prompt after the playbook
  }
}
```

Adapter placeholders: `{prompt}`, `{prompt_file}`, `{mcp_config}`, `{agent_id}`, `{workdir}`, `{project}`, `{tazzy}`. `stream` names the stdout decoder (`claude`, `codex`, `gemini`, `antigravity`, or empty). `note` is optional text appended to the worker's bootstrap prompt after the playbook, for advice specific to that CLI; the built-in `antigravity` adapter uses it to tell workers to stick to file tools. Overriding a built-in replaces it wholesale, so keep `"stream"` (and `"note"`) if you redefine one. (The file is plain JSON; the comments above are for illustration.)

**Crash-loop guard.** If hub-spawned workers keep dying right after they start (for example every `agy` worker failing at once with a 429 `RESOURCE_EXHAUSTED` quota error), the hub stops respawning them instead of burning through `max_total_spawns`. A worker that exits within `quick_exit_s` of its spawn is a quick death; the spawn cooldown doubles with each one in a row (capped at 60s). After `max_quick_exits` in a row the hub pauses the swarm and tells the lead once: `workers keep dying right after start (<n> in a row; last: <reason>): swarm paused. Fix the cause, then tazzy_control resume.` The count resets when a worker survives past `quick_exit_s`, a task is done, or the swarm is resumed.

**Project guard.** Without `-dir` or `$TAZZY_PROJECT`, `tazzy mcp` asks the client for its workspace root and otherwise falls back to its working directory. That fallback is used only if it is inside a git work tree or already has a `.tazzyronth/` folder; anything else (such as an app's install folder) is refused, no hub is started, and every tool call returns `no project folder: the client reported no workspace root and <dir> is not a project ...`. To fix it, open the project folder in your editor first (in the Antigravity app, open a folder before running `/tazzy`), or pin the project with `"-dir", "<project>"` in the tazzyronth MCP server's args.

## How it works

```
                 ┌─────────────────────── tazzy hub ───────────────────────┐
                 │ task graph · scheduler · leases · bus · board · monitor │
                 │ supervisor · activity + diffs · .tazzyronth/events.jsonl│
                 └──────▲──────────────▲──────────────▲─────────────▲──────┘
            NDJSON/TCP  │ 127.0.0.1    │              │             │ events
                 ┌──────┴──────┐ ┌─────┴──────┐ ┌─────┴──────┐ ┌────┴─────────┐
                 │ tazzy mcp   │ │ tazzy mcp  │ │ tazzy mcp  │ │ tui / web /  │
                 └──────▲──────┘ └─────▲──────┘ └─────▲──────┘ │ watch --json │
                  stdio │ MCP          │              │        └──────────────┘
                 ┌──────┴──────┐ ┌─────┴──────┐ ┌─────┴──────┐
                 │ lead (your  │ │ worker     │ │ worker     │
                 │ agent CLI)  │ │ (subagent  │ │ (codex     │
                 │             │ │ or claude) │ │  exec ...) │
                 └─────────────┘ └────────────┘ └────────────┘
```

Every host starts its own `tazzy mcp` shim; the first one starts a hub for the project (`.tazzyronth/hub.json` holds its loopback port and auth token). All state lives in the hub and is an append-only event log, replayed on restart. Workers pull tasks (`tazzy_next`, scored by priority, critical path, caps, file affinity and lease conflicts), lease files before editing, checkpoint with `tazzy_progress`, and block in `tazzy_wait` instead of polling. Messages for an agent ride along with its next tool result. Shims exit with their host, and the hub exits `hub_idle_s` after the last host disconnects (closing the editor or ending the session), killing any workers it spawned, so nothing keeps running once you stop using it.

## Limitations and unverified parts

- **Codex and Gemini adapters and their stream decoders are unverified** (neither CLI was available during development). The Claude adapter matches `claude --help`; its decoder was written from the stream-json format, not checked against a live run. No end-to-end run with a real LLM swarm has been recorded yet.
- **Antigravity**: checked against `agy` 1.4.2 (its flags, the stream-json output the `antigravity` decoder reads, MCP client name `antigravity-client`, and `tazzy mcp` serving its tools under `agy`), and in one live swarm (an `agy` lead and hub-spawned `agy` workers). `agy` has no per-run MCP config flag, so its workers rely on the global entry and allow rule that `tazzy init` writes (`tazzy doctor` checks both). **Headless `agy` ends the whole run on the first denied action**: any shell command without a matching `command(...)` allow rule is refused, the run stops at once and the worker's unfinished work is lost (agy still reports `SUCCESS`). The `antigravity` adapter's `note` tells workers to inspect and edit files only with their file tools and to run a shell command only when the task names it. If a hub-spawned worker is denied anyway, the lead gets one message per worker naming the denied command and the rule to add. Add `command(...)` allow rules to `permissions.allow` in `~/.gemini/antigravity-cli/settings.json` for the commands your tasks need, e.g. `command(go build)`, `command(go test)`, `command(go vet)`, and read-only ones such as `command(Get-ChildItem)`, `command(Test-Path)`, `command(Get-Content)`. Antigravity starts MCP servers outside the project directory, so a `/tazzy` lead in Antigravity may pick the wrong project: open a folder in the app before `/tazzy`, or pin it with `-dir` in the MCP entry (without either, the project guard refuses a working directory that is not a project).
- The other non-Claude host integrations (Codex, Gemini CLI, Cursor) were written from the hosts' docs and not tried live. How a skill receives the text after `/tazzy` is unverified, and so is Cursor's MCP client name (its workers fall back to the `claude` adapter). Gemini CLI was retired for free accounts in June 2026.
- Token counts for host-mode Claude Code sessions come from their transcripts (`transcript_path`), read when hooks fire (after tool calls, at subagent stop and at the end of a turn), so they can lag by up to one turn. A lead's main transcript is counted from when the hub first sees it (its `tazzy_start`), not its earlier conversation history; a subagent's transcript is counted in full; after a hub restart, reading resumes where it left off.
- The Claude Code pane/band were checked with `claude plugin test`, not in a live session. The plugin's sync `PreToolUse` hook starts a `tazzy` process per tool call.
- Not implemented: headless `tazzy run`, `replay`, `tazzy_control` reprioritize, `tui.open: "inline"`, web view actions (read-only), budget enforcement from token usage, retry backoff, multi-machine hubs.
- Worktree mode needs the project to be the top level of a git repo with at least one commit; failed attempts are kept on `tazzy/<task>-failed-N` branches. Leftover branches are expected.

See [`docs/PROGRESS.md`](docs/PROGRESS.md) for per-phase details and deviations from [`docs/IMPLEMENTATION_PLAN.md`](docs/IMPLEMENTATION_PLAN.md).

## Development

```sh
go build ./... && go vet ./... && go test -count=1 ./...
go generate ./assets      # regenerate the logo files from assets/logo.jfif
```

Tests never start real LLM CLIs (the supervisor refuses `claude`/`codex`/`gemini`/`agy` under `go test`); swarms are simulated with `test/sim` and `test/fakeagent`.

## License

No license has been chosen yet.
