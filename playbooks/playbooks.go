// Package playbooks embeds the prompts that tell LLM agents how to take part
// in a swarm (plan §7.4). They are served as MCP prompts, returned by
// tazzy_start/tazzy_join and used as hub-spawned workers' bootstrap prompt.
package playbooks

import _ "embed"

var (
	// Lead is for the session that plans and coordinates (tazzy_start).
	//go:embed lead.md
	Lead string

	// Worker is for every agent that claims and does tasks (tazzy_join).
	//go:embed worker.md
	Worker string
)
