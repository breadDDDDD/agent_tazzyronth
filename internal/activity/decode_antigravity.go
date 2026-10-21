package activity

import (
	"encoding/json"
	"fmt"
	"strings"
)

// antigravityDecoder reads `agy -p --output-format stream-json` (Antigravity
// CLI). Verified against agy 1.4.2 runs and
// https://antigravity.google/docs/cli/headless/:
//
//	{"event":"init","conversation_id":..,"init":{"cwd":..,"tools":[..],"permission_mode":..}}
//	{"event":"step_update","step_update":{"step_index":1,"state":"ACTIVE","step_type":"agent_response","text_delta":".."}}
//	{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"agent_response","usage":{..}}}
//	{"event":"step_update","step_update":{"step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"run_command","tool_info":{"name":..,"parameters":{"CommandLine":..}}}}
//	{"event":"step_update","step_update":{"step_index":2,"state":"DONE"|"ERROR","step_type":"tool","tool_info":{..,"output":..,"error":{"type":..,"message":..}}}}
//	{"event":"result","result":{"status":"SUCCESS","response":..,"usage":{..},"error":..,"denied_actions":[{"action":"command",..}]}}
//
// usage is {input_tokens, output_tokens, thinking_tokens, cache_read_tokens,
// total_tokens}; total = input + output, so input includes the cache reads
// and output the thinking. Each agent_response step reports its own usage
// and the result the sum; the result only tops up what the steps missed.
// Text deltas are buffered and emitted as one Say.
type antigravityDecoder struct {
	calls map[int]toolCall
	text  strings.Builder
	seen  agyUsage // sum of the step usages emitted so far
}

type agyUsage struct {
	Input     int64 `json:"input_tokens"`
	Output    int64 `json:"output_tokens"`
	CacheRead int64 `json:"cache_read_tokens"`
}

func (u agyUsage) tokens() Tokens {
	return Tokens{Input: u.Input - min(u.CacheRead, u.Input), CacheRead: u.CacheRead, Output: u.Output}
}

type agyToolInfo struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
	Output     json.RawMessage `json:"output"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (d *antigravityDecoder) line(b []byte, emit func(Activity)) {
	var ev struct {
		Event string `json:"event"`
		Step  *struct {
			Index    int          `json:"step_index"`
			State    string       `json:"state"`
			Type     string       `json:"step_type"`
			ToolName string       `json:"tool_name"`
			Delta    string       `json:"text_delta"`
			Usage    *agyUsage    `json:"usage"`
			Tool     *agyToolInfo `json:"tool_info"`
		} `json:"step_update"`
		Result *struct {
			Status string          `json:"status"`
			Usage  *agyUsage       `json:"usage"`
			Error  json.RawMessage `json:"error"`
			Denied []struct {
				Action string `json:"action"`
			} `json:"denied_actions"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	switch {
	case ev.Event == "step_update" && ev.Step != nil:
		s := ev.Step
		switch s.Type {
		case "agent_response":
			d.text.WriteString(s.Delta)
			if s.State == "ACTIVE" {
				return
			}
			d.flush(emit)
			if u := s.Usage; u != nil {
				d.seen.Input += u.Input
				d.seen.Output += u.Output
				d.seen.CacheRead += u.CacheRead
				t := u.tokens()
				emit(Activity{Kind: Usage, Summary: fmt.Sprintf("%d tokens", t.Total()), Usage: &t})
			}
		case "tool":
			d.flush(emit)
			info := agyToolInfo{Name: s.ToolName}
			if s.Tool != nil {
				info = *s.Tool
				if info.Name == "" {
					info.Name = s.ToolName
				}
			}
			d.tool(s.Index, s.State, info, emit)
		}
	case ev.Event == "result" && ev.Result != nil:
		d.flush(emit)
		r := ev.Result
		if len(r.Error) > 0 && string(r.Error) != "null" {
			emit(Activity{Kind: Alert, Summary: "error: " + OneLine(text(r.Error), 200)})
		}
		if len(r.Denied) > 0 {
			acts := make([]string, 0, len(r.Denied))
			for _, x := range r.Denied {
				acts = append(acts, x.Action)
			}
			emit(Activity{Kind: Alert, Summary: "denied (headless, no allow rule): " + strings.Join(acts, ", ")})
		}
		a := Activity{Kind: State, Summary: "run " + strings.ToLower(r.Status)}
		if u := r.Usage; u != nil {
			rest := agyUsage{Input: max(u.Input-d.seen.Input, 0), Output: max(u.Output-d.seen.Output, 0), CacheRead: max(u.CacheRead-d.seen.CacheRead, 0)}
			if rest != (agyUsage{}) {
				t := rest.tokens()
				a.Usage = &t
			}
			a.Summary += fmt.Sprintf(", %d tokens", u.Input+u.Output)
		}
		emit(a)
	}
}

// tool handles a tool step: the call when it starts (ACTIVE, or a DONE/ERROR
// seen first), and the result when it ends.
func (d *antigravityDecoder) tool(idx int, state string, info agyToolInfo, emit func(Activity)) {
	call, started := d.calls[idx]
	if !started {
		a := agyToolActivity(info.Name, info.Parameters)
		call = toolCall{name: info.Name, kind: a.Kind, summary: a.Summary}
		d.calls[idx] = call
		if a.Kind != "" {
			emit(a)
		}
	}
	if state == "ACTIVE" {
		return
	}
	delete(d.calls, idx)
	failed := state == "ERROR" || info.Error != nil
	out := text(info.Output)
	if info.Error != nil {
		out = info.Error.Message
	}
	if a, ok := resultActivity(call, failed, out); ok {
		emit(a)
	}
}

// agyToolActivity maps an Antigravity tool call. Its parameters are
// PascalCase (Path, CommandLine, ...); call_mcp_tool names the server and
// tool, and tazzy tools are skipped (the hub sees those calls itself).
func agyToolActivity(name string, params json.RawMessage) Activity {
	var in map[string]any
	json.Unmarshal(params, &in)
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := in[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	a := Activity{Kind: Tool, Summary: name, Detail: params}
	file := str("Path", "AbsolutePath", "TargetFile", "FilePath", "File", "DirectoryPath")
	switch name {
	case "call_mcp_tool":
		tool := str("ToolName")
		if _, ok := tazzyTool(tool); ok {
			return Activity{}
		}
		a.Summary = "mcp " + str("ServerName") + "/" + tool
		return a
	case "read_file", "view_file", "list_dir", "view_file_outline", "view_code_item":
		a.Kind = Read
	case "write_file", "write_to_file", "replace_file_content", "multi_replace_file_content", "sed_file", "notebook_edit":
		a.Kind = Edit
	case "run_command", "send_command_input":
		a.Kind = Cmd
		if c := str("CommandLine", "Command", "Input"); c != "" {
			a.Summary = name + " " + c
		}
		return a
	case "grep_search", "find_by_name", "search_web", "read_url_content", "codebase_search":
		a.Kind = Search
		if q := str("Query", "Pattern", "SearchPattern", "Url", "URL"); q != "" {
			a.Summary = fmt.Sprintf("%s %q", name, q)
			if p := str("SearchPath", "SearchDirectory", "DirectoryPath", "Path"); p != "" {
				a.Summary += " in " + p
			}
			return a
		}
	}
	if file != "" {
		a.Files = []string{file}
		a.Summary = name + " " + file
	}
	return a
}

func (d *antigravityDecoder) flush(emit func(Activity)) {
	t := strings.TrimSpace(d.text.String())
	d.text.Reset()
	if t == "" {
		return
	}
	dt, _ := json.Marshal(map[string]string{"text": t})
	emit(Activity{Kind: Say, Summary: t, Detail: dt})
}
