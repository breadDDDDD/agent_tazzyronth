---
name: tazzy
description: Lead an Agent Tazzyronth swarm - plan a goal as a task DAG and coordinate parallel worker agents through the tazzyronth MCP server. Use only when the user invokes /tazzy or $tazzy or explicitly asks for a Tazzyronth swarm.
---

Lead an Agent Tazzyronth swarm for the goal the user gave with this command (the text after /tazzy or $tazzy; an optional `--max N` is the maximum number of workers).

1. Call the tazzyronth MCP tool `tazzy_start` with `goal` set to that goal (and `max_agents` set to N if given).
2. Follow the lead playbook in its result, passing `agent="lead"` on every later tazzy_* call.

If no tazzy_* tools are available, tell the user to run `tazzy init` and restart this session.
