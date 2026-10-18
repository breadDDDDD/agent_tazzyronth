package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"tazzyronth/internal/proto"
	"tazzyronth/playbooks"
)

type promptArg struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required,omitempty"`
}

type prompt struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Arguments   []promptArg `json:"arguments,omitempty"`
}

// prompts are the MCP prompts (plan §7.4); hosts that support them show
// "tazzy" as a slash command.
var prompts = []prompt{
	{
		Name:        "tazzy",
		Description: "Lead an Agent Tazzyronth swarm for a goal",
		Arguments: []promptArg{
			{Name: "goal", Description: "what the swarm should achieve", Required: true},
			{Name: "max", Description: "maximum number of worker agents"},
		},
	},
	{Name: "tazzy-worker", Description: "Work as an Agent Tazzyronth swarm worker"},
}

func getPrompt(raw json.RawMessage) (any, error) {
	var p struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.Error{Code: proto.CodeInvalidParams, Message: err.Error()}
	}
	var desc, text string
	switch p.Name {
	case "tazzy":
		goal := strings.TrimSpace(p.Arguments["goal"])
		if goal == "" {
			return nil, &proto.Error{Code: proto.CodeInvalidParams, Message: "argument goal is required"}
		}
		start := fmt.Sprintf("tazzy_start(goal=%q", goal)
		if m := strings.TrimSpace(p.Arguments["max"]); m != "" {
			n, err := strconv.Atoi(m)
			if err != nil || n < 1 {
				return nil, &proto.Error{Code: proto.CodeInvalidParams, Message: "argument max must be a positive number"}
			}
			start += fmt.Sprintf(", max_agents=%d", n)
		}
		desc = prompts[0].Description
		text = playbooks.Lead + "\n## Your goal\n\n" + goal + "\n\nBegin with " + start + ").\n"
	case "tazzy-worker":
		desc, text = prompts[1].Description, playbooks.Worker
	default:
		return nil, &proto.Error{Code: proto.CodeInvalidParams, Message: "unknown prompt " + p.Name}
	}
	return map[string]any{
		"description": desc,
		"messages": []map[string]any{{
			"role":    "user",
			"content": map[string]string{"type": "text", "text": text},
		}},
	}, nil
}
