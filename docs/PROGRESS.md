# Build Progress

Handoff log between build phases. Each phase appends one section: what was built, how to run/test it, deviations from `IMPLEMENTATION_PLAN.md` (and why), and anything the next phase must know. Keep each section short.


## Phase 0 — Skeleton

**What exists**
- `go.mod` — module `tazzyronth`; only dep is `golang.org/x/sys` (Windows VT mode).
- `cmd/tazzy` — table-driven subcommand router (`commands` slice). `tazzy` prints the splash + usage; `tazzy version` works; every other §9 subcommand prints "not implemented yet" (exit 1). Unknown command → usage, exit 2. Colour only when stdout is a terminal, `NO_COLOR`/`TERM=dumb` unset, and truecolor is available (`COLORTERM=truecolor|24bit`, or Windows with VT enabled via `vt_windows.go`).
- `internal/proto` — JSON-RPC 2.0 `Message` (single struct; `IsRequest/IsNotification/IsResponse`), `Error` (implements `error`), constructors, NDJSON `Reader` (skips blank lines, tolerates CRLF, malformed line → `*Error` with `CodeParseError`/`CodeInvalidRequest` and the stream stays usable; batches rejected) and mutex-guarded `Writer` (one `Write` per message).
- `tools/logo2ansi` + `assets/` — `logo.jfif` (source) → `logo_full.{ans,txt}` (39×19 cells) and `logo_mini.{ans,txt}` (7×3). `.ans` is truecolor half-block; `.txt` is a colourless half-block silhouette. Near-white background is emitted as bare spaces. Embedded by package `assets`; regenerate with `go generate ./assets`. `.gitattributes` marks `assets/logo_*` as `-text` so CRLF conversion can't corrupt them.

**Commands**: `go build ./...`, `go vet ./...`, `go test ./...`, `go run ./cmd/tazzy`.

**Deviations**
- No 256-colour variant: non-truecolor terminals get the plain silhouette. Add one in Phase 7 if anyone needs it.
- No per-subcommand `flag.FlagSet`s yet — nothing takes flags. Each phase adds its own when it implements its command (set the `run` field in `commands`).
- The 1 s splash delay (§6.3) belongs to `tazzy tui`; bare `tazzy` prints and exits.

**Notes for Phase 1 (hub core)**
- Use `proto.NewReader/NewWriter` on the TCP conn. `Reader.Read` returning `*proto.Error` means "reply with `proto.NewError(nil, err)` and continue"; any other error means the conn is dead.
- `Message.ID` is raw JSON — compare/echo it as bytes, don't re-type it.
- `proto.MaxLineSize` is 32 MiB; a longer line ends the stream (`bufio.ErrTooLong`).


## Phase 1 — Hub core

**What exists**
- `internal/graph` — task DAG. `Add(tasks...)` is atomic: unique ids, deps must exist (forward refs inside the batch are fine), cycle detection reports the path (`a -> b -> a`). States per §3.1 with an explicit transition table; `pending→ready` is automatic only (on insert and when a dep goes `done`); claim needs an owner and bumps `Attempts`; `→ready` clears the owner; `done`/`cancelled` are terminal, `failed→ready` is a retry. `Heights()` (longest remaining chain from each task through its dependents) and `CriticalPath()` for the scheduler.
- `internal/store` — append-only `events.jsonl` (`Event{seq,time,type,data}`). `Open` returns existing events and truncates a torn last line; a corrupt middle line is an error.
- `internal/bus` — per-agent queues of unacked messages (at-least-once: read doesn't consume, `Ack(ids)` does), dedup by message id (client may supply `id`; otherwise `m<seq>`). Addresses `agent:`, `role:`, `topic:`, `broadcast` (fan-out excludes the sender; zero recipients is an error), `task:<id>` (queued on the task and read by whoever owns it *now*, so it follows reassignment).
- `internal/board` — KV with per-key version (namespace = key prefix, `List(prefix)`) + decision log.
- `internal/hub` — server, state, client. All state changes go through `state.apply(event)`, which is deterministic in the event, then the event is logged and fanned out. Startup replays the log. Listens on `127.0.0.1:0`, writes `.tazzyronth/hub.json` `{port, token, pid}` (removed on clean `Close`). Refuses to start if `hub.json` points at a live hub (`ErrRunning`); a stale one is overwritten.
- `tazzy hub [-dir DIR]` runs it until Ctrl+C.

**RPC methods** (params/result types are exported from `internal/hub`)
- `hello {token}` — must be first on every connection; anything else, or a bad token, gets `-32001` and the connection is closed. 10 s deadline.
- Mutations (logged, fanned out): `agent.join`, `task.add`, `task.state {id,state,agent?,result?}`, `task.note`, `msg.send`, `msg.ack`, `msg.subscribe` (topic), `board.set`, `board.decide`.
- Queries: `agent.list`, `task.get`, `task.list {state?}`, `msg.inbox {agent}`, `board.get {key}` (null if missing), `board.list {prefix?}` → `{entries, decisions}`, `status` → full `Snapshot`.
- `events.subscribe` → `{seq}`; then `"event"` notifications carrying each `store.Event`. A subscriber whose 256-event buffer fills is disconnected.
- Errors: `-32000` operation failed (bad transition, cycle, unknown agent...), `-32601`, `-32602` as usual.

**Run/test**: `go test ./...`; `go run ./cmd/tazzy hub`. `-race` not run: the only C toolchain here is 32-bit MinGW (`cc1: 64-bit mode not compiled in`); hub tests were run 30× without failures instead.

**Deviations**
- No snapshot (`state.json`); replay is a single pass over the log, fast enough for now. Add if logs get large.
- No inbox bound (a bound would drop messages, contradicting at-least-once); no per-message `seq` acks beyond the RPC response itself (the response carries `id` and `seq`).
- Connections aren't bound to an agent identity; methods take `agent`/`from` explicitly. `task.state` doesn't check that the caller is the owner (belongs with leases/scheduler, Phase 3).
- Dependents of a failed/cancelled task stay `pending` forever (Phase 5 policy).
- Event log is not fsynced; if an append fails the in-memory state has already moved on (error is returned).
- Requests on one connection are handled sequentially.

**Notes for Phase 2 (MCP shim)**
- `hub.Connect(projectDir)` discovers and authenticates; `Client.Call(ctx, method, params, &result)` returns `*proto.Error` for hub errors. Auto-start: try `Connect`; on failure spawn `tazzy hub -dir ...` detached and poll `Connect`.
- `tazzy_wait` / `tazzy_next(wait)`: there's no server-side wait. Either use `Client.Subscribe` in the shim and re-query on relevant events, or add a blocking hub method — but with sequential per-connection handling, a blocking method needs its own connection or a goroutine per request.
- `tazzy_next` claim = `task.list {state:ready}` + `task.state {state:claimed, agent}`; the claim fails cleanly if someone else won the race. Proper scoring is Phase 3.
- Piggybacked inbox = `msg.inbox`; ack what you've shown the agent with `msg.ack`.


## Phase 2 — MCP shim

**What exists**
- `internal/mcp` — hand-rolled stdio MCP server. `initialize` (speaks 2025-06-18, 2025-03-26, 2024-11-05; echoes the client's version if supported, else offers 2025-06-18; capabilities `tools` only, plus a short `instructions` string), `notifications/initialized`, `notifications/cancelled`, `ping`, `tools/list`, `tools/call`. Tool failures are in-band (`isError: true`); unknown tool → `-32602`. Tool calls run one at a time in arrival order on a worker goroutine, so pings/cancels are answered while `tazzy_wait` blocks.
- `tazzy mcp [-dir DIR]` — connects lazily on the first tool call that needs the hub. No live hub → spawns `tazzy hub -dir <abs dir>` detached (Windows: `DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP`; Unix: `setsid`), output to `.tazzyronth/hub.log`, and polls for up to 10 s. `.tazzyronth/hub.lock` (O_EXCL, stale after 30 s) stops concurrent shims from starting two hubs. Dropped hub connections are re-dialled (and the hub re-started) once per call. Logs → stderr.
- Each shim subscribes to the hub event stream; every event wakes blocked tools, which re-query the hub. **No hub changes were needed** for `tazzy_wait`.

**Tools**: `tazzy_start`, `tazzy_plan`, `tazzy_status`, `tazzy_control`, `tazzy_join`, `tazzy_next`, `tazzy_progress`, `tazzy_done`, `tazzy_fail`, `tazzy_propose`, `tazzy_send`, `tazzy_wait`, `tazzy_board`.
- Stubbed: `tazzy_control` implements only `cancel`; `spawn/retire/reprioritize/pause/resume/stop` return an error.
- Omitted: `tazzy_lease` (Phase 3); `tazzy_wait(until="lease:…")` returns an error.
- Lead registers with the fixed id `lead` (a restarted lead shim keeps its identity); workers get hub-assigned ids. Only `tazzy_start/join/status` work before registering.
- Piggyback: every result has a second text block `{"inbox":{"messages":[…],"notices":[…]}}` when there is something; messages are acked as they are shown. Notices so far: "your task was cancelled".
- `tazzy_next` = highest `priority` ready task whose `caps` ⊆ the agent's caps (ties by id); returns spec, checkpoints and the deps' results. `wait` blocks up to 45 s. `tazzy_wait`: `task:<id>` (done/failed/cancelled), `msg`, `any` (message or any task add/state change); default 30 s, max 45 s.

**Register with Claude Code** (not run here): `claude mcp add tazzyronth -- C:\path\to\tazzy.exe mcp` — the hub lives in the host's working directory; pass `mcp -dir <project>` to pin it.

**Tests**: `internal/mcp/mcp_test.go` — protocol over pipes (initialize/negotiation, ping, tools/list, tools/call errors); a lead + two workers on a real hub (plan with a dep, claim, `tazzy_wait(task:)` unblocks on done, `tazzy_wait(msg)` unblocks with the message piggybacked, `task:` messages follow the owner, cancel notice); hub restart reconnect. Run 30× clean. Manually checked: piped JSON-RPC into the built binary in an empty dir → hub auto-started, `hub.json` written, second shim reused it.

**Deviations / known gaps**
- No MCP resources or prompts (`tazzy://…`, `/tazzy`): deferred to Phase 8 with the playbooks.
- `tazzy_start` takes only `goal` (stored on the board as `goal`); `max_agents/adapter/isolation` belong with the supervisor (Phase 4).
- Inbox delivery to the LLM is at-most-once: acked before the reply is written.
- The auto-started hub never exits on its own (no idle shutdown yet). If the host kills its whole Windows job, the hub goes with it; the next shim restarts it from the log.
- **Concern:** Claude Code native subagents share the parent session's MCP servers, so in mode B lead and subagents would share one shim and one agent id. Likely fix: an optional `agent` argument on worker tools (or one shim per agent via mode A). Decide before Phase 8.

**Notes for Phase 3 (leases + scheduler)**
- Replace `claim()` in `internal/mcp/tools.go` with a hub-side `sched.next {agent}` RPC (atomic score + claim), and drop the client-side cap filter.
- Add `tazzy_lease` to the `tools` table and a `lease:<path>` case in `wait()`; lease events will wake waiters automatically (any hub event re-runs the check).
- Owner checks for `task.state`/`task.note` are still missing hub-side (the shim doesn't check either); add them with leases, and release leases on done/fail.
- Blocked tools re-query on *every* hub event; fine for tens of agents, revisit if the event rate grows.


## Phase 3 — Leases + scheduler

**What exists**
- `internal/lease` — `Normalize(root, p)` (forward slashes, cleaned, relative to the project root, lower-case on Windows, `.` → `**`, rejects paths outside the project and bad globs; idempotent). `Overlap(a, b)`: segment-wise match with `*`/`?`/`[..]` and `**`; **every pattern also covers everything beneath it** (so `src/api` leases the directory). Two globs in one segment are compared by literal prefix/suffix only, so the answer is conservative (may say "overlap", never misses one). `Table`: `Acquire` is all-or-nothing and returns `[]Conflict` naming the holder; own leases never conflict; re-acquire updates mode/TTL. `Release(holder, paths)` (empty = all).
- `internal/sched` — pure `Score`/`Pick` per §3.2: priority, critical-path height, caps (hard filter, then +per required cap so specialists take specialised work), file affinity (declared files overlapping the agent's leases and files of tasks it finished), −lease conflicts, −attempts, −`Refail` if this agent failed/gave it up. `sched.Defaults` (plan §8 values); ties → smallest id.
- Hub: lease table, `failedBy` and `left` in `state` (all replayed). Task `files` are normalized on `task.add`. Ending a task (done/failed/ready/cancelled) releases **all** of the previous owner's leases. **Owner checks**: `task.state` (non-claim) and `task.note` that name an `agent` must be the owner; calls naming no agent (lead's cancel) still pass.
- TTL: the hub records the last request naming each agent (`agent`/`from`/`by` param) in memory; a 250 ms ticker releases leases whose holder has been silent longer than the lease TTL (default 300 s) via an ordinary logged `lease.release {reason:"expired"}` — replay never consults the clock. After a restart the clock restarts for every holder.

**New RPCs**: `lease.acquire {agent,paths,mode?,ttl?}` → `{leases}` or error `-32002` (`CodeConflict`, message names holders, `data` = `[]lease.Conflict`); `lease.release {agent,paths?,reason?}` → `{released}`; `lease.check {agent?,paths,mode?}` → `{conflicts}`; `lease.list {agent?}`; `sched.next {agent}` → claimed task or null (scores + claims under one lock, logged as a normal `task.state` event); `agent.leave {agent}` → releases leases, requeues its claimed/running/blocked tasks, removes it (id is never auto-assigned again). `status` snapshot gains `leases`.

**Tool changes**
- `tazzy_lease(paths, mode?, release?)`: a conflict is a normal result `{granted:false, conflicts, held_by, hint}`, not a tool error. `tazzy_wait(until="lease:<path>")` is met when a write lease would be granted (`held_by` in the result). `tazzy_next` uses `sched.next` (client-side claim/cap filter removed). `tazzy_done/fail` pass the agent (owner check + lease release). Compact `tazzy_status` lists leases.
- **Shared shim**: every tool except `tazzy_start/join` takes optional `agent`; it overrides the shim's default identity for that call (incl. the piggybacked inbox and notices, now kept per agent). The first registration in a shim becomes the default (`tazzy_start` always does); `tazzy_join` returns a `note` telling the agent to pass its id, and doesn't piggyback (it might be someone else's inbox). **Tool calls now run concurrently** in the shim — otherwise one subagent's 45 s `tazzy_wait` blocked the lead and all other subagents sharing it.

**Simulator** (`test/sim`): `Run(ctx, dir, Config{Agents, Tasks, Seed, Files, MaxDeps, Work, FailRate})` starts a real hub, plans a seeded random DAG whose tasks declare overlapping files (paths + `pkgN/**` globs, some needing cap `go`), and runs fake agents over `hub.Client`: `sched.next` → lease all files at once (retry on conflict; no hold-and-wait, so no deadlock) → sleep (per-agent speed) → done, or retryable fail. Two independent checks: a monitor that mirrors leases/claims from the event stream (overlapping write/read-write leases; claim before deps done) and an in-process "who is editing what" tracker; finally every task must be `done` and no lease left. `TestSwarm` runs 10 agents × 50 tasks for 3 seeds (~0.3 s total, ~150 lease conflicts per run); 20× clean. `TestMonitor` proves the monitor flags violations.

**Deviations**
- No push suggestions; no `agentLoad` term (for a pull by one agent it's the same for every candidate). Weights are a struct, no config file.
- Leases are per agent, not per task: finishing any task releases all of the agent's leases (an agent works one task at a time). No per-call TTL in the tool (RPC has `ttl`).
- Conflicting requests fail fast; there's no server-side queue — `tazzy_wait(lease:…)` + retry is the queue. A conflicting task is penalised, not skipped, by the scheduler.
- Glob escapes (`\*`) unsupported: backslashes are separators.
- The sim is deterministic in its inputs (graph, speeds, failures per seed), not in goroutine interleaving.

**Notes for Phase 4 (supervisor + elastic scaling)**
- Retire/dead agents with `agent.leave` — it already releases leases and requeues tasks. Mode-A workers can be given their id up front (`agent.join {id}`).
- Parallel width: ready tasks whose declared files don't pairwise `lease.Overlap`; `state.pick`/`Heights` show how to read the graph under `h.mu`.
- `h.seen` (last activity per agent) is the natural heartbeat source for stall detection; it isn't persisted.
- Shims are now concurrent per call; the session is mutex-guarded, and `hub.Client` was already safe for concurrent calls.


## Phase 4 — Supervisor + elastic scaling

**What exists**
- `internal/config` — `.tazzyronth/config.json` unmarshalled over `Default()` (missing fields keep defaults; `adapters` merge by name over the built-ins). Fields: `max_agents` (5), `min_agents` (0), `default_adapter` (`claude`), `spawn_mode` (`host`), `timeouts.{idle_retire_s 90, spawn_cooldown_s 5, scale_tick_s 2, hub_idle_s 600}` (float seconds), `budget.max_total_spawns` (20), `scheduler.weights` (sched.Weights, now JSON-tagged), `adapters.<name>.{cmd, env}`. No global `~/.tazzyronth` layering, no `max_runtime`, `isolation`, `retries`, `tui`.
- `internal/supervisor` — `Spawn(agent, adapter, prompt)`: writes `.tazzyronth/run/<agent>/{mcp.json,prompt.md}`, expands placeholders `{prompt} {prompt_file} {mcp_config} {agent_id} {workdir} {project} {tazzy}` in argv and env, runs in the project dir with stdout+stderr → `.tazzyronth/logs/<agent>.log`, sets `TAZZY_AGENT_ID`. Windows: one Job Object per worker with KILL_ON_JOB_CLOSE (tree dies on kill, on worker exit, and with the hub). Unix: process group, killed on kill/exit/hub `Close` (not on a hub crash). `Kill`, `Live`, `Close`; exit reported via callback. `worker.md` is the embedded bootstrap playbook.
- MCP config given to workers: `{"mcpServers":{"tazzyronth":{"command":<tazzy exe>,"args":["mcp","-dir",<project>,"-agent",<id>]}}}`. **Identity binding:** the hub pre-registers the id (`agent.join {id, adapter}`, id is in the logged event); `tazzy mcp -agent <id>` presets the shim's identity and `tazzy_join` re-joins with it (keeps `adapter`).
- Hub: `StartWith(dir, Options{Exe, Config})` (`Start` = defaults; `Exe` is what workers' MCP configs run — tests pass a built `tazzy`). Scaling tick (§3.3): in spawn mode `hub`, `desired = clamp(min, busy workers + width, max)` where width = greedy id-order pick of ready tasks whose files overlap neither each other nor any current lease; spawn one per cooldown while live < desired and the budget lasts. In any mode, hub-spawned workers idle ≥ `idle_retire_s` are retired while live > desired. Exit of a still-registered worker → `agent.leave {reason:"exited: exit status N"}` (releases leases, requeues its task). On start, agents with an `adapter` left over from a previous hub are removed (`reason:"hub restarted"`). `Hub.Idle()` closes after `hub_idle_s` with no connections and no worker processes; `tazzy hub` then exits.
- New RPCs: `swarm.set {spawn?, max_agents?, adapter?}` (logged; status snapshot gains `swarm`), `agent.spawn {adapter?}` → Agent (errors: unknown adapter, at max_agents, budget), `agent.retire {agent, reason?}`. `agent.leave` takes `reason`; `Agent`/`JoinParams` gain `adapter`.
- Tools: `tazzy_start(goal, spawn?, max_agents?, adapter?)`; `tazzy_control` `spawn` (target = adapter, optional; works in either mode) and `retire` (target = agent). pause/resume/stop/reprioritize still unimplemented.
- Scheduler: `sched.Pick` now ranks any conflict-free task above every conflicting one regardless of weights (lexicographic, not a weight tweak); score orders within each group.

**Adapters** (built-in; all request a JSON event stream for Phase 6)
- `claude` — **verified** with `claude --help`: `claude -p {prompt} --mcp-config {mcp_config} --strict-mcp-config --allowedTools mcp__tazzyronth --permission-mode acceptEdits --output-format stream-json --verbose` (`--verbose` is required for stream-json with `-p`; MCP tools must be allowed or `-p` denies them).
- `codex` — **unverified** (not installed): `codex exec --json --full-auto --skip-git-repo-check -C {workdir} -c mcp_servers.tazzyronth.command='{tazzy}' -c "mcp_servers.tazzyronth.args=['mcp','-dir','{project}','-agent','{agent_id}']" {prompt}`.
- `gemini` — **unverified** (not installed): `gemini -p {prompt} --output-format stream-json --approval-mode auto_edit --allowed-mcp-server-names tazzyronth`, env `GEMINI_CLI_SYSTEM_SETTINGS_PATH={mcp_config}` (same `mcpServers` shape). MCP tool calls may still need approval in auto_edit; check when installed.

**Tests**: `internal/supervisor` (exit status observed, run dir written, Kill takes a grandchild with it — test binary as helper process). `internal/hub/scale_test.go` (width, idle shutdown, spawn errors/budget). `internal/mcp/control_test.go` (start settings, control spawn/retire, preset id). `test/fakeagent`: a Go program that reads its MCP config, starts `tazzy mcp` and drives it like an LLM (join → next → lease → progress → done); `TestMain` builds it and `tazzy`. `TestElasticScaling`: max_agents 5, DAG width 1→3→1 → exactly 3 workers alive at peak (counted from the event log), all retired as idle afterwards (~1.7 s). `TestWorkerDeath`: a worker exits with status 3 while holding a lease → leave logged with the status, lease released, task finished by the next worker. 10× clean; no stray processes (checked with tasklist).

**Deviations**
- No `small_buffer`, no `max_runtime`; spawn budget counts failed starts too (counted on pre-registration, replayed from the log).
- Retirement is immediate (leave + kill), not "finish the current call": only idle workers are retired automatically, so nothing is lost; `tazzy_control retire` on a busy agent requeues its task.
- `spawn_mode` has no `auto`; default `host`, `tazzy_start(spawn="hub")` or config switches it.
- Supervised worker exits during `Hub.Close` aren't logged; the next start cleans them up.
- **Accident during development:** an old Phase 2 assertion (`tazzy_control spawn` must fail) briefly launched a real `claude -p` once before the hub's Close killed it (milliseconds). Tests now only use the fake adapter or nonexistent ones.

**Notes for Phase 5 (failure handling)**
- Death today = `agent.leave` with a reason: leases released, task straight back to `ready` (the next claim bumps attempts; the dead agent is *not* added to `failedBy`). Phase 5 owns retry limits, backoff, escalation to the lead, and stall detection (`h.seen` as heartbeat; `Supervisor.Kill` for mode A).
- The hook is `Hub.workerExited`; exit code is in the error text (`exit status N`). Worker stdout (JSON stream) is only in the log file; Phase 6 parses it.
- Windows: a worker's children started in the microseconds before `AssignProcessToJobObject` could escape the job. Adapters that resolve to `.cmd` shims (npm-installed codex/gemini) go through cmd.exe, which mangles multi-line `{prompt}` args — use `{prompt_file}` or a stdin-based template if that bites.


## Phase 5 — Failure handling

**What exists**
- Config: `timeouts.stall_s` (300), `timeouts.stall_grace_s` (60), `retries.max_attempts` (3; a task's own `max_attempts` overrides), `retries.escalate_after` (2).
- **Monitor** (hub, same ticker as lease expiry; tick = min(250 ms, stall/4, grace/4)): any request naming an agent (`agent`/`from`/`by`) is a heartbeat (`h.seen`). An agent that **holds work** (an active task or a lease) and is silent for `stall_s` gets a `directive` from `hub` in its inbox; still silent `stall_grace_s` later it is removed (`agent.leave {reason:"stalled: silent for …"}`) and, if hub-spawned, its process tree is killed. Idle agents are never stalled out (a worker blocked in `tazzy_next(wait=45)` makes no RPCs).
- **Failure policy** — new logged event/RPC `task.fail {id, agent, reason, retryable}`: records a failure note (`Task.Failures`), adds the owner to `failedBy` (scheduler prefers another agent), releases its leases; requeues if retryable and `attempts < max_attempts`, else `failed` **and every pending dependent fails with it** (failure note "dependency X failed"). `agent.leave` (death, stall, retire, exit, hub restart) runs the same policy for each active task, as a retryable failure. The hub copies `max_attempts` into the event so replay doesn't depend on config. The hub messages `role:lead` (kind `blocker`, from `hub`) when a task fails for good (with cascaded ids) or reaches `escalate_after` distinct failing agents, listing every failure reason. No lead registered → silently skipped.
- **Removed agents**: any request naming an agent that left (except `agent.join`) gets `-32003 CodeRemoved` "agent X was removed (why): stop working; … tazzy_join to start over". `checkOwner` is now strict: naming an agent on an ownerless (e.g. requeued) task is refused too.
- **Handover**: `Note.Files` (from `tazzy_progress.files_touched`, no longer folded into the text). `tazzy_next` returns `task.{checkpoints:["agent: note"], files_touched, failures:["agent: why"]}` plus a resume hint on attempts > 1. `tazzy_fail` calls `task.fail`. `worker.md` mentions heartbeats, resuming and removal.
- **Test guard**: `supervisor.Spawn` refuses `claude`/`codex`/`gemini` (any path/extension) when `testing.Testing()`.

**Tests**: `internal/hub/failure_test.go` (retry → escalation → final failure + cascade + lead messages; stall ping/answer vs. removal, late RPCs refused, rejoin gets the checkpoints), replay test covers `task.fail`, `internal/mcp/failure_test.go` (handover context, removed worker, lead told), `test/fakeagent` `TestWorkerStall` (`-hang-on`: hub-spawned worker hangs → killed, task finished by the next one with max_agents 1). `test/sim` **chaos** (`TestChaos`, 4 seeds × 10 agents × 50 tasks, stall 60 ms, ~2 s): silent kills (replaced by fresh agents), stalls past stall+grace that come back, delays between stall and stall+grace, done requests whose connection drops before the reply, retryable and fatal fails. Invariants: every task ends done/failed; no overlapping write leases (event monitor + in-process editors); every acknowledged checkpoint present on each re-claim and at the end; every done/fail event comes from the current owner. 20× clean, plus 40 extra seeds once; whole suite ~13 s for `-count=3`.

**Deviations**
- No backoff before a retry (requeue is immediate; the `Refail` penalty steers it elsewhere). Escalation is a message only; retries continue until `max_attempts`.
- Leaving with a task (including a manual `retire` of a busy agent or "hub restarted") burns an attempt.
- Cascade on terminal failure only; dependents of a *cancelled* task still stay pending. A lead retry of a failed task (`failed→ready`) isn't exposed and wouldn't revive cascaded dependents.
- No `heartbeat_s`/`wait_max_s` config; stall checks are in-memory (`seen`, `pinged`), the resulting leave is logged.
- The chaos sim models a stalled agent as not editing while silent; a real zombie LLM could still write files until its next call — only worktree isolation fixes that.

**Notes for Phase 6 (activity capture)**
- Heartbeats are only hub RPCs naming an agent. Parsed worker stdout events (mode A) and host hooks (mode B) are natural extra heartbeats: update `h.seen[agent]` and clear `h.pinged[agent]` (as `Hub.touch` does) so a worker busy in a long tool call isn't stalled out.
- Failure notes (`Task.Failures`), `task.fail` and `agent.leave {reason}` events and hub `directive`/`blocker` messages (`from:"hub"`) are ready-made activity items (`state`/`alert`).


## Phase 6 — Activity capture

**What exists**
- `internal/activity` — `Activity{id, agent, task, ts, kind, source, summary, files, detail, detail_size, usage}` (kinds per §3.9 plus `tool`; sources `stream|hook|diff|hub`). `Store`: per-agent ring (config `activity.ring`, 500) + `.tazzyronth/activity/<agent>.jsonl`; details > 4 KiB go to `activity/blobs/<id>.json` (`detail` empty, `detail_size` set). Ids are project-wide and increasing; reopening restores rings, ids and the full usage tally.
- Decoders (`decode_*.go`, `activity.Decode(format, r, emit)`): line-based, panic-safe per line, always drain the pipe. Tool calls → read/edit/cmd/search/tool with args as detail; results only for commands and failures (`cmd_result`); text → `say`; usage attached to the event it came with. tazzy_* tool calls are dropped: the hub records coordination itself (intent from `task.note`, `lease`, `msg`, task/agent `state`).
- Supervisor tees stdout of adapters with a `stream` field (new `config.Adapter.Stream`; built-ins set claude/codex/gemini) into the hub's decoder; `WaitDelay` 2 s so an inherited pipe can't hold the worker's exit open. Decoded and hook activity are heartbeats.
- `internal/diff` — LCS unified diff (prefix/suffix trimmed; > 4M cells degrades to one block), 3 lines of context. `Watcher`: snapshots write-leased files on lease (synchronously, under the hub lock), polls stamps every `activity.poll_s` (1 s), and checks each file once more when its lease ends, so an edit just before `tazzy_done` is still attributed to the holder and task. Globs/dirs: up to 200 existing files snapshotted; files > 1 MiB or with NUL bytes are reported as changed, not diffed. Unattributed sweep every `activity.sweep_s` (5 s, 0 = off) only in a git repo: `git status --porcelain -z -uall` (gitignore-aware, `.tazzyronth` excluded); the first sweep is a baseline; a file that changes while no write lease covers it → `alert` under agent `hub`.
- `tazzy hook report [-dir] [-agent]`: reads the payload, connects (never starts a hub), 2 s cap, prints nothing, always exits 0. Dir: `-dir`, `$CLAUDE_PROJECT_DIR`, payload `cwd`, `.`; `-agent` defaults to `$TAZZY_AGENT_ID`.
- Usage: `status` gains `usage {agents, swarm}` (tokens + cost as reported by the CLI); compact `tazzy_status` shows a swarm line.
- Phase 5 fix: cancelling a task fails its pending dependents (note "dependency X was cancelled") and messages the lead. `task.state` now returns `TaskStateResult` (the task, flattened, plus `cascaded`).

**New RPCs**: `activity.list {agent?, after?, limit?=100}` → oldest-first, merged across agents if no agent; `activity.detail {id}` → raw detail JSON; `activity.hook {agent?, payload}` → `{agent, recorded}`; `events.subscribe {activity:true}` (client `SubscribeActivity`) adds unlogged `"activity"` events (seq 0, data = Activity). Plain subscribers (the MCP shims) don't get them. `activity.list`/`activity.hook` aren't heartbeats and aren't refused for removed agents.

**Decoder status**: claude written from the stream-json shape and checked against the Phase 4 adapter args (`-p … --output-format stream-json --verbose`), not against a live run. Usage dedups by assistant message id and tops up to the final `result` totals/`total_cost_usd`. codex and gemini are **unverified** (not installed) and say so in their files; they report no cost. Fixtures in `internal/activity/testdata`.

**Hook assumptions**: fields `session_id`, `hook_event_name`, `tool_name`, `tool_input`, `tool_response`, `error`, and `agent_id`/`agent_type` for subagent events. Host identity = `agent_id`, else `session_id`. It is bound to an agent when the hook ran with `-agent`, when a tazzy tool call carries `agent` in `tool_input`, or when tazzy_join/tazzy_start's response contains `"agent"` (searched through content blocks/JSON text). Bindings are in memory (re-bound by the next tazzy call after a restart); unbound identities record as `host-<first 8 chars>`. PreToolUse records the call, PostToolUse(Failure) the result (commands and failures only), so configure both. Hooks from a hub-spawned worker whose stream is decoded only count as a heartbeat (no duplicates). Nothing installs the hooks yet (Phase 8 `tazzy init`).

**Tests**: decoder fixtures + hostile input, store reload/blobs/tally, hook parsing, diff exactness + 500 random patches applied back + minimality, watcher (holder, new file, binary, release check), sweep, git status parsing; hub: intent, diff edit → holder/task, hook binding, live vs plain subscribers, stream usage in status, persistence across restart, cancel cascade, unattributed alert (real `git init` in a temp dir); `cmd/tazzy` hook report (hub down → 0 fast; bound to `-agent`); `test/fakeagent -claude-stream` E2E: tool calls, exact diff, intent and usage in the hub's activity. 10× clean; no stray processes.

**Deviations**
- Diff is LCS, not Myers (simpler; bounded table). No separate `.gitignore`-aware walk of the repo: the sweep is `git status` only and is skipped outside git.
- The diff engine's `edit` and the stream's `Edit` tool call are both recorded (source `diff` vs `stream`); the TUI should show the diff one in the diff pane and may hide one in the feed.
- No budget enforcement from usage, no price table (cost only where the CLI reports it). Adapter overrides in config replace the built-in wholesale, so a custom `claude` adapter must set `"stream": "claude"` itself.
- Watched globs can hold up to 200 × 1 MiB in memory per lease; fine for now.

**Notes for Phase 7 (TUI)**
- Bootstrap with `activity.list {agent, limit}` (ring is 500), then follow `SubscribeActivity` and dedup by `id` (use `after` to catch up after reconnecting). Fetch `activity.detail` for `detail_size > 0`; edits from source `diff` have detail `{path, diff, added, removed}` for the live diff pane. NOW headline = latest `intent`. Usage per agent/swarm is in `status.usage`. Alerts live under agent `hub`.


## Phase 7 — TUI

**What exists**
- `internal/tui` — no framework. `Model` (snapshot, activity by id, messages, gone agents, view state) + `Render(m, w, h) []string` (pure; exactly h lines, none wider than w, last line w-1 so it can't scroll) + `Model.Key(name) Action` (the side effect — send, retire, load an agent's history — runs in `term.go`). `term.go`: raw mode (`golang.org/x/term`, now a dep), alt screen, hidden cursor, line-diff redraw (only changed lines rewritten; full clear on resize, size polled each tick), ≤10 fps (redraws when data changed, on keys, and once a second for clocks), splash ~1 s (any key skips; skipped if the terminal is smaller than the logo). Terminal restored on q, Ctrl+C (0x03 in raw mode), SIGINT/SIGTERM, stdin EOF and panic. A data goroutine connects (`status` + `activity.list` + `SubscribeActivity`), refetches `status` after logged events (one in flight), fetches `activity.detail` for what a view needs, and on failure shows "hub not running — waiting" / "hub connection lost — reconnecting" (keeps the last data on screen, catches up with `after`). Untrusted text is stripped of control characters; widths are display widths (small built-in wide-rune table); only single-width symbols are used.
- Views: overview (mini logo header, swarm summary, agent table with up to 3 recently gone agents, task graph as layers `a ✔ → b ●  c ◐ → d ○`, newest messages), agent detail (header, NOW = latest intent, activity feed, live diff of the latest edit or `d`-picked file / expanded event with full detail, files touched + leases, output tail; side by side at ≥100 cols, stacked below), swarm activity (merged, agent column coloured per agent), files (changed files, agent, +/−, lease holder, crimson conflict/unattributed, diff of the selected file), graph (layered table, owner, attempts, critical path, last note/failure), messages (`/` filters by from/to), board (notes + decisions), usage (per agent + swarm).
- **Keys**: overview `↑↓` select, `enter` agent, `k` retire (y/N), `m` messages; everywhere `a` activity, `F` files, `g` graph, `b` board, `u` usage, `esc` back, `q`/Ctrl+C quit, `↑↓ PgUp PgDn Home End` scroll. Agent detail: `↑↓` move the cursor (stops follow), `f` follow, `enter` expand event, `o` latest command output, `d` cycle diff file, `←→` previous/next agent, `m` message (`tab` toggles directive/info; sent as `msg.send {from:"human", to:"agent:<id>"}`), `k` retire (`agent.retire`).
- **Edit de-dup** (`feed()`): a diff-engine edit (source `diff`) is folded into the latest still-uncounted tool edit (stream/hook) by the same agent of the same file (case-insensitive), which then shows `+N −M`; a diff edit with no such call stays its own line ("✎ diff"). Usage-only events are hidden from feeds.
- `tazzy tui [-dir] [-no-splash] [-frame WxH [-agent ID]]` — `-frame` prints one frame of the live state without a terminal (debugging, and handy for hosts). `tazzy status [-dir]` plain summary (exit 1 if no hub); `tazzy status --line` one line, e.g. `tazzy 3/3 agents · 0/4 tasks · 1 blocked · ✎ a3 web/login.tsx` (`tazzy: no hub` and exit 0 when down). `tazzy watch [--json] [-dir]`.
- **`watch --json` format**: one `store.Event` per line, `{"seq","time","type","data"}`. Per connection it starts with `{"type":"status","data":<hub.Snapshot incl. usage>}` (seq 0), then every hub event: logged ones (`seq>0`, `type` = RPC name, `data` = its params, e.g. `task.state`, `agent.leave`, `msg.send`) and `{"type":"activity","seq":0,"data":<activity.Activity>}`; after any logged event a fresh `status` follows within 500 ms. When the hub is unreachable it emits `{"type":"disconnected","data":{"error":…}}` once and retries every second; the next connection starts again with `status`. It never starts a hub. Exits when stdout closes.

**Tests**: frames for every view at 80×24 and 120×40, with and without colour (line count, no line over width, no control chars, escapes iff colour, expected content); offline/reconnecting/tiny frames; edit merge; keys (navigation, message, retire confirm, expand); on-demand detail; key parsing; `fit`/`clean`; status text/line; `watch` (disconnected → status → events → status → disconnected) and `status`/`-frame` against an in-process hub. `TAZZY_DUMP=1 go test ./internal/tui -run TestDump -v` prints all frames. Manual: fake swarm (fakeagent, `-claude-stream`) with status/line/watch/`-frame`; the real `tazzy tui` driven in a ConPTY (splash, enter, a, g, esc, m, q; Ctrl+C; hub killed mid-run → "reconnecting", then q) — exit 0, alt screen and cursor restored, no stray processes.

**Deviations**
- No forced black background (the terminal's own background is used); no 256-colour palette — colour is truecolor or nothing, like Phase 0.
- No "remaining budget" in usage (the hub doesn't expose the spawn budget yet), no task progress %, and the task graph is a layered line rather than the mockup's branch drawing. Dead agents come from `left:` state activity (last 3 shown).
- Messages: agents' messages come from `msg` activity (history survives a TUI restart); hub-sent messages (stall pings, lead blockers) only from live `msg.send` events and unacked queues.
- No mouse, no diff-pane scrolling (the diff shows from its first hunk), no config (`tui.fps/logo`).

**Notes for Phase 8**
- Claude Code mod: spawn `tazzy watch --json -dir <project>`; keep the last `status` as state and append `activity` events (dedup by `id`, apply the same edit-merge rule as `tui.feed`); treat `disconnected` as "swarm offline". The band text can be taken from `tui.StatusLine` semantics, or just run `tazzy status --line` for the classic `statusLine`.
- Launcher: open `tazzy tui -dir <project>`; it waits for the hub if it isn't up yet, so it can be launched before or after `tazzy_start`.
- The TUI's connection counts as a hub connection, so an open dashboard keeps an idle hub alive.


## Phase 8a — Host integrations + launcher

**What exists**
- `playbooks/` (package `playbooks`): `lead.md` (explore → board decisions → DAG with files/deps/caps and task sizing → workers per spawn mode, incl. mode B via the host's subagent tool → monitor with `tazzy_wait`/`tazzy_status` → re-plan on blockers/failures → final check + report) and `worker.md` (join → next → lease → work/progress → ask, don't guess → done/fail; pass your id; `tazzy_wait`, never poll; host subagents stop after two empty waits). `internal/supervisor/worker.md` is gone: `supervisor.Prompt` = identity header + `playbooks.Worker`.
- MCP **prompts** (capability `prompts`): `tazzy` (args `goal` required, `max`) → lead playbook + goal + the `tazzy_start(...)` call to begin with; `tazzy-worker` → worker playbook.
- `tazzy_start` returns `{agent, spawn, max_agents, adapter?, next, dashboard?, playbook}` (the lead playbook, so host commands stay a few lines) and opens the dashboard once per shim. `tazzy_join` returns the worker playbook unless the shim was started with `-agent` (hub-spawned workers already have it).
- **Spawn mode `auto`** (new config default): `tazzy_start` sends the MCP `clientInfo.name` to the hub (`swarm.set {client}`); auto → `host` if the name contains "claude" (Claude Code lead spawns subagents), `hub` for any other client (so `/tazzy` in Codex/Gemini/Cursor/Antigravity actually gets workers), `host` while no lead is known. `swarm.set` now returns the effective settings.
- `internal/launch`: `Decide(Env, mode, argv) Plan` (pure) + `Open(Plan)` (start, don't wait). `tazzy mcp` (without `-agent`) wires it into `tazzy_start` with config `tui.open` (`auto`|`split`|`tab`|`window`|`none`; `inline`/`web` → "not supported"). Failure lands in the `dashboard` field with a "run `tazzy tui -dir …`" hint; never fatal.
- `integrations/` (package `integrations`, embedded): `skill/tazzy/SKILL.md` (shared Agent Skill), `gemini/commands/tazzy.toml`; manual snippets `codex/config.toml`, `gemini/settings.json`, `antigravity/mcp_config.json`, `cursor/mcp.json`.
- `tazzy init [-y] [-dry-run] [-host a,b]` (`internal/setup` plans read-only; `Apply` writes): detects hosts by binary on PATH or config dir, prints every change, asks y/N per host. JSON: only `mcpServers.tazzyronth` is added (key order kept, file re-indented, BOM dropped); an existing entry is never touched; unparsable JSON (e.g. comments) → host skipped with an error. TOML: block appended if there's no `mcp_servers.tazzyronth` (text detection). Entries use the absolute path of the `tazzy` that ran init, so install it somewhere stable first.

**Launcher** (first match wins; `auto` = `split`, falling back to an OS window)
| Env | split / tab |
|---|---|
| `TMUX` (+tmux) | `tmux split-window -h '<cmd>'` / `tmux new-window` |
| `TERM_PROGRAM=vscode` (VS Code/Cursor/Antigravity terminal) | OS window (ignores an inherited `WT_SESSION`) |
| `WEZTERM_PANE` | `wezterm cli split-pane --right -- …` / `wezterm cli spawn -- …` |
| `KITTY_WINDOW_ID` | `kitten @ launch --location=vsplit …` / `--type=tab` (needs remote control) |
| `WT_SESSION` (+wt) | `wt -w 0 sp -V --title Tazzyronth …` / `wt -w 0 nt …`; window `wt -w new nt …` |
| OS window | Windows `wt -w new nt …` else `cmd /c start "" …`; macOS osascript Terminal `do script` / iTerm2 `create window … command`; Linux (needs `DISPLAY`/`WAYLAND_DISPLAY`) `$TERMINAL -e`, `x-terminal-emulator -e`, `gnome-terminal --`; SSH or headless → nothing |

`CLAUDECODE` gets the same as its terminal for now. Nothing was opened on this machine; argv is checked in tests only.

**Per-host files** (checked 2026-10-17 against official docs unless marked)
| Host | MCP | /tazzy |
|---|---|---|
| Claude Code | init runs `claude mcp add --scope user tazzyronth -- <tazzy> mcp`, skipped if `~/.claude.json` has it (verified: `claude mcp --help`, https://code.claude.com/docs/en/mcp) | none here (8b plugin) |
| Codex | `$CODEX_HOME` or `~/.codex/config.toml` `[mcp_servers.tazzyronth]` (https://learn.chatgpt.com/docs/extend/mcp) | `~/.agents/skills/tazzy/SKILL.md`, invoked `$tazzy <goal>` or via `/skills`. Custom prompts (`~/.codex/prompts`, `/prompts:…`) are **deprecated** in favour of skills (https://learn.chatgpt.com/docs/custom-prompts, https://learn.chatgpt.com/docs/build-skills) |
| Gemini CLI | `~/.gemini/settings.json` `mcpServers` (https://geminicli.com/docs/tools/mcp-server/) | `~/.gemini/commands/tazzy.toml` (`description`, `prompt` with `{{args}}`), `/tazzy <goal>` (https://geminicli.com/docs/cli/custom-commands/) |
| Antigravity CLI + IDE | `~/.gemini/config/mcp_config.json` `mcpServers` (https://www.antigravity.google/docs/mcp/) | skills `~/.gemini/antigravity-cli/skills/tazzy/` (CLI) and `~/.gemini/config/skills/tazzy/` (IDE), `/tazzy` (https://www.antigravity.google/docs/skills/). Workflows stop working **2026-10-19**, so none is written (https://www.antigravity.google/docs/migration/workflows-to-skills/) |
| Cursor | `~/.cursor/mcp.json` `mcpServers`, `type: stdio` (https://cursor.com/docs/context/mcp) | `~/.agents/skills/tazzy/SKILL.md` (Cursor reads it too; one file serves Codex and Cursor), `/tazzy` (https://cursor.com/docs/skills) |

Unverified: how a skill receives text typed after `/tazzy` (no doc covers it, so the skill tells the agent to treat it as the goal); the MCP `clientInfo` names of the non-Claude hosts (auto only looks for "claude"); detection by binary names `agy`, `antigravity`, `cursor`, `cursor-agent`; whether global MCP servers in Cursor and Antigravity start in the project dir (`tazzy mcp` uses its cwd). **Gemini CLI was retired for free and Google One accounts on 2026-06-18** and replaced by Antigravity CLI; its files are still written if it's installed. None of these hosts is installed here, so none was tried live.

**Tests**: `internal/launch` (19 env fixtures → placement + exact argv, nothing opened); `internal/setup` (temp HOME: detection; all five hosts over existing unrelated config; key order and BOM; second plan empty; existing entries kept; bad JSON untouched; `CODEX_HOME`; the claude CLI call recorded, not run); `cmd/tazzy` `doInit` (dry-run writes nothing, per-host y/N, rerun is up to date); `internal/mcp` prompts list/get/errors and `tazzy_start` auto (claude-code → host, other → hub, explicit wins, dashboard opened once, playbooks returned). `go build`, `go vet`, `go test -count=1 ./...` clean; no stray processes. Ran `tazzy init -dry-run` here: only Claude Code detected, nothing written.

**Deviations**
- Skills replace Codex custom prompts and Antigravity workflows (both deprecated per the docs), and Cursor gets a skill instead of a `.cursor/commands` file. No `AGENTS.md`/`GEMINI.md`/rules snippets: the skill plus the playbook in `tazzy_start` replace them. No hooks installed (Claude Code hooks are the 8b plugin's job; other hosts' hooks not done).
- `tui.open` `inline`/`web` are not implemented. tmux and osascript get a POSIX-quoted command string.
- Hub-mode workers still default to `default_adapter` (claude) whatever the lead's host; the lead can pass `adapter`.

**Notes for Phase 8b (Claude Code plugin + mod)**
- Bundle the MCP entry (`tazzy mcp`), a `/tazzy` command whose body can be the skill text (`integrations/skill/tazzy/SKILL.md`: call `tazzy_start`, follow the returned playbook), and `PreToolUse`/`PostToolUse` → `tazzy hook report`. `tazzy init` only runs `claude mcp add --scope user`; if the plugin declares the server too, drop `planClaude` so there aren't two `tazzyronth` servers.
- Mode B: auto already resolves to `host` for Claude Code, and step 5 of the lead playbook has it spawn subagents that call `tazzy_join`. A `tazzyronth:worker` agent type can use `playbooks.Worker` (MCP prompt `tazzy-worker`) as its prompt.
- In-terminal pane: change the Claude Code branch of `launch.Decide`, or have `tazzy mcp` skip `SetLauncher` when the mod is active (e.g. an env var the plugin sets on the MCP server), so a wt split doesn't open as well.
- Cursor also reads `~/.claude/skills`; don't install a `tazzy` skill there or Cursor lists it twice.


## Phase 8b — Claude Code plugin

**Layout** (`integrations/claude-code/`, one plugin named `tazzyronth`, also its own one-plugin marketplace)
- `.claude-plugin/plugin.json` (+ `"types"`), `.claude-plugin/marketplace.json` (`source: "./"`), `.mcp.json` → `tazzy mcp`.
- `commands/tazzy.md` → `/tazzyronth:tazzy <goal> [--max N]` (also `/tazzy` when unambiguous): call `tazzy_start`, follow the returned lead playbook; in host mode spawn workers with the Agent tool, each told to `tazzy_join` and pass its id.
- `hooks/hooks.json` holds both the mod (`"modules": ["./register.tsx"]`) and the classic hooks (validate parses both): `PreToolUse` → `tazzy hook report -enforce` (sync, timeout 5 s), `PostToolUse`/`PostToolUseFailure`/`SubagentStart`/`SubagentStop` → `tazzy hook report` (`async: true`). Exec form (`command` + `args`), so `tazzy` must be on PATH.
- Mod: `hooks/register.tsx` (wiring + drawing), `hooks/swarm.ts` (pure: summary line, agent rows, feed dedup, snapshot-diff toasts, line splitting), `types/index.d.ts` (state `tazzyronth.{swarm, feed, selected}`), `tests/tazzy.test.ts`.
- `session.start` (interactive only) spawns `tazzy watch --json -dir <cwd>` via `$.process.spawn`, restarted 15 s after it exits. `status` → `swarm` atom + toasts from diffing consecutive snapshots (task failed, task reassigned/back to ready, swarm done); `agent.leave` with an exit/stall reason → "agent died" toast; `activity` → `feed` (dedup by id, last 300, details kept only for diff-engine edits); `disconnected` → offline. Status line (`$.ui.status`) and the `AbovePrompt` band show `🕷 2/3 agents · 1/3 tasks · 1 blocked · ✎ w1 src/x.go` only while a swarm is active (band has a `pane` button). Pane `tazzyronth`: mini logo + summary, agent table (rows are Buttons that select), selected agent's last 10 events and latest diff (path, +/−, first 12 lines). `/tazzy-pane` reopens it.
- **Launcher skip**: the mod's `tool.call` hook on `*__tazzy_start` calls `$.ui.open`; if the pane is placed it adds `ui: "host"` to the arguments, and `tazzy_start` (new optional schema field `ui`, enum `host`) then opens no external window (`dashboard`: "shown inside the host"). Not placed (narrow terminal) → args unchanged → the normal launcher runs; band + status line still show.

**Try it**: `claude --plugin-dir integrations/claude-code` (needs `tazzy` on PATH: `go install ./cmd/tazzy`). Install: `tazzy init` now writes the embedded plugin to `~/.tazzyronth/claude-code/` and prints `claude plugin marketplace add <that dir>` then `claude plugin install tazzyronth@tazzyronth` (it installs nothing, and no longer runs `claude mcp add`; if `~/.claude.json` still has a user-scope `tazzyronth` server it says to `claude mcp remove --scope user tazzyronth`, else there'd be two). It warns when `tazzy` isn't on PATH (it isn't on this machine). Nothing was installed or enabled here.

**Verification**: `claude plugin validate integrations/claude-code` → passed (only note: "gating hook without .catch: tool.call", intended — if it throws, `tazzy_start` runs unchanged). `claude plugin test` → 3 pass (pure helpers; fake `watch` stream → toasts, status line, band on terminal+desktop, pane tree; `tazzy_start` gets `ui:"host"`). No `tsc` on this machine (no TypeScript installed, didn't fetch one), so type-checking is validate + the test kit only. Go: build, vet, `test -count=1 ./...` clean; new tests for `hook report -enforce` (holder allowed, other agent denied, unleased/outside-project allowed, hub down allowed) and `tazzy_start ui=host`.

**Hook payload findings**: confirmed by the hooks docs and this build's types: subagent tool calls fire the same `PreToolUse`/`PostToolUse` hooks with `agent_id` + `agent_type`; `SubagentStart`/`Stop` carry `agent_id`, `agent_type` (Stop also `agent_transcript_path`, `last_assistant_message`); `PostToolUseFailure` has `error`. Phase 6's assumptions were right; nothing changed in `activity.FromHook`. Plugin MCP tools are named `mcp__plugin_tazzyronth_tazzyronth__tazzy_*`, which `tazzyTool` already handles (suffix after the last `__`).

**Deviations**
- One reporting path: the classic hooks, always (they work with and without mod support and carry `agent_id`). The mod's `tool.call` hook does not forward anything, so no double-reporting and no mod-active detection needed.
- Lease enforcement folded into `hook report -enforce` (one process per PreToolUse): after `activity.hook` binds/attributes the caller, an `Edit`/`Write`/`MultiEdit`/`NotebookEdit` on a path another agent holds a **write** lease on is denied via `hookSpecificOutput.permissionDecision: "deny"`. Hub down, timeout, paths outside the project, or an error → allowed. An unbound session (`host-…`, e.g. a non-swarm session in the same project) is also denied on leased files.
- Deferred: the mod spawning workers itself (`$.agent.register`/`$.agent.spawn`); the lead playbook spawns them via the Agent tool. No activity history on attach (`watch` starts with `status` only, so the feed fills from live events); no budget toasts; the band says "N blocked" rather than "w2 waiting on w1"; no `/tazzy` registered by the mod (the classic command covers builds without mods). The pane only opens on this session's `tazzy_start` (or `/tazzy-pane`/the band button), not when another session's swarm appears.

**Concerns**: the sync `PreToolUse` hook starts a `tazzy` process on every tool call (a few tens of ms on Windows); drop `-enforce`/make it async if that's noticeable. Unverified: whether a Claude Code build without mod support tolerates the `modules` key in `hooks.json` (this build does); the pane/band were checked in the test kit, not in a live session.

**Notes for Phase 8c (web view)**: `tazzy watch --json` was enough for an in-host UI; the same fold (`hooks/swarm.ts`: status snapshot + deduped activity, snapshot diffs for alerts) ports directly to a browser client over an SSE/WebSocket wrapper of `events.subscribe {activity:true}`. A bootstrap `activity.list` call would fix the empty feed on attach for both. `tazzy_start`'s `ui: "host"` is the hook for "the host shows it" if the IDE web panel opens itself.


## Phase 8c — Web view

**What exists**
- `internal/web`: stdlib `net/http` server on a random `127.0.0.1` port, one embedded `index.html` (inline CSS+JS, no CDN, **8.8 KB**) + `/logo.png` (7 KB transparent PNG, `assets.LogoPNG`, now also written by `tools/logo2ansi` via `go generate ./assets`). Shows the header (logo, `N/max agents · done/total tasks · blocked · spawn · $cost`, live/reconnecting), agent table (live + 3 most recently gone, click to select), task layer line (`a ✔ → b ●`) + task table, messages (agents' `msg` activity + hub-sent `msg.send`), selected agent's NOW + feed + latest diff (diff-engine edit; `/api/detail` fetched for large ones). §6.3 theme.
- Endpoints (all need the token): `GET /` page, `/logo.png`, `/api/snapshot` (= `status`: hub.Snapshot incl. usage), `/api/activity?agent=&after=&limit=` (= `activity.list`, limit ≤ 1000), `/api/detail?id=` (= `activity.detail`, 404 if unknown), `/api/events` SSE: `event: status` (snapshot; first, then ≤300 ms after any logged change), `event: activity` (activity.Activity), `event: event` (logged store.Event); `: ping` every 15 s. The browser reconnects by itself and catches up with `after`.
- Auth: `?token=<hub token>` → HttpOnly SameSite=Strict cookie `tazzy_<port>` (and a 303 to `/` so the token leaves the address bar); otherwise the cookie; else 401. Requests whose `Host` isn't `127.0.0.1`/`localhost` get 403 (DNS rebinding).
- Hub: in-process (no TCP hop) via `webSource` — the hub fans events to "taps" next to its connections (slow tap → closed, client reconnects); an open stream counts as a connection for idle shutdown, like the TUI. `tazzy hub --web`, config `"web": true`, or RPC `web.start` (idempotent, returns `{url}`) start it; the port goes into `hub.json` as `"web"`.
- **Open it**: the URL from `tazzy_start`'s `dashboard` field, or `http://127.0.0.1:<web>/?token=<token>` from `.tazzyronth/hub.json`.
- Launcher: `tui.open: "web"`, or `auto` with `TERM_PROGRAM=vscode` (VS Code/Cursor/Antigravity terminal), → `web.start` + default browser: Windows `rundll32 url.dll,FileProtocolHandler <url>`, macOS `open`, Linux `xdg-open` (needs a display; ssh → nothing). Failure → the dashboard field asks the user to open the URL. Explicit `split/tab/window` in an IDE terminal still opens an OS terminal window.
- **Adapter per host** (`config.WorkerAdapter`): tazzy_start `adapter` > config `default_adapter` (now empty by default) > adapter matching the lead's MCP client name (contains `codex`/`gemini`/`claude`, if that adapter exists) > `claude`. Used by spawns and the `swarm.set` result.

**Tests**: web (no/bad token 401 on every route, foreign Host 403, loopback listener, token URL → 303 + cookie, cookie → page, page < 25 KB, JSON endpoints, SSE status → activity → event → status), hub (`--web` writes the port, `web.start` idempotent, snapshot keys, SSE delivers a `task.add`, tap counted), `dashboardPlan` (IDE terminal → rundll32 argv with the URL and the web server started on demand; console → tui; config `web` wins; nothing opened), launch `WantsWeb`/`Browser` argv, `WorkerAdapter` table + hub `swarm.set` per client. Manual: `tazzy hub --web` in a temp dir, fake swarm via `tazzy mcp` (hub mode, 2 workers, claude-style stream): curl 401/303/200, page 8.8 KB, snapshot/activity, SSE carried activity/event/status during the run; a headless Edge screenshot showed agents, gone workers, task line, feed and the c.txt diff; the hub went idle and removed hub.json; no stray processes.

**Deviations**
- Opens the OS default browser, not the IDE's built-in browser panel (no portable way to drive it from outside; the URL is in the tool result for pasting into one). No `tui.open: inline`.
- Read-only: no message/retire from the page (no CSRF surface to defend). No files/board/usage views; no edit de-dup (stream edits and diff edits both show in the feed).
- Antigravity/Cursor client names unknown, so their workers fall back to `claude` (Antigravity has no adapter anyway). Changing `default_adapter`'s default to `""` means `swarm.set`/tazzy_start report the host-matched adapter.

**Notes for Phase 9 (worktree isolation)**
- Diffs and file paths in the page come straight from activity (`files`, diff-engine `detail.path`), relative to the project root. With per-agent worktrees, keep paths relative to the worktree root (or prefix them) and run the diff watcher per worktree; the page needs no change for that. If agents get a `worktree`/`branch` field in the snapshot, add a column in `index.html`'s agent table (one template line).


## Phase 9 — Worktree isolation

**How it works**
- Config `isolation: "lease" | "worktree"` (default `lease`); `tazzy_start(isolation)` overrides per swarm (`swarm.set {isolation}`). Worktree mode needs the project to be the **top level** of a git repo with at least one commit; otherwise `swarm.set`/`sched.next` fail with a clear message and nothing is recorded.
- `internal/worktree` shells out to `git` (needs **2.38+** for `merge-tree --write-tree`; 2.41 here). On first use the hub creates `tazzy/integration-<UTC yyyymmdd-hhmmss>` at HEAD and records it in `swarm.integration` (logged, kept by later `swarm.set`, shown in `status`, `tazzy_start` and compact `tazzy_status`). Merges are `merge-tree` + `commit-tree` + `update-ref` on that branch: **no checkout is ever touched**, the user's branch and working tree included.
- Each agent has one worktree, `.tazzyronth/worktrees/<agent>` (a `.gitignore` of `*` there keeps them out of the user's `git status`). Hub-spawned workers get it at spawn (detached at the integration head) and run in it (supervisor `Spawn(..., workdir)`; `{workdir}` expands to it). On claim (`sched.next`) the worktree is reset to a fresh branch `tazzy/<task-id>` (sanitised) from the integration head; the result carries `worktree` (host workers: `tazzy_next` returns `worktree` + `worktree_note`; the worker playbook says to work only there and never commit/merge/switch).
- `task.state done`: the hub commits the worktree (`git add -A` minus `.tazzyronth`, message = title + summary; fallback identity `Tazzyronth <tazzy@localhost>` only if the repo has none), merges into integration, detaches the worktree and deletes the task branch. Done before the state change, so a task's dependents always branch from a head that contains it.
- **Conflict flow**: merge aborted (merge-tree writes nothing), task branch kept, task still marked done; a new task `merge-<id>` "resolve conflict merging <id>" (files = conflicted paths, no deps, priority +1, caps inherited, `merge: "tazzy/<id>"`) is added and made a dep of every pending dependent of `<id>` (new `TaskAddParams.Blocks` + `graph.AddDeps`, logged so replay is exact); the lead gets a blocker message. Whoever claims it gets a worktree on `tazzy/merge-<id>` from integration with `tazzy/<id>` merged `--no-commit`, markers in place. `tazzy_done` refuses while conflicted files still contain markers; otherwise the merge commit is concluded and merged like any task (a second conflict just makes another resolve task), and `tazzy/<id>` is deleted.
- Unfinished attempts (`task.fail`, `agent.leave`, `task.state` ready/failed/cancelled): the work is committed and the branch renamed `tazzy/<id>-failed-<attempt>` for inspection; the retry starts clean.
- Cleanup: worktree removed when its agent leaves (host workers) or its process exits (hub-spawned: never while the process holds it as cwd); `git worktree remove --force --force` with retries, falling back to `RemoveAll` + prune (Windows locks). Hub start: `git worktree prune` and removal of worktrees of agents no longer registered.
- Leases unchanged; absolute paths inside the caller's worktree are normalised relative to it (`lease.acquire/release/check`). Diff watcher: `Watcher.Dir(holder)` reads leased files from the holder's worktree, so live diffs/paths stay relative to the worktree root. The unattributed-edit sweep still covers only the project checkout.

**Tests**: `internal/hub/worktree_test.go` (host-style workers on a temp repo: parallel different files → both on integration, new files too; live diff from the worktree; absolute worktree lease path; dependent sees merged work; no-change task; same lines → one merges, `merge-te` created and blocks the dependent, markers in its worktree, done refused with markers, resolved → integration has both; failed attempt kept as `tazzy/tf-failed-1`; worktrees and branches gone; `main`, HEAD and checkout untouched), empty repo refused; `internal/worktree` (`Open` errors, branch names); `internal/mcp/worktree_test.go` (`tazzy_start`/`tazzy_next`/`tazzy_done`/`tazzy_status`); `test/fakeagent` `TestWorktreeWorkers` (2 hub-spawned fake workers editing in their cwd in parallel → both merged, worktrees removed after idle retire). 3× clean, no stray processes. Test repos set local `user.*` and `core.autocrlf=false`.

**Deviations**
- Worktrees are per agent and reused across its tasks (reset per task), not created/removed per task: a hub-spawned worker's cwd can't be deleted under it on Windows. Removal is on agent death/retire.
- No integrator agent; the hub merges inline, in completion order (deps are already merged by construction). Git runs under the hub lock: each claim/done blocks the hub for a few git calls (~100–300 ms on Windows). Move it off the lock if that shows.
- A conflicting task is still marked done (its dependents are held by the resolve task instead). A cancelled task's worktree changes are kept on a `-failed-N` branch like a failure.
- Project must be the repo top level (no subdirectory projects). No `worktree`/`branch` column in the TUI/web agent tables.

**Notes for Phase 10 (release)**
- Document `isolation` in the README/config docs, the git ≥ 2.38 requirement, and that users should add `.tazzyronth/` to `.gitignore` (worktrees are ignored already; the event log etc. are not).
- Release smoke test: a real repo with `isolation: "worktree"`, then `git log --graph tazzy/integration-*`; leftover `tazzy/*-failed-*` branches are expected after failures.


## Phase 10 — Release prep

**What exists**
- `tools/release` (stdlib Go, no make): `go run ./tools/release -version 0.1.0 [-out dist]` empties `dist/`, builds `./cmd/tazzy` with `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X main.version=<v>"` for windows/linux/darwin × amd64/arm64 into `dist/tazzy_<v>_<os>_<arch>/`, archives each (`.zip` windows, `.tar.gz` others; binary + `README.md` + `LICENSE` if present, under a top-level dir, fixed mtime) and writes `dist/SHA256SUMS` (`sha256sum -c` passes). ~16 s. `dist/` was already in `.gitignore`.
- Sizes (0.1.0): binaries 6.9–7.8 MB (windows/amd64 7.81, linux/amd64 7.50, darwin/arm64 7.03), archives 2.9–3.3 MB — under the 10 MB target. `tazzy version` → `tazzy 0.1.0`.
- `tazzy doctor [-dir]` (`cmd/tazzy/doctor.go`): tazzy on PATH, config loads, hub (`hub.json` missing / live via loopback `hello` / stale), each configured adapter's binary on PATH + the default worker adapter, `git --version` (≥ 2.38) and whether the project is a repo top level with a commit, isolation, truecolor, where `tazzy_start` would open the dashboard (`launch.Decide`/`WantsWeb`), Claude Code plugin (`~/.claude/plugins/installed_plugins.json` contains `tazzyronth@`, else `~/.tazzyronth/claude-code` written but not installed). Only `[warn]` lines count; exit 1 if any. No network beyond loopback, no LLM calls.
- `README.md` at the root (logo `assets/logo_web.png`, install, per-host quick start, dashboards + keys, commands, config reference from `internal/config`, diagram, limitations).
- Cleanup: dropped the never-implemented `run` and `replay` subcommands from the router (and the "not implemented" branch); `tazzy_control`'s schema enum is now `cancel|spawn|retire` (unknown actions error). gofmt/vet clean.

**Deviations**
- No npm/winget/brew wrappers and nothing published (no remote). `go install <path>@latest` waits for a real module path; README says to `go install ./cmd/tazzy` from a clone.
- No LICENSE (owner's decision); logo redistribution (plan §13.5) still unconfirmed.
- The archived README's logo link is relative to the repo, so it doesn't render inside the archive.

**Remaining gaps** (see README "Limitations"): codex/gemini adapters and decoders unverified; no live end-to-end LLM swarm run recorded; non-Claude hosts not tried live; Claude Code pane only checked in the plugin test kit; release smoke test of `isolation: "worktree"` on a real repo still to do.


## Phase 11 — Pause/stop, host token counting, Antigravity check

**What exists**
- **Run state** (`internal/hub/state.go`): new logged event `swarm.control` (`ControlParams{Action: pause|resume|stop, By}`), hub RPC `swarm.control` → `{"state": "running"|"paused"|"stopped"}`. The snapshot has a top-level `state` field (`""`/`running` means running). *Paused*: `sched.next` hands out nothing and the autoscaler spawns nothing; claimed and running tasks carry on. *Resume*: back to running. *Stop* is terminal for the swarm: every task that hasn't ended is cancelled (dependents first, so none fails by cascade; claimed ones too), hub-spawned workers are retired, all agents get the directive "swarm stopped by <by>: stop work now, release leases, exit", and the lead is notified. Resume after stop is refused; the next `swarm.set` (`tazzy_start`) runs again. Each change is broadcast as an info message.
- **MCP** (`internal/mcp/tools.go`): `tazzy_control` actions `pause|resume|stop` (no target). `tazzy_next` checks the run state first. Paused returns `{"task": null, "state": "paused", "note": …}`, and the worker waits for a message (these waits don't count as empty). Stopped returns `state: "stopped"`, and the worker exits without `tazzy_done`/`tazzy_fail`. Compact `tazzy_status` has `state` when not running. The lead and worker playbooks describe both.
- **Dashboards**: TUI `p` toggles pause/resume and `x` stops (y/N). Both call `swarm.control` with `By: "human"`. Header badge `⏸ PAUSED` / `■ STOPPED`; footer hints. `tazzy status --line` appends `paused`/`stopped`. Web view: header badge (still read-only). Claude Code plugin: summary in the band and status line shows paused/stopped, with toasts on each change.
- **Host-mode token counting** (`internal/activity/transcript.go`, `internal/hub/activity.go`): Claude Code hook payloads carry no usage, so the hub reads the session's JSONL transcript. That is `agent_transcript_path` for subagents, else derived from `transcript_path` + `agent_id` (`<session>/subagents/agent-<id>.jsonl`). It reads from the last byte offset, summing `message.usage` of assistant lines once per message id. Sidechain lines are skipped unless the file is the subagent's own; a file that shrank is re-read from 0, and each read takes at most 32 MB. The result is recorded as an `activity.Usage` for the bound agent, so it lands in Totals. Reads happen on PostToolUse, PostToolUseFailure, SubagentStop and a new async Stop hook. After a hub restart, reading resumes from the last offset in the recorded usage events, so nothing is counted twice. Hub-spawned streamed workers are unchanged: their stream already counts.
- **Antigravity**, checked live against **agy 1.4.2** (installed here; the `antigravity` binary is not). Sources: agy's built-in docs (`~/.gemini/antigravity/builtin/skills/agy-customizations`), https://antigravity.google/docs/cli/headless/, https://antigravity.google/docs/permissions?tab=cli, https://antigravity.google/docs/skills/, https://antigravity.google/docs/plugins?tab=cli.
  - Setup paths were right. The MCP config is `~/.gemini/config/mcp_config.json`, shared by the CLI and the IDE. agy creates it as an empty file, which setup handles. CLI skills live in `~/.gemini/antigravity-cli/skills/`, IDE skills in `~/.gemini/config/skills/`, and the CLI turns each skill into `/<name>`.
  - MCP `clientInfo.name` is `antigravity-client`. agy sends `server/discover` (protocol 2026-07-28) before `initialize`. The real `tazzy mcp`, run as a workspace plugin server, answered it with method-not-found and then served all 14 tools. MCP servers inherit agy's environment. A plugin server's cwd is its plugin dir, not the project.
  - Headless: `agy -p … --output-format stream-json` prints NDJSON `init` / `step_update` / `result` events. Without a prompt, MCP calls and shell commands are soft-denied (exit 0, `result.denied_actions`). They need `permissions.allow` rules (`mcp(server/*)`, `command(...)`) in `~/.gemini/antigravity-cli/settings.json`.
  - New `antigravity` decoder (`decode_antigravity.go`): text deltas become `say`, tool steps (PascalCase params) become read/edit/cmd/search/tool, `call_mcp_tool` to `tazzy_*` is skipped, ERROR steps become `cmd_result`, and each step's usage becomes `usage` with the result only topping up what the steps missed. Result errors and denied actions become `alert`s.
  - New built-in adapter `antigravity`: `agy -p {prompt} --output-format stream-json --mode accept-edits --print-timeout 0s` with env `TAZZY_PROJECT={project}`. `WorkerAdapter` matches `antigravity` in the lead's client name. agy has no per-run MCP config flag, so workers use the global `tazzyronth` entry: `tazzy mcp`'s `-dir`/`-agent` now default to `$TAZZY_PROJECT`/`$TAZZY_AGENT_ID` (the supervisor already sets the latter).
  - `tazzy init` also appends `mcp(tazzyronth/*)` to `permissions.allow` in `~/.gemini/antigravity-cli/settings.json` (`mergeAllow`; keys and order kept). `tazzy doctor` checks the MCP entry and the rule when agy is installed, and warns if antigravity is the default worker. The supervisor's under-test guard refuses `agy`/`antigravity` too. New manual snippet `integrations/antigravity/settings.json`.

**Tests**: hub `TestPauseResume`, `TestStop`, `TestControlErrors` (`internal/hub/control_test.go`); mcp `TestControl` + `TestPauseStop` (`tazzy_control` pause/resume/stop, `tazzy_next` while paused and after resume); TUI `TestPauseStop` (keys, confirm, badge, still 80 columns); web badge; plugin `tazzy.test.ts` assertions. `TestTranscriptRead`, `TestSubagentTranscript`, more `TestFromHook` cases, hub `TestHookTranscriptUsage` (lead + subagent usage, appended lines, no double count across a hub restart). `TestDecodeAntigravity` + `TestDecodeAntigravityTopUp` (testdata built from real agy runs; four real runs also decoded by hand), `WorkerAdapter("antigravity-client")`, setup over an empty `mcp_config.json` and an existing `settings.json` allow list, `TestMergeAllow`, `TestAgyWorkerSetup`.

**Deviations**
- The web view stays read-only: it shows the run state but has no pause/stop buttons.
- The transcript reader's per-path state is a package-level `sync.Map` keyed by `*Hub` (`hub.go` was leased by another task); it could move to a `Hub` field. Host token counts lag by up to one turn (read when hooks fire).
- agy workers need the global setup from `tazzy init`, and they inherit the user's global agy config. They can't run shell commands unless the user adds `command(...)` allow rules. `--dangerously-skip-permissions` is not used.

**Unverified / remaining**
- No full hub-spawned agy worker run. The `mcp(tazzyronth/*)` rule against a *global* server was not tried (the global `~/.gemini` config wasn't changed here); for a plugin server the rule form seen was `mcp(probe_probe/probe_echo)`.
- The cwd of global MCP servers under Antigravity is very likely not the project, so a `/tazzy` lead there may get the wrong project dir. Follow-up task `mcp-roots-dir` would use MCP `roots/list` (agy advertises `roots`); until then pin `-dir` in the entry.
- The Antigravity IDE/app keep permissions in their own settings UI. How args after `/tazzy` reach a skill is still undocumented. This phase replaces the earlier notes (Phases 7–8) that Antigravity's client name was unknown and that it had no adapter.
