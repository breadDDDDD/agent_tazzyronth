---
description: Lead an Agent Tazzyronth swarm for a goal (plan a task DAG, run parallel worker subagents)
argument-hint: <goal> [--max N]
---

Lead an Agent Tazzyronth swarm for this goal: $ARGUMENTS

An optional `--max N` in it is the maximum number of workers; the rest is the goal.

1. Call the tazzyronth MCP tool `tazzy_start` with `goal` set to the goal (and `max_agents` set to N if given).
2. Follow the lead playbook in its result, passing `agent="lead"` on every later tazzy_* call. In spawn mode "host", start each worker with the Agent tool (in the background), prompted to call `tazzy_join`, follow the worker playbook it returns, and pass the agent id it gets as `agent` on every tazzy_* call.

If no tazzy_* tools are available, the `tazzy` binary is probably not on PATH: tell the user to install it (`go install ./cmd/tazzy` in the Tazzyronth repo) and restart this session.
