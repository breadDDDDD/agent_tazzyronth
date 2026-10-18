package activity

import (
	"encoding/json"
	"strings"
)

// Hook is what a Claude Code hook payload says.
type Hook struct {
	// Key is the host-side identity: the subagent's agent_id if present,
	// else the session_id.
	Key string
	// Agent is the Tazzyronth agent id the payload reveals, if any: the
	// "agent" argument of a tazzy tool call, or the id in tazzy_join's or
	// tazzy_start's result. The hub binds Key to it.
	Agent string
	// Activity to record; Kind is "" when there is nothing worth recording.
	Activity Activity
}

// FromHook decodes a Claude Code hook payload (PreToolUse, PostToolUse,
// PostToolUseFailure, SubagentStart, SubagentStop). Assumed fields:
// session_id, hook_event_name, tool_name, tool_input, tool_response, error,
// and agent_id/agent_type when the event comes from a subagent. Unknown
// events and fields are ignored.
func FromHook(payload []byte) Hook {
	var p struct {
		Session   string          `json:"session_id"`
		Event     string          `json:"hook_event_name"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		Response  json.RawMessage `json:"tool_response"`
		Error     json.RawMessage `json:"error"`
		AgentID   string          `json:"agent_id"`
		AgentType string          `json:"agent_type"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return Hook{}
	}
	h := Hook{Key: p.Session}
	if p.AgentID != "" {
		h.Key = p.AgentID
	}
	tool, isTazzy := tazzyTool(p.ToolName)
	if isTazzy {
		var in struct {
			Agent string `json:"agent"`
		}
		json.Unmarshal(p.ToolInput, &in)
		h.Agent = in.Agent
		if h.Agent == "" && (tool == "tazzy_join" || tool == "tazzy_start") {
			h.Agent = findAgent(p.Response, 0)
		}
	}
	switch p.Event {
	case "PreToolUse":
		h.Activity = ToolActivity(p.ToolName, p.ToolInput)
	case "PostToolUse", "PostToolUseFailure":
		a := ToolActivity(p.ToolName, p.ToolInput)
		failed := p.Event == "PostToolUseFailure" || hookFailed(p.Response)
		out := text(p.Response)
		if len(p.Error) > 0 {
			out = text(p.Error)
		}
		if r, ok := resultActivity(toolCall{name: p.ToolName, kind: a.Kind, summary: a.Summary}, failed, out); ok {
			h.Activity = r
		}
	case "SubagentStart", "SubagentStop":
		verb := "started"
		if p.Event == "SubagentStop" {
			verb = "stopped"
		}
		h.Activity = Activity{Kind: State, Summary: strings.TrimSpace("subagent " + p.AgentType + " " + verb)}
	}
	if h.Activity.Kind != "" {
		h.Activity.Source = SrcHook
	}
	return h
}

// hookFailed spots error markers in a tool_response object.
func hookFailed(raw json.RawMessage) bool {
	var r struct {
		IsError     bool `json:"is_error"`
		IsErrorAlt  bool `json:"isError"`
		Interrupted bool `json:"interrupted"`
		Success     *bool
	}
	if json.Unmarshal(raw, &r) != nil {
		return false
	}
	return r.IsError || r.IsErrorAlt || r.Interrupted || r.Success != nil && !*r.Success
}

// findAgent looks for an "agent" string in v: directly, in nested values, or
// inside strings that are themselves JSON (MCP text content blocks).
func findAgent(raw json.RawMessage, depth int) string {
	if depth > 5 || len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	switch v := v.(type) {
	case map[string]any:
		if s, ok := v["agent"].(string); ok && s != "" {
			return s
		}
		for _, k := range []string{"content", "text", "structuredContent", "result"} {
			if x, ok := v[k]; ok {
				b, _ := json.Marshal(x)
				if s := findAgent(b, depth+1); s != "" {
					return s
				}
			}
		}
	case []any:
		for _, x := range v {
			b, _ := json.Marshal(x)
			if s := findAgent(b, depth+1); s != "" {
				return s
			}
		}
	case string:
		if strings.HasPrefix(strings.TrimSpace(v), "{") || strings.HasPrefix(strings.TrimSpace(v), "[") {
			return findAgent(json.RawMessage(v), depth+1)
		}
	}
	return ""
}
