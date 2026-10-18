package activity

import (
	"encoding/json"
	"fmt"
	"strings"
)

// geminiDecoder reads `gemini --output-format stream-json`. UNVERIFIED:
// written from the documented event shapes, not checked against a real
// gemini (not installed):
//
//	{"type":"init","session_id":..,"model":..}
//	{"type":"message","role":"assistant","content":"..","delta":true}
//	{"type":"tool_use","tool_name":"read_file","tool_id":..,"parameters":{..}}
//	{"type":"tool_result","tool_id":..,"status":"success"|"error","output":..,"error":{"message":..}}
//	{"type":"error","severity":..,"message":..}
//	{"type":"result","status":"success","stats":{"input_tokens":..,"output_tokens":..,"total_tokens":..}}
//
// Assistant text arrives in deltas; it is buffered and emitted as one Say
// when anything else happens. gemini reports no cost.
type geminiDecoder struct {
	calls map[string]toolCall
	text  strings.Builder
}

func (d *geminiDecoder) line(b []byte, emit func(Activity)) {
	var ev struct {
		Type       string          `json:"type"`
		Role       string          `json:"role"`
		Content    string          `json:"content"`
		ToolName   string          `json:"tool_name"`
		ToolID     string          `json:"tool_id"`
		Parameters json.RawMessage `json:"parameters"`
		Status     string          `json:"status"`
		Output     json.RawMessage `json:"output"`
		Error      json.RawMessage `json:"error"`
		Message    string          `json:"message"`
		Stats      *struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
			Cached int64 `json:"cached"`
		} `json:"stats"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	if ev.Type == "message" {
		if ev.Role == "assistant" {
			d.text.WriteString(ev.Content)
		}
		return
	}
	d.flush(emit)
	switch ev.Type {
	case "tool_use":
		a := ToolActivity(ev.ToolName, ev.Parameters)
		d.calls[ev.ToolID] = toolCall{name: ev.ToolName, kind: a.Kind, summary: a.Summary}
		if a.Kind != "" {
			emit(a)
		}
	case "tool_result":
		call, ok := d.calls[ev.ToolID]
		if !ok {
			call = toolCall{name: "tool", kind: Tool}
		}
		delete(d.calls, ev.ToolID)
		out := text(ev.Output)
		failed := ev.Status == "error"
		if failed && len(ev.Error) > 0 {
			out = text(ev.Error)
		}
		if a, ok := resultActivity(call, failed, out); ok {
			emit(a)
		}
	case "error":
		emit(Activity{Kind: Alert, Summary: "error: " + ev.Message})
	case "result":
		a := Activity{Kind: State, Summary: "run " + ev.Status}
		if s := ev.Stats; s != nil {
			a.Usage = &Tokens{Input: s.Input - min(s.Cached, s.Input), CacheRead: s.Cached, Output: s.Output}
			a.Summary += fmt.Sprintf(", %d tokens", a.Usage.Total())
		}
		emit(a)
	}
}

func (d *geminiDecoder) flush(emit func(Activity)) {
	if d.text.Len() == 0 {
		return
	}
	t := d.text.String()
	d.text.Reset()
	dt, _ := json.Marshal(map[string]string{"text": t})
	emit(Activity{Kind: Say, Summary: t, Detail: dt})
}
