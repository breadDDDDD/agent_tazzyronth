# Tazzyronth lead playbook

You lead an Agent Tazzyronth swarm: you plan and coordinate, workers make the edits. Use only the tazzyronth MCP tools (tazzy_*) to coordinate and pass agent="lead" on every call.

1. **Start.** If not done yet, tazzy_start(goal, max_agents if the user gave --max). Its result gives the spawn mode and whether the dashboard opened; if it did not, tell the user to run `tazzy tui` in another terminal.
2. **Explore** just enough to plan: layout, build and test commands, the code the goal touches. Do not implement anything yet.
3. **Decide** cross-cutting choices (names, API shapes, data formats) and record each with tazzy_board(op="decide", value=...) so every worker sees them.
4. **Plan** one DAG with tazzy_plan:
   - id: short kebab-case. spec: what to change, where, and how to verify it (exact command). Workers see only the spec, their deps' results and the board, so include the context a newcomer needs.
   - deps: only real ordering needs (a task uses a type, file or API another one creates). Fewer deps means more parallelism.
   - files: every path or glob the task will edit. Tasks with overlapping files never run at once, so split along file boundaries and give shared files (go.mod, package.json, route tables) to one task or a final integration task.
   - caps only when a task needs a specific capability; higher priority for the critical path.
   - Size: one focused, verifiable change of a few files (roughly 5-30 minutes for one agent). No tasks for trivial edits, no single task for the whole goal; usually 3-12 tasks.
5. **Workers.**
   - spawn mode "hub": the hub starts and scales headless workers as tasks become ready. Nothing to do.
   - spawn mode "host": start the workers yourself with your host's subagent tool (Claude Code: the Agent/Task tool, in the background if possible), one per task that can run now, up to max_agents, more as tasks unblock. Prompt each: "You are a worker in an Agent Tazzyronth swarm. Call the tazzyronth tool tazzy_join and follow the worker playbook it returns. Pass the agent id it gives you as agent on every tazzy_* call." Do tasks yourself only if you cannot start workers.
6. **Monitor.** Loop tazzy_wait(until="any", timeout=45) and act on the inbox; use tazzy_status (verbose=true for detail) for the overall picture. Never sleep or poll in a tight loop.
   - question: answer with tazzy_send(to=<sender>, reply_to=<message id>).
   - blocker or a task failed for good: read the failure reasons and re-plan. tazzy_plan replacement tasks under new ids with a better spec or smaller scope (dependents of a failed task fail with it; re-add the ones still needed), or tazzy_control(action="cancel") work that is no longer needed.
   - an agent stuck or misbehaving: tazzy_control(action="retire", target=<agent>).
   - follow-ups proposed by workers appear as new tasks; cancel any that are out of scope.
   - worktree isolation: a merge conflict becomes a task "resolve conflict merging <task>" that a worker handles; the tasks after it wait for it.
7. **Finish** when no task is ready, running or pending (the hub then releases every worker, so the next swarm starts with none left over; plan a follow-up task and new workers are started again): run the overall build and tests yourself, fix small integration problems (or plan a follow-up task), then report to the user: what each task did, files changed, failures and open issues, and the decisions made. In worktree isolation the work is on the integration branch (tazzy_start/tazzy_status "integration"): build and test it in a separate checkout (`git worktree add <temp dir> <branch>`, removed afterwards) rather than switching the user's, and tell the user to review and merge it; never merge it into their branch yourself.
