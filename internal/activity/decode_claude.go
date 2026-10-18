package activity

import (
	"encoding/json"
	"fmt"
)

// claudeDecoder reads `claude -p --output-format stream-json --verbose`:
//
//	{"type":"system","subtype":"init",...}
//	{"type":"assistant","message":{"id":..,"content":[{"type":"text","text":..}|{"type":"tool_use","id":..,"name":..,"input":{..}}],"usage":{..}}}
//	{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":..,"content":..,"is_error":bool}]}}
//	{"type":"result","subtype":"success","total_cost_usd":..,"usage":{..},"num_turns":..}
//
// Usage: an assistant message can arrive as several events sharing one
// message id and usage, so each id is counted once. The final result carries
// the session totals; whatever exceeds what was already counted is added then.
type claudeDecoder struct {
	calls map[string]toolCall // tool_use id -> call
	msgs  map[string]bool     // assistant message ids whose usage was counted
	seen  Tokens              // counted so far
}

type claudeUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

func (u *claudeUsage) tokens() Tokens {
	if u == nil {
		return Tokens{}
	}
	return Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
}

func (d *claudeDecoder) line(b []byte, emit func(Activity)) {
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		Message struct {
			ID      string `json:"id"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
				IsError   bool            `json:"is_error"`
			} `json:"content"`
			Usage *claudeUsage `json:"usage"`
		} `json:"message"`
		// result
		IsError  bool         `json:"is_error"`
		Result   string       `json:"result"`
		CostUSD  *float64     `json:"total_cost_usd"`
		NumTurns int          `json:"num_turns"`
		Usage    *claudeUsage `json:"usage"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	switch ev.Type {
	case "assistant":
		var out []Activity
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					dt, _ := json.Marshal(map[string]string{"text": c.Text})
					out = append(out, Activity{Kind: Say, Summary: c.Text, Detail: dt})
				}
			case "tool_use":
				a := ToolActivity(c.Name, c.Input)
				d.calls[c.ID] = toolCall{name: c.Name, kind: a.Kind, summary: a.Summary}
				if a.Kind != "" {
					out = append(out, a)
				}
			}
		}
		if u := ev.Message.Usage; u != nil && !d.msgs[ev.Message.ID] {
			if ev.Message.ID != "" {
				d.msgs[ev.Message.ID] = true
			}
			t := u.tokens()
			if t.Total() > 0 {
				d.seen.add(t)
				if len(out) == 0 {
					out = append(out, Activity{Kind: Usage, Summary: fmt.Sprintf("%d tokens", t.Total())})
				}
				out[0].Usage = &t
			}
		}
		for _, a := range out {
			emit(a)
		}
	case "user":
		for _, c := range ev.Message.Content {
			if c.Type != "tool_result" {
				continue
			}
			call, ok := d.calls[c.ToolUseID]
			if !ok {
				call = toolCall{name: "tool", kind: Tool}
			}
			delete(d.calls, c.ToolUseID)
			if a, ok := resultActivity(call, c.IsError, text(c.Content)); ok {
				emit(a)
			}
		}
	case "result":
		var t Tokens
		if total := ev.Usage.tokens(); total.Total() > d.seen.Total() {
			t = Tokens{Input: max(0, total.Input-d.seen.Input), Output: max(0, total.Output-d.seen.Output),
				CacheRead: max(0, total.CacheRead-d.seen.CacheRead), CacheWrite: max(0, total.CacheWrite-d.seen.CacheWrite)}
		}
		if ev.CostUSD != nil && *ev.CostUSD > d.seen.CostUSD {
			t.CostUSD = *ev.CostUSD - d.seen.CostUSD
		}
		d.seen.add(t)
		status := "finished"
		if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
			status = "ended with " + ev.Subtype
		}
		sum := fmt.Sprintf("run %s after %d turns", status, ev.NumTurns)
		if ev.CostUSD != nil {
			sum += fmt.Sprintf(", $%.2f", *ev.CostUSD)
		}
		dt, _ := json.Marshal(map[string]any{"result": ev.Result, "subtype": ev.Subtype})
		a := Activity{Kind: State, Summary: sum, Detail: dt}
		if t != (Tokens{}) {
			a.Usage = &t
		}
		emit(a)
	}
}

func (d *claudeDecoder) flush(func(Activity)) {}
