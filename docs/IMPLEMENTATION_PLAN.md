# Agent Tazzyronth — Implementation Plan

> **Status (2026-10-17):** v0.1 is implemented (Phases 0–10). This plan is kept as the original design; what was actually built, and every deviation from it, is in [`PROGRESS.md`](PROGRESS.md). User docs: [`README.md`](../README.md).

> An agent-swarm coordinator. You give it a goal and a cap on how many agents it may use. It starts only as many agents as the work needs, keeps them out of each other's way, sends messages between them, unblocks waiting agents, reassigns failed work, and shows all of it live in a terminal dashboard.
> It ships as **one small binary** with **no runtime dependencies** and speaks **MCP**, so it works with Claude Code, Codex, Gemini CLI, Antigravity, Cursor, and other MCP clients.

---

## 1. Goals and non-goals

### Goals
| # | Goal | What counts as done |
|---|------|------------------|
| G1 | **Elastic swarm** | `max_agents` is a ceiling, not a target. The number of agents tracks how much work can actually run in parallel. |
| G2 | **Real coordination** | Agents share a task graph, a message bus, a shared board, and file leases. They negotiate instead of colliding. |
| G3 | **Self-healing** | A crashed, stuck, or silent agent is detected. Its task goes back in the queue with its partial progress attached, and another agent picks it up. |
| G4 | **Unblocking** | An agent waiting on a dependency or a message resumes as soon as that dependency is done or the message arrives. |
| G5 | **Adaptive** | Agents can add subtasks, report blockers, and trigger a re-plan while the swarm is running. |
| G6 | **Observable** | You can watch what every agent is **actually doing**: each tool call, file read, shell command, and edit (with a live diff), plus what it says it intends, its messages, and its token use. The dashboard opens **from inside your agent CLI** (`/tazzy`), either in a split pane, a new terminal, or inside Claude Code itself. The logo appears in the header. |
| G7 | **Featherweight** | One static binary under 10 MB, under 20 MB RAM when idle, zero runtime dependencies, starts in under 50 ms. |
| G8 | **Host-agnostic** | MCP is the primary interface. Adapters let it launch workers through Claude Code, Codex, Gemini, or any CLI. |

### Non-goals (v1)
- Being an LLM itself. The hub is **deterministic**: all reasoning (decomposing work, writing code) is done by the agents.
- Multiple machines or a cloud cluster. v1 is local only, but the protocol is designed so this can be added later.
- A web UI. Terminal first. A web view could be added later because the hub already streams events.

---

## 2. Key architectural decisions

### 2.1 Language: **Go** (decided)
| Option | Pros | Cons |
|--------|------|------|
| **Go** ✅ | Single static binary, cross-compiles to Win/Mac/Linux, goroutines are a good fit for many concurrent agents, the stdlib covers JSON/networking/processes, low RAM | Not installable via `npx` (solved by shipping an npm wrapper later) |
| Node/TS | `npx` install, large MCP ecosystem | Needs the Node runtime, ~50 MB RSS, `node_modules` |
| Rust | Smallest and fastest | Slower to build, harder for contributors |
| Python | Fast to prototype | Needs the runtime, slow startup, packaging problems |

**Dependency policy:** Go stdlib plus `golang.org/x/term` and `golang.org/x/sys` (official Go sub-repos, used for raw terminal mode and Windows Job Objects). Nothing else. MCP (JSON-RPC 2.0 over stdio) and the TUI renderer are written by hand. They are small, well-specified pieces.

### 2.2 Topology: hub + thin shims
MCP stdio servers are started **once per client process**. Each agent (Claude Code, a Codex worker, and so on) therefore gets its own MCP process. Those processes need one shared brain, so there is a single **hub** process:

```
                ┌───────────────────────────── tazzy hub (daemon) ─────────────────────────────┐
                │  Task Graph (DAG)   Scheduler   Lease Manager   Message Bus   Board   Monitor │
                │  Supervisor (spawns/kills workers)    Event Log (.tazzyronth/events.jsonl)    │
                └──────────────▲───────────────▲───────────────▲───────────────▲────────────────┘
                   NDJSON/TCP  │ 127.0.0.1     │               │               │  event stream
                ┌──────────────┴──┐  ┌─────────┴───────┐ ┌─────┴──────────┐ ┌──┴──────────┐
                │ tazzy mcp (shim)│  │ tazzy mcp (shim)│ │tazzy mcp (shim)│ │  tazzy tui  │
                └──────▲──────────┘  └────────▲────────┘ └──────▲─────────┘ └─────────────┘
                 stdio │ MCP           stdio  │ MCP        stdio │ MCP
                ┌──────┴──────┐       ┌───────┴──────┐    ┌──────┴──────┐
                │ LEAD agent  │       │  worker #1   │    │  worker #2  │
                │ (your Claude│       │ (claude -p / │    │ (codex exec)│
                │  Code chat) │       │  subagent)   │    │             │
                └─────────────┘       └──────────────┘    └─────────────┘
```

- **`tazzy mcp`** is the stdio MCP server that hosts launch. If no hub is running it **starts one automatically**, then forwards calls to it. It holds no state.
- **`tazzy hub`** owns all state. It listens on `127.0.0.1:<port>`. The port and a random auth token are written to `.tazzyronth/hub.json`.
- **`tazzy tui`** connects to the hub's event stream and renders the dashboard. You can open it in a second terminal at any time.

### 2.3 Wire protocol (inter-agent communication)
- **NDJSON-framed JSON-RPC 2.0 over loopback TCP.** It is the same message shape as MCP, so one codec handles both. Latency is sub-millisecond, there is no broker, and it works the same on Windows, where Unix sockets are awkward.
- Every connection authenticates with the token from `hub.json` (it is a local file, mode 0600).
- Messages carry a monotonically increasing `seq`, and the hub acks each one. Delivery to an agent's inbox is **at-least-once**, with dedup by `msg_id`.

### 2.4 How agents get spawned (two modes, both supported)
LLM hosts cannot be forced to start subagents from the outside, so Tazzyronth supports two complementary modes:

| Mode | How it works | Best for |
|------|--------------|----------|
| **A. Hub-spawned (headless CLI)** | The hub's Supervisor launches worker processes such as `claude -p …`, `codex exec …`, or `gemini -p …`. Each one gets an MCP config pointing at `tazzy mcp` and a bootstrap prompt. The hub sees the PID and exit code directly. | True elastic scaling, crash detection, any host |
| **B. Host-spawned (native subagents)** | The lead agent uses its host's own subagent feature (Claude Code's Agent/Task tool, for example). Each subagent calls `tazzy_join` and from then on behaves exactly like a mode-A worker. Health is tracked by heartbeat only. | Running inside one Claude Code session with no extra processes |

Worker launchers are **adapter templates** in config, so adding a new CLI requires no code change:
```json
"adapters": {
  "claude": { "cmd": ["claude", "-p", "{prompt}", "--mcp-config", "{mcp_config}", "--permission-mode", "acceptEdits"] },
  "codex":  { "cmd": ["codex", "exec", "{prompt}"], "env": { "CODEX_MCP": "{mcp_config}" } },
  "gemini": { "cmd": ["gemini", "-p", "{prompt}"] },
  "custom": { "cmd": ["my-agent", "--task-file", "{task_file}"] }
}
```
*(The exact flags for each CLI are verified in Phase 4. These are placeholders.)*

### 2.5 Getting messages to an LLM that only acts when it calls a tool
An LLM agent cannot be interrupted, so Tazzyronth uses three mechanisms together:
1. **Piggyback:** every tool response includes an `inbox` block (unread messages, lease changes, cancellations, newly unblocked deps). An agent that is doing anything coordinated sees new events on its next call.
2. **Blocking wait:** `tazzy_wait(until=…, timeout≤45s)` long-polls until a dependency finishes, a message arrives, or a lease frees up. This is how a waiting agent resumes "directly after needed" (G4). The timeout stays under typical MCP tool timeouts, and the bootstrap prompt tells agents to loop on it.
3. **Cancel signal:** for mode-A workers, the Supervisor can kill the process outright. In mode B, the next tool response returns `{"directive":"stop"}`.

### 2.6 Persistence
- **Append-only event log** at `.tazzyronth/events.jsonl` is the source of truth. State is rebuilt by replaying it, so the hub can crash and restart with no work lost.
- A periodic snapshot (`state.json`) keeps replay fast. There is no database.
- The log doubles as an audit trail and a replay file for debugging (`tazzy replay`).

---

## 3. Core subsystems

### 3.1 Task Graph
```go
type Task struct {
    ID, Title, Spec   string
    Deps              []string          // DAG edges
    Caps              []string          // required capabilities: "go", "frontend", "tests"
    Files             []string          // declared/expected paths or globs (for lease pre-planning)
    Priority          int
    State             TaskState         // pending|ready|claimed|running|blocked|review|done|failed|cancelled
    Owner             string            // agent id
    Attempts, MaxAttempts int
    Checkpoints       []Note            // progress notes; handed to the next owner on reassignment
    Result            *Result           // summary, changed files, artifacts
    ParentID          string            // for dynamically proposed subtasks
}
```
- Cycle detection runs on every insert.
- A task becomes `ready` automatically when all its deps are `done`, and every agent waiting on it is woken.
- **Contracts and artifacts:** a task can publish named artifacts (for example, `api-schema`) that dependent tasks read from the Board. This lets agents build against interfaces that land before the implementations.

### 3.2 Scheduler (lightweight and deterministic)
A priority queue of ready tasks. Each `(task, agent)` pair is scored in O(ready × idle), which is tiny in practice:

```
score = w1·priority
      + w2·criticalPath(task)      // longest chain of downstream dependents → do bottlenecks first
      + w3·capMatch(task, agent)   // capability fit; hard filter if required caps missing
      + w4·fileAffinity(task, agent)  // agent already holds/edited these files → fewer conflicts, warm context
      − w5·leaseConflict(task)     // declared files currently leased by someone else
      − w6·agentLoad(agent)
      − w7·attempts(task)          // de-prioritize flaky tasks slightly; avoid re-giving to agent that failed it
```
Weights live in config with sensible defaults. Assignment is **pull-based** (`tazzy_next` claims the best task for the caller), and the hub can also **push** a suggestion to an idle agent's inbox.

### 3.3 Elastic scaling (G1)
Every scheduler tick (default 2 s):
```
parallel_width = number of ready tasks whose declared files don't conflict with each other
desired        = clamp(min_agents, parallel_width + small_buffer, max_agents)
if desired > live  and cooldown elapsed → spawn (desired − live), one at a time
if agent idle > idle_timeout and live > desired → retire it (graceful: finish current call, then stop)
```
- Hysteresis and cooldowns prevent thrashing.
- A **budget guard** (`max_agents`, optional `max_total_spawns` and `max_runtime`) bounds cost.

### 3.4 Lease Manager (non-interference)
- Agents call `tazzy_lease(paths, mode=write|read, ttl)` before editing. Globs are supported (`src/api/**`).
- Write leases are exclusive. Read leases are shared. Conflicting requests either queue (the agent can `tazzy_wait(until="lease:<path>")`) or fail fast with the name of the holder, so the two agents can negotiate.
- Leases have a TTL, renewed by any tool call (an implicit heartbeat), and are auto-released when a task completes or an agent dies.
- **Optional `isolation: "worktree"` mode:** each worker gets its own `git worktree` and branch. The hub (or a dedicated *integrator* agent task) merges completed branches back in dependency order, and merge conflicts become new tasks. Leases are still used to *reduce* conflicts, but worktrees make collisions impossible.
- **Optional enforcement:** a Claude Code `PreToolUse` hook (`tazzy hook check-lease`) blocks `Edit`/`Write` calls on files the agent doesn't hold. Hosts without hooks rely on the prompt contract.

### 3.5 Message Bus
- Address types: `agent:<id>`, `role:<name>` (for example, `role:lead`), `task:<id>` (the task's current owner, whoever that is), `topic:<name>`, `broadcast`.
- Message kinds: `info`, `question` (expects a reply, so it carries `reply_to`), `handoff`, `blocker`, `directive`.
- Each agent has a bounded inbox. Messages addressed to `task:<id>` follow the task when it is reassigned.

### 3.6 Board (shared memory / blackboard)
- A namespaced KV store plus an append-only **decision log** ("we chose JWT, not sessions — agent w2").
- This is where agents publish interfaces, conventions, and discoveries so every new agent starts with the shared context. It is included in the bootstrap prompt as a short summary.

### 3.7 Monitor and failure handling (G3)
| Signal | Detection | Action |
|--------|-----------|--------|
| Process exit ≠ 0 (mode A) | Supervisor wait() | Mark the agent dead, release its leases, requeue the task with checkpoints |
| No heartbeat for `stall_timeout` (default 5 min, configurable) | Monitor tick | Ping via inbox → after a grace period, treat as dead (mode A: kill the process tree via a Job Object / process group) |
| `tazzy_fail(retryable=true)` | Explicit | Requeue, increment attempts, prefer a different agent |
| `tazzy_fail(retryable=false)` or attempts exhausted | Explicit/limit | Mark the task `failed`, notify `role:lead`, which can re-plan |
| Same task fails on N different agents | Pattern | Escalate to the lead with all failure notes |

On reassignment, the new owner receives the original spec, **all checkpoints**, the files changed so far, and the previous failure reason. Work resumes from where it stopped instead of starting over.

### 3.8 Supervisor (process management)
- Spawns workers through the adapters, captures stdout and stderr to `.tazzyronth/logs/<agent>.log`, and tails the last lines for the TUI.
- Windows: a **Job Object**, so killing the hub kills every worker and no orphans are left. Unix: process groups.
- Writes a per-worker temporary MCP config and bootstrap prompt into `.tazzyronth/run/<agent>/`.

### 3.9 Activity capture ("what is each agent doing right now?")
Coordination tool calls alone only say *which task* an agent has. To show *what it is doing*, the hub combines four sources into one normalized **activity stream**:

| # | Source | Applies to | What it yields |
|---|--------|-----------|----------------|
| 1 | **Structured output streams** of headless workers: `claude -p --output-format stream-json --verbose`, `codex exec --json`, `gemini --output-format stream-json` | Mode A (hub-spawned) | Every tool call with its arguments (file paths, old/new strings, shell commands), tool results (pass/fail, test output), the agent's visible text, and token/cost usage. Parsed by small per-adapter decoders in `internal/activity/decode_*.go` |
| 2 | **Host hooks** that report each tool call to the hub: Claude Code `PreToolUse` / `PostToolUse` / `SubagentStart` / `SubagentStop` → `tazzy hook report` (reads the hook JSON on stdin and forwards it), or the Claude Code mod's `tool.call` hook (§7.3), which sees calls from every subagent tagged with its `agentId` | Mode B (host subagents) | The same tool-level detail for native subagents, which have no stdout the hub can read |
| 3 | **File watcher + diff engine** in the hub. It snapshots each file when it is leased, then polls the mtimes of leased paths (cheap, since it is only the leased set), plus a slower `.gitignore`-aware sweep of the rest of the repo. A hand-rolled Myers diff produces a live unified diff | All modes | Live `+/−` diffs per file and per agent, even for hosts with no hooks or streams. An edit to a file **nobody leased** raises an `⚠ unattributed edit` alert, which catches protocol violations |
| 4 | **Self-reported intent** from `tazzy_progress(note=…)`, for example "now: writing refresh-token rotation" | All modes | A one-line **NOW** headline per agent, in the agent's own words |

**Identity binding (mode B):** when a subagent calls `tazzy_join`, that call itself passes through the host hook carrying the host's `agentId` / session id. The hub links the two ids automatically, so all later hook events are attributed to the right Tazzyronth agent.

**Normalized event:**
```go
type Activity struct {
    Agent, Task string
    TS          time.Time
    Kind        string   // intent | say | read | edit | cmd | cmd_result | search | msg | lease | state | usage | alert
    Summary     string   // one line for the feed: "Edit src/auth/refresh.go (+24 −3)"
    Files       []string
    Detail      json.RawMessage // full args/diff/output, fetched on demand by the detail view
}
```
- Kept in memory as a **ring buffer per agent** (default 500 events) for instant TUI rendering, and persisted to `.tazzyronth/activity/<agent>.jsonl` for replay and post-mortems.
- Large payloads (full diffs, long command output) are stored once and referenced, so the event stream stays small.
- Each agent's usage events add up to **tokens and estimated cost per agent and per swarm**, shown in the dashboard and enforced by the budget guard.
- Limitation: agents' private reasoning isn't available unless the host streams it. The dashboard shows what agents *do* and *say*, plus their declared intent.

---

## 4. MCP tool surface

The list is kept deliberately small (about 14 tools), because every tool description costs context tokens in every agent.

**Lead / orchestration**
| Tool | Purpose |
|------|---------|
| `tazzy_start(goal, max_agents?, adapter?, isolation?)` | Create a swarm session and register the caller as lead |
| `tazzy_plan(tasks[])` | Submit or extend the task DAG (ids, deps, caps, files, priority) |
| `tazzy_status(verbose?)` | A compact snapshot of agents, tasks, leases, and blockers |
| `tazzy_control(action, target)` | `spawn`, `retire`, `cancel`, `reprioritize`, `pause`, `resume`, `stop` |

**Worker lifecycle**
| Tool | Purpose |
|------|---------|
| `tazzy_join(caps[], name?)` | Register as a worker (mode B). Returns the agent id and a board summary |
| `tazzy_next(wait?)` | Claim the best ready task, optionally blocking until one exists |
| `tazzy_progress(task, note, files_touched?)` | Checkpoint plus heartbeat |
| `tazzy_done(task, summary, artifacts?)` | Complete the task and release its leases |
| `tazzy_fail(task, reason, retryable)` | Give up on the task cleanly |
| `tazzy_propose(parent, tasks[])` | Add discovered subtasks or follow-ups (adaptation, G5) |

**Coordination**
| Tool | Purpose |
|------|---------|
| `tazzy_lease(paths[], mode, release?)` | Acquire or release file leases |
| `tazzy_send(to, kind, body, reply_to?)` | Send a message |
| `tazzy_wait(until, timeout?)` | Block on `task:<id>`, `msg`, `lease:<path>`, or `any` |
| `tazzy_board(op, key?, value?)` | Get, set, or list the board, or append a decision |

MCP **resources** expose read-only views (`tazzy://status`, `tazzy://board`, `tazzy://task/{id}`), and MCP **prompts** provide `/tazzy <goal>` for hosts that support prompt templates.

---

## 5. End-to-end flow

1. You type `/tazzy "Add OAuth login with tests" --max 5` in Claude Code.
2. Your session becomes the **lead**. It explores the repo and calls `tazzy_plan` with a DAG, for example: `schema → (backend-auth, frontend-login) → integration-tests → docs`.
3. The scheduler sees a parallel width of 1 (only `schema` is ready) and spawns **1** worker, not 5.
4. `schema` finishes and publishes the `user-schema` artifact. Two tasks become ready, so the swarm scales to **2** workers. `integration-tests` waits.
5. The backend worker needs a field the frontend worker didn't expect, so it `tazzy_send(to="task:frontend-login", kind="info")`. The frontend worker sees the message piggybacked on its next tool call and adjusts.
6. The frontend worker's process crashes. The Monitor detects the exit, releases its leases, and requeues the task with its checkpoints. A fresh worker resumes it.
7. Both tasks finish. `integration-tests` becomes ready and an idle worker (with file affinity) claims it. The extra worker sits idle and is **retired**.
8. The lead receives a summary of all task results, the changed files, and the decision log, and reports to you.

The TUI shows every step live.

---

## 6. Terminal dashboard (`tazzy tui`)

```
 ▄▀▄▀▀▄  ▄▀▀▄ ▄▀▀  ▀█▀ AGENT TAZZYRONTH           session: oauth-login   ⏱ 00:12:41
 ▀▄▄▀▄█  █▄▄▀ ▀▄▄   █  swarm 3/5 agents · 7/12 tasks · 2 blocked · 0 failed
 ─────────────────────────────────────────────────────────────────────────────────
 AGENT   ADAPTER  STATE     TASK                       FILES (leased)        ♥
 lead    claude   ● plan    —                          —                     1s
 w1      claude   ● run     backend-auth (62%)         src/auth/**           3s
 w2      codex    ◐ wait    frontend-login → w1:schema web/login.tsx         2s
 w3      claude   ✖ dead    (reassigned: tests-unit)   —                     —
 ─────────────────────────────────────────────────────────────────────────────────
 TASK GRAPH  schema ✔ ─┬─ backend-auth ● ─┬─ integration-tests ○ ── docs ○
                       └─ frontend-login ◐┘
 ─────────────────────────────────────────────────────────────────────────────────
 MESSAGES  12:40 w1→task:frontend-login  "User now has `provider` field"
           12:39 hub  w3 exited (code 1) → tests-unit requeued (attempt 2/3)
 [↑↓] select  [enter] agent log  [g] graph  [m] messages  [b] board  [k] kill  [q] quit
```

### 6.1 Agent detail view (`enter` on an agent): everything that agent is doing
```
 w1 · claude · ● running · backend-auth (62%) · 14m02s · 38.2k tok · ~$0.41      [f]ollow ON
 NOW ▸ "Implementing refresh-token rotation in src/auth/refresh.go"
 ┌ ACTIVITY ───────────────────────────────┐┌ LIVE DIFF  src/auth/refresh.go   +24 −3 ──────┐
 │ 12:41:07 ✎ Edit   src/auth/refresh.go   ││  41   func Rotate(t Token) (Token, error) {   │
 │ 12:40:55 ▶ Bash   go test ./auth/...    ││  42 +     if t.Expired() {                    │
 │          ✖ exit 1 · 2 failed            ││  43 +         return Token{}, ErrExpired      │
 │ 12:40:31 ◉ Read   src/auth/token.go     ││  44 +     }                                   │
 │ 12:40:20 ⌕ Grep   "RefreshToken"  (7)   ││  45 -     return t, nil                       │
 │ 12:40:02 ✉ → task:frontend-login        ││  46 +     return issue(t.UserID)              │
 │          "User now has provider field"  ││                                               │
 └──────────────────────────────────────────┘└───────────────────────────────────────────────┘
 FILES   ✎ src/auth/refresh.go +24 −3   ✎ src/auth/token.go +2 −1   🔒 src/auth/** (write)
 OUTPUT  --- FAIL: TestRotateExpired (0.00s)  refresh_test.go:31: want ErrExpired, got nil
 [↑↓] scroll  [enter] expand event  [d] diff picker  [o] raw output  [m] message agent  [k] kill  [esc] back
```
- **Activity feed:** every tool call as one line (edit / read / command / search / message / lease). `enter` expands an event to its full arguments, result, or output.
- **Live diff pane:** follows the file the agent most recently edited (`d` picks any file it touched), updated as edits land.
- **Follow mode:** with `f` on, the view auto-scrolls and the diff pane jumps to each new edit.
- **Message the agent:** `m` sends a `directive` or `info` message from you, which the agent sees on its next tool call. This is a human-in-the-loop override.

### 6.2 Other views
| Key | View | Shows |
|-----|------|-------|
| `a` | **Swarm activity** | All agents' feeds merged on one timeline, color-coded by agent. This is the best view for watching agents collaborate |
| `F` | **Files** | Every changed file → which agent changed it, `+/−` counts, its lease holder, and its diff. Conflicts and unattributed edits are highlighted crimson |
| `g` | **Graph** | The task DAG with state, owner, and attempts per node |
| `m` | **Messages** | The full message bus, filterable by agent or topic |
| `b` | **Board** | Shared KV notes and the decision log |
| `u` | **Usage** | Tokens and cost per agent, plus remaining budget |

### 6.3 Logo and rendering
- **Logo:** `agent logo.jfif` is converted **at build time** (`tools/logo2ansi`, a Go program using the stdlib `image/jpeg`) into truecolor half-block (`▀`) art for the splash screen and a compact 2–3 line mark for the header. The output is embedded with `go:embed`, so there is no image decoding at runtime. There are 256-color and plain-ASCII fallbacks for limited terminals (`NO_COLOR` is respected).
- **Theme from the logo:** obsidian black background, **magenta `#E0218A`** accents (active), **crimson `#B3123A`** (errors/dead), smoky silver (idle/text).
- The splash (full logo plus the name) shows for about 1 s on `tazzy tui` startup and on `tazzy` with no arguments.
- Rendering: hand-rolled ANSI with diff-based redraw (only changed lines are rewritten) at 10 fps max, so CPU stays near 0% when idle.
- `tazzy status` prints a one-shot, non-interactive version for CI or quick checks. `tazzy watch --json` streams the same events as NDJSON. It is the feed that the in-host integrations (§7.3) and the web view read.

### 6.4 Optional web view (zero extra dependencies)
The hub can serve `http://127.0.0.1:<port>/` (behind the same token): one embedded HTML file, updated over Server-Sent Events, built with the stdlib `net/http` only. It is mainly for IDE-based hosts like **Antigravity**, Cursor, or VS Code, where it can open in the editor's built-in browser panel next to the chat. It shows the same data as the TUI. It is off by default; turn it on with `--web`.

---

## 7. Host integration (G8)

### 7.1 `/tazzy` slash command in every host
Typing `/tazzy <goal> [--max N]` in your agent CLI turns the current session into the swarm **lead**. It then calls `tazzy_start`, which starts the hub and **opens the dashboard automatically** (§7.2). Each host has its own native way to define the command, and `tazzy init` installs it:

| Host | How `/tazzy` is defined | Invoked as |
|------|------------------------|-----------|
| **Claude Code** | A plugin bundling a `/tazzy` command, the MCP server entry, the playbooks, the lease hook, and the in-terminal UI mod (§7.3). Installed with `/plugin install` | `/tazzy …` |
| **Codex CLI** | A custom prompt file `~/.codex/prompts/tazzy.md` (or a Codex skill) plus the MCP server in `config.toml` | `/prompts:tazzy …` |
| **Gemini CLI** | A custom command `.gemini/commands/tazzy.toml` plus `mcpServers` in `settings.json` | `/tazzy …` |
| **Antigravity** | A workflow `.agent/workflows/tazzy.md` plus an MCP server entry | `/tazzy …` |
| **Cursor / others** | A rules / command snippet plus MCP | host-specific |

*(The command file formats and locations are checked against each host's current docs in Phase 7. Hosts change these often.)*

### 7.2 Where the dashboard appears
When `tazzy_start` runs, the hub's **launcher** works out where you are and opens `tazzy tui` in the best place available. You can always override it with `"tui.open": "split" | "tab" | "window" | "inline" | "web" | "none"`.

| You are in… | Default placement | Mechanism |
|-------------|------------------|-----------|
| **Claude Code** (any terminal) | **Inside Claude Code**: a live pane, plus a swarm band above the prompt and a status-line entry (§7.3) | Tazzyronth Claude Code mod |
| **Windows Terminal** | Split pane next to your agent CLI | `wt -w 0 sp -V --title Tazzyronth tazzy tui` |
| **tmux** | Split pane | `tmux split-window -h "tazzy tui"` |
| **WezTerm / kitty** | Split pane | `wezterm cli split-pane -- tazzy tui` / `kitten @ launch --location=vsplit tazzy tui` |
| **iTerm2 / macOS Terminal** | New tab or window | `osascript` |
| **Linux desktop** | New window | `$TERMINAL`, `x-terminal-emulator`, `gnome-terminal` |
| **IDE integrated terminal** (Antigravity, VS Code, Cursor) | Web view in the IDE browser panel, or a new OS terminal window | §6.4 / OS launcher |
| **Unknown / headless** | Nothing opens. The tool result tells the agent to suggest `! tazzy tui` | — |

Detection uses environment variables the terminals set (`WT_SESSION`, `TMUX`, `WEZTERM_PANE`, `KITTY_WINDOW_ID`, `TERM_PROGRAM`, `CLAUDECODE`, …). This is a few dozen lines with no dependencies.

### 7.3 Inside the Claude Code terminal itself
Claude Code's plugin system can draw live UI inside its own terminal. The Tazzyronth plugin ships a small **mod** (TypeScript, run by Claude Code itself, so it adds nothing to the Go binary) that:
- **Opens a live "Tazzyronth" pane** with the agent table, the selected agent's activity feed and live diff, and the logo. It uses `$.ui.open` plus a `ui.render` hook on `Pane`. Claude Code seats a pane that opens automatically only on terminals at least about 110–144 columns wide, so on narrow terminals the mod falls back to the band below and the external launcher (§7.2).
- **Shows a one-line swarm band above the prompt** (`AbovePrompt`): `🕷 3/5 agents · 7/12 tasks · w2 waiting on w1 · ✎ w1 src/auth/refresh.go`.
- **Adds a status-line entry** (`$.ui.status`) and **toasts / notifications** for important events (an agent died, a task was reassigned, the swarm finished, a budget warning).
- **Gets its data** by spawning `tazzy watch --json` (`$.process.spawn` streams each NDJSON line into the mod's state atoms, which redraw the pane). No polling.
- **Reports subagent activity** through a `tool.call` hook. It sees every subagent's tool calls tagged with `agentId` and forwards them to the hub (source 2 in §3.9). This gives mode B full per-agent visibility.
- **Can spawn the workers itself** (`$.agent.register` defines a `tazzyronth:worker` subagent type with the worker playbook; `$.agent.spawn` runs it) when the hub asks for a new mode-B agent. The scaling loop then works even inside a single Claude Code session.
- Registers the `/tazzy` command (`$.command.register`).

Fallback for Claude Code builds without mod support: a classic `statusLine` command (`tazzy status --line`) plus classic hooks for activity reporting, with the dashboard opened in a split pane or window per §7.2.

Codex, Gemini CLI, and Antigravity currently have no comparable API for drawing live panes inside their own UI. For them the dashboard opens in a split pane, a new window, or the web view, and `tazzy_status` gives a compact in-chat snapshot on request.

### 7.4 Per-host MCP setup

| Host | Integration |
|------|-------------|
| **Claude Code** | `claude mcp add tazzyronth -- tazzy mcp`. A **plugin/skill** bundle adds the `/tazzy` command, the lead playbook prompt, and the optional `PreToolUse` lease-enforcement hook. Mode B uses native subagents; mode A uses `claude -p` workers |
| **Codex CLI** | `~/.codex/config.toml` `[mcp_servers.tazzyronth] command="tazzy" args=["mcp"]`, plus an `AGENTS.md` snippet with the lead and worker playbooks |
| **Gemini CLI / Antigravity** | `settings.json` `mcpServers` entry, plus a `GEMINI.md` / rules snippet |
| **Cursor / VS Code / others** | Standard `mcpServers` JSON |
| **Any shell** | `tazzy run "goal" --max 4 --adapter claude` runs a headless swarm with no MCP host, with the lead itself spawned through an adapter |

`tazzy init` detects which hosts are installed and writes their configs and playbook snippets (each one confirmed first).

**Playbooks** (prompt files embedded in the binary, also served as MCP prompts):
- `lead.md`: how to explore, decompose into a DAG with declared files and caps, size tasks, monitor, and re-plan.
- `worker.md`: the claim → lease → work → checkpoint → done loop, how to ask instead of guess, how to never edit unleased files, and how to call `tazzy_wait` instead of polling.

---

## 8. Configuration

`.tazzyronth/config.json` in the project, with `~/.tazzyronth/config.json` as global defaults. CLI flags override both:
```json
{
  "max_agents": 5,
  "min_agents": 0,
  "default_adapter": "claude",
  "isolation": "lease",              // "lease" | "worktree"
  "spawn_mode": "auto",              // "hub" (A) | "host" (B) | "auto"
  "timeouts": { "heartbeat_s": 60, "stall_s": 300, "idle_retire_s": 90, "wait_max_s": 45 },
  "retries": { "max_attempts": 3 },
  "scheduler": { "weights": { "priority": 3, "critical_path": 2, "cap_match": 5, "affinity": 2, "conflict": 4, "load": 1, "attempts": 1 } },
  "budget": { "max_total_spawns": 20, "max_runtime_min": 120 },
  "adapters": { "...": "see §2.4" },
  "tui": { "fps": 10, "logo": "auto" }   // "auto" | "full" | "mini" | "off"
}
```

---

## 9. Repository layout

```
agent_swarm/
├── cmd/tazzy/main.go            # subcommands: hub, mcp, tui, run, status, init, replay, hook
├── internal/
│   ├── proto/                   # JSON-RPC 2.0 + NDJSON codec (shared by MCP and hub wire)
│   ├── mcp/                     # stdio MCP server: initialize, tools/list, tools/call, resources, prompts
│   ├── hub/                     # hub server, sessions, auth, event fan-out
│   ├── graph/                   # task DAG, cycle detection, critical path
│   ├── sched/                   # scoring, assignment, elastic scaling
│   ├── lease/                   # path/glob leases, TTLs, wait queues
│   ├── bus/                     # inboxes, addressing, delivery/ack
│   ├── board/                   # KV + decision log
│   ├── monitor/                 # heartbeats, stall detection, failure policy
│   ├── supervisor/              # process spawn/kill, adapters, job objects, logs
│   ├── store/                   # event log, snapshot, replay
│   ├── worktree/                # optional git worktree isolation + merge
│   ├── activity/                # normalized activity stream, per-adapter stream decoders, hook intake
│   ├── diff/                    # file snapshots, mtime watcher, Myers diff
│   ├── launch/                  # terminal detection + open TUI in split/tab/window
│   ├── web/                     # optional embedded HTML + SSE dashboard
│   └── tui/                     # ANSI renderer, layout, input, theme, views
├── assets/
│   ├── logo.jfif                # source (renamed copy of "agent logo.jfif")
│   └── logo_*.ans               # generated, embedded
├── playbooks/                   # lead.md, worker.md (embedded)
├── integrations/
│   ├── claude-code/             # plugin: /tazzy command, MCP entry, hooks, in-terminal UI mod (TS)
│   ├── codex/                   # prompts/tazzy.md, config.toml snippet, AGENTS.md snippet
│   ├── gemini/                  # commands/tazzy.toml, settings snippet
│   ├── antigravity/             # workflows/tazzy.md, MCP snippet
│   └── cursor/
├── tools/logo2ansi/             # build-time logo converter
├── test/sim/                    # fake scripted agents for deterministic swarm tests
└── docs/
```

---

## 10. Delivery phases

| Phase | Scope | Exit criteria |
|-------|-------|---------------|
| **0. Skeleton** (½ day) | `go mod init`, CLI router, `proto` codec, `logo2ansi` + splash | `tazzy` prints the logo; codec round-trip tests pass |
| **1. Hub core** | Task graph, event log + replay, board, bus, hub server, auth, `hub.json` discovery | Unit tests cover the DAG, replay determinism, and message delivery |
| **2. MCP shim** | stdio MCP server, auto-start hub, all §4 tools, piggybacked inbox, `tazzy_wait` | Claude Code connects; a manual two-session demo can claim and complete tasks |
| **3. Leases + scheduler** | Lease manager (globs, TTL, waits), scoring, pull/push assignment | Simulator: 10 fake agents × 50 tasks, zero write-lease overlaps, DAG order respected |
| **4. Supervisor + elastic scaling** | Adapters (claude / codex / gemini / custom), spawn and retire, Job Objects, logs, scaling loop | The `--max 5` demo spawns 1→3→1 workers to match parallel width |
| **5. Failure handling** | Monitor, stall detection, requeue with checkpoints, escalation | Chaos sim: killing random agents still finishes every task with no lost checkpoints |
| **6. Activity capture** | Activity stream, stream-json decoders (claude / codex / gemini), `tazzy hook report`, file snapshot + diff engine, unattributed-edit alerts, usage tally | A simulated and a real `claude -p` worker show every tool call and a correct live diff |
| **7. TUI** | Overview, agent detail (feed + live diff + output), swarm activity, files, graph, messages, board, usage; `tazzy watch --json` | Runs smoothly at 120×40 and 80×24; idle CPU under 1%; diff pane keeps up with rapid edits |
| **8. Host integrations + launcher** | `tazzy init`; `/tazzy` for Claude Code / Codex / Gemini / Antigravity; terminal launcher (wt / tmux / wezterm / kitty / macOS / Linux); Claude Code plugin with the in-terminal mod (pane, band, status, subagent reporting, worker spawning); playbooks | `/tazzy` on Claude Code shows the live pane in the same terminal; on Codex in Windows Terminal it opens a split pane; one real goal completed end to end on each |
| **8b. Web view** (optional) | Embedded HTML + SSE dashboard | Opens in the Antigravity / VS Code browser panel |
| **9. Worktree isolation** | Per-agent worktrees, integrator merge, conflict → task | Two agents editing the same file in parallel merge cleanly or produce a conflict task |
| **10. Release** | goreleaser-style cross-builds (plain `go build` matrix script, no extra tool needed), npm/winget/brew wrappers, README with the logo | Install with one command on Win/Mac/Linux |

Phases 0–3 produce a usable mode-B coordinator inside a single Claude Code session. Phase 4 and later make it a true self-scaling swarm.

---

## 11. Testing strategy
- **Unit tests** for each package (graph, sched, lease, bus, store), table-driven, stdlib `testing` only.
- **Deterministic simulator** (`test/sim`): scripted fake agents connect over the real wire protocol, with configurable speed, failure rate, file overlap, and message chatter. It exercises the full hub without spending any LLM tokens. Runs in CI.
- **Chaos mode:** random kills, delayed heartbeats, and dropped connections. Invariants checked: every task ends `done` or `failed`, no two write leases ever overlap, and no checkpoint is lost.
- **Replay tests:** any `events.jsonl` replays to a byte-identical state snapshot.
- **Live smoke tests** (manual or nightly): small real goals on Claude Code and Codex.

---

## 12. Risks and mitigations
| Risk | Mitigation |
|------|------------|
| LLM agents ignore the protocol (edit without a lease, forget to heartbeat) | Piggybacked inbox makes every call a heartbeat; strong playbooks; `PreToolUse` lease hook on Claude Code; worktree isolation as a hard guarantee |
| MCP tool-call timeouts on `tazzy_wait` | Cap waits at 45 s (configurable); the agent loops; the response always includes "still waiting on X" |
| Token and cost blow-up from N agents | Elastic scaling (rarely at max), budget guard, compact board summaries, small tool surface |
| Headless CLI flags change between versions | Adapters are config templates; `tazzy doctor` validates each adapter |
| Orphaned processes on Windows | Job Objects with `KILL_ON_JOB_CLOSE` |
| Poor decomposition by the lead | Lead playbook with task-sizing rules; workers can `tazzy_propose` and raise blockers; the lead re-plans live |
| Host stream formats / hook payloads change (stream-json, `codex --json`) | Decoders are isolated per adapter, tolerate unknown fields, and fall back to the file watcher + self-reported intent so visibility never drops to zero; `tazzy doctor` checks each one |
| Claude Code mod API is new and may change | Keep the mod thin (it renders `tazzy watch --json` data and forwards hook events); classic `statusLine` + hooks + external pane remain a working fallback |
| Hosts without in-UI panes (Codex, Gemini, Antigravity) | Auto split-pane / window launcher, web view for IDEs, in-chat `tazzy_status` |
| Hand-rolled MCP drifts from the spec | Pin to a specific MCP protocol version, run a conformance test against the official inspector, keep the codec isolated in `internal/mcp` |

---

## 13. Open questions (defaults chosen and can be changed)
1. **Language:** ✅ Decided: **Go**. The only non-Go piece is the optional Claude Code mod, which is TypeScript run by Claude Code itself.
2. **Default isolation:** `lease` (simple, works without git). `worktree` is opt-in.
3. **Default spawn mode:** `auto`, which means host subagents when the lead is inside Claude Code and hub-spawned CLIs otherwise.
4. **Binary name:** `tazzy` (short to type), with the product shown as *Agent Tazzyronth*.
5. **Logo rights:** confirm the artwork can be redistributed in a public release. If not, ship a placeholder and keep the logo for local builds.
