package activity

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// decoder turns one JSON line of a worker's output stream into activity.
// Decoders are stateful (e.g. to pair tool results with their calls), must
// ignore unknown or malformed input, and are never called concurrently.
type decoder interface {
	line(b []byte, emit func(Activity))
	flush(emit func(Activity)) // end of stream
}

// Formats lists the stream formats Decode understands (config adapter "stream").
var Formats = map[string]func() decoder{
	"claude": func() decoder { return &claudeDecoder{calls: map[string]toolCall{}, msgs: map[string]bool{}} },
	"codex":  func() decoder { return &codexDecoder{} },
	"gemini": func() decoder { return &geminiDecoder{calls: map[string]toolCall{}} },
}

// Decode reads a worker's stdout in the given format until EOF and calls
// emit for each activity (Agent left empty). It always drains r, so the
// worker never blocks on a full pipe, and a bad line (or a decoder bug)
// costs only that line.
func Decode(format string, r io.Reader, emit func(Activity)) {
	mk := Formats[format]
	if mk == nil {
		io.Copy(io.Discard, r)
		return
	}
	d := mk()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		b, err := br.ReadBytes('\n')
		if b = bytes.TrimSpace(b); len(b) > 0 && b[0] == '{' {
			safe(func() { d.line(b, emit) })
		}
		if err != nil {
			break
		}
	}
	safe(func() { d.flush(emit) })
}

func safe(f func()) {
	defer func() { recover() }()
	f()
}

// toolCall is a tool invocation awaiting its result.
type toolCall struct {
	name    string
	kind    string
	summary string
}

// ToolActivity maps a Claude Code tool call (stream-json tool_use or a hook's
// tool_name/tool_input) to an activity. Unknown tools become Tool; tazzy
// tools give Kind "" (nothing to record: the hub sees those calls itself).
func ToolActivity(name string, input json.RawMessage) Activity {
	if _, ok := tazzyTool(name); ok {
		return Activity{}
	}
	var in map[string]any
	json.Unmarshal(input, &in)
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := in[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	a := Activity{Kind: Tool, Summary: name, Detail: input}
	file := str("file_path", "notebook_path", "path", "absolute_path")
	switch name {
	case "Read", "NotebookRead", "read_file", "read_many_files", "list_directory", "LS":
		a.Kind = Read
	case "Edit", "MultiEdit", "Write", "NotebookEdit", "write_file", "replace":
		a.Kind = Edit
	case "Bash", "BashOutput", "run_shell_command":
		a.Kind = Cmd
		if c := str("command"); c != "" {
			a.Summary = name + " " + c
		}
		return a
	case "Grep", "Glob", "WebSearch", "WebFetch", "glob", "search_file_content", "google_web_search", "web_fetch":
		a.Kind = Search
		if q := str("pattern", "query", "url"); q != "" {
			a.Summary = fmt.Sprintf("%s %q", name, q)
			if file != "" {
				a.Summary += " in " + file
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

// tazzyTool returns "tazzy_x" for an MCP tool name like
// "mcp__tazzyronth__tazzy_x" (any server name), or the bare name.
func tazzyTool(name string) (string, bool) {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	return name, strings.HasPrefix(name, "tazzy_")
}

// resultActivity is the activity for a tool result: always for commands,
// otherwise only for failures (the call itself is already in the feed).
func resultActivity(c toolCall, isError bool, output string) (Activity, bool) {
	if c.kind == "" || c.kind != Cmd && !isError {
		return Activity{}, false
	}
	status := "✓"
	if isError {
		status = "✖"
	}
	head := c.summary
	if head == "" {
		head = c.name
	}
	sum := status + " " + head
	if first := OneLine(output, 80); first != "" {
		sum += ": " + first
	}
	d, _ := json.Marshal(map[string]any{"tool": c.name, "is_error": isError, "output": output})
	return Activity{Kind: CmdResult, Summary: sum, Detail: d}, true
}

// text flattens a tool result (a string, a list of blocks with "text", or an
// object with stdout/output/content) into one string.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		for _, k := range []string{"stdout", "output", "content", "text", "message"} {
			if s, ok := obj[k].(string); ok {
				if e, ok := obj["stderr"].(string); ok && e != "" {
					s += "\n" + e
				}
				return s
			}
		}
	}
	return string(raw)
}
