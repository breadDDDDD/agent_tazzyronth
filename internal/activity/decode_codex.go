package activity

import (
	"encoding/json"
	"fmt"
	"strings"
)

// codexDecoder reads `codex exec --json`. UNVERIFIED: written from the
// documented event shapes, not checked against a real codex (not installed):
//
//	{"type":"item.started"|"item.completed","item":{"id":..,"type":"command_execution","command":..,"aggregated_output":..,"exit_code":N,"status":..}}
//	item types: agent_message{text}, reasoning{text}, command_execution, file_change{changes:[{path,kind}]},
//	            mcp_tool_call{server,tool,arguments,result,error,status}, web_search{query}
//	{"type":"turn.completed","usage":{"input_tokens":..,"cached_input_tokens":..,"output_tokens":..}}
//	{"type":"turn.failed","error":{"message":..}}, {"type":"error","message":..}
//
// Calls are reported on item.started (or item.completed if it never
// started), results on item.completed. codex reports no cost.
type codexDecoder struct {
	started map[string]bool
}

func (d *codexDecoder) line(b []byte, emit func(Activity)) {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			ID        string                        `json:"id"`
			Type      string                        `json:"type"`
			Text      string                        `json:"text"`
			Command   string                        `json:"command"`
			Output    string                        `json:"aggregated_output"`
			ExitCode  *int                          `json:"exit_code"`
			Status    string                        `json:"status"`
			Changes   []struct{ Path, Kind string } `json:"changes"`
			Server    string                        `json:"server"`
			Tool      string                        `json:"tool"`
			Arguments json.RawMessage               `json:"arguments"`
			Result    json.RawMessage               `json:"result"`
			Error     json.RawMessage               `json:"error"`
			Query     string                        `json:"query"`
		} `json:"item"`
		Usage *struct {
			Input  int64 `json:"input_tokens"`
			Cached int64 `json:"cached_input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	if d.started == nil {
		d.started = map[string]bool{}
	}
	it := ev.Item
	switch ev.Type {
	case "item.started", "item.updated", "item.completed":
		completed := ev.Type == "item.completed"
		first := !d.started[it.ID]
		if ev.Type != "item.updated" {
			d.started[it.ID] = true
		}
		switch it.Type {
		case "agent_message":
			if completed && it.Text != "" {
				dt, _ := json.Marshal(map[string]string{"text": it.Text})
				emit(Activity{Kind: Say, Summary: it.Text, Detail: dt})
			}
		case "command_execution":
			call := toolCall{name: "shell", kind: Cmd, summary: "shell " + it.Command}
			if first && ev.Type != "item.updated" {
				dt, _ := json.Marshal(map[string]string{"command": it.Command})
				emit(Activity{Kind: Cmd, Summary: call.summary, Detail: dt})
			}
			if completed {
				failed := it.Status == "failed" || it.ExitCode != nil && *it.ExitCode != 0
				out := it.Output
				if it.ExitCode != nil {
					out = fmt.Sprintf("exit %d\n%s", *it.ExitCode, out)
				}
				if a, ok := resultActivity(call, failed, out); ok {
					emit(a)
				}
			}
		case "file_change":
			if completed {
				var files, parts []string
				for _, c := range it.Changes {
					files = append(files, c.Path)
					parts = append(parts, c.Kind+" "+c.Path)
				}
				dt, _ := json.Marshal(it.Changes)
				emit(Activity{Kind: Edit, Summary: "patch " + strings.Join(parts, ", "), Files: files, Detail: dt})
			}
		case "mcp_tool_call":
			name := it.Server + "." + it.Tool
			a := Activity{Kind: Tool, Summary: name, Detail: it.Arguments}
			if _, ok := tazzyTool(it.Tool); ok {
				break // recorded by the hub itself
			}
			if first && ev.Type != "item.updated" {
				emit(a)
			}
			if completed && (it.Status == "failed" || len(it.Error) > 0 && string(it.Error) != "null") {
				if r, ok := resultActivity(toolCall{name: name, kind: a.Kind, summary: a.Summary}, true, text(it.Error)); ok {
					emit(r)
				}
			}
		case "web_search":
			if first && ev.Type != "item.updated" {
				emit(Activity{Kind: Search, Summary: fmt.Sprintf("web_search %q", it.Query)})
			}
		}
	case "turn.completed":
		if u := ev.Usage; u != nil {
			// cached_input_tokens is a subset of input_tokens.
			t := Tokens{Input: u.Input - u.Cached, CacheRead: u.Cached, Output: u.Output}
			emit(Activity{Kind: Usage, Summary: fmt.Sprintf("turn finished, %d tokens", t.Total()), Usage: &t})
		}
	case "turn.failed":
		emit(Activity{Kind: Alert, Summary: "turn failed: " + ev.Error.Message})
	case "error":
		emit(Activity{Kind: Alert, Summary: "error: " + ev.Message})
	}
}

func (d *codexDecoder) flush(func(Activity)) {}
