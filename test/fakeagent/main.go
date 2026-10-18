// Command fakeagent stands in for an LLM worker CLI in tests. Like a real
// adapter it is handed an MCP config file, starts the server named there
// ("tazzy mcp ...") and drives it over stdio the way the bootstrap prompt
// tells an LLM to: join, then loop next -> lease -> progress -> done.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tazzyronth/internal/proto"
)

func main() {
	mcpConfig := flag.String("mcp", "", "MCP config file (mcpServers.tazzyronth)")
	work := flag.Duration("work", 200*time.Millisecond, "time spent on each task")
	dieOn := flag.String("die-on", "", "exit with status 3 after leasing this task's files (once per project)")
	hangOn := flag.String("hang-on", "", "go silent forever after leasing this task's files (once per project)")
	flag.BoolVar(&claudeStream, "claude-stream", false, "print claude-style stream-json on stdout and edit each leased file")
	flag.Parse()
	log.SetPrefix("fakeagent: ")
	if err := run(*mcpConfig, *work, *dieOn, *hangOn); err != nil {
		log.Fatal(err)
	}
}

type client struct {
	w    *proto.Writer
	r    *proto.Reader
	next int
}

func (c *client) request(method string, params any) (json.RawMessage, error) {
	c.next++
	m, err := proto.NewRequest(c.next, method, params)
	if err != nil {
		return nil, err
	}
	if err := c.w.Write(m); err != nil {
		return nil, err
	}
	for {
		resp, err := c.r.Read()
		if err != nil {
			return nil, err
		}
		if !resp.IsResponse() || string(resp.ID) != string(m.ID) {
			continue
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// tool calls a tool and decodes its first content block.
func (c *client) tool(name string, args map[string]any) (map[string]any, error) {
	raw, err := c.request("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	var res struct {
		Content []struct{ Text string }
		IsError bool
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Content) == 0 {
		return nil, fmt.Errorf("%s: bad result %s", name, raw)
	}
	if res.IsError {
		return nil, fmt.Errorf("%s: %s", name, res.Content[0].Text)
	}
	var out map[string]any
	err = json.Unmarshal([]byte(res.Content[0].Text), &out)
	return out, err
}

func run(mcpConfig string, work time.Duration, dieOn, hangOn string) error {
	b, err := os.ReadFile(mcpConfig)
	if err != nil {
		return err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	srv, ok := cfg.MCPServers["tazzyronth"]
	if !ok {
		return errors.New("no tazzyronth server in MCP config")
	}
	cmd := exec.Command(srv.Command, srv.Args...)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()
	defer in.Close()
	c := &client{w: proto.NewWriter(in), r: proto.NewReader(out)}

	if _, err := c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "fakeagent", "version": "0"},
	}); err != nil {
		return err
	}
	if m, err := proto.NewNotification("notifications/initialized", nil); err == nil {
		c.w.Write(m)
	}
	j, err := c.tool("tazzy_join", map[string]any{"name": "fake"})
	if err != nil {
		return err
	}
	me := j["agent"].(string)
	log.Printf("joined as %s", me)
	call := func(name string, args map[string]any) (map[string]any, error) {
		args["agent"] = me
		return c.tool(name, args)
	}

	for {
		res, err := call("tazzy_next", map[string]any{"wait": 5})
		if err != nil {
			return err
		}
		task, _ := res["task"].(map[string]any)
		if task == nil {
			continue
		}
		id := task["id"].(string)
		var files []any
		if f, ok := task["files"].([]any); ok {
			files = f
		}
		log.Printf("claimed %s", id)
		for len(files) > 0 {
			l, err := call("tazzy_lease", map[string]any{"paths": files})
			if err != nil {
				return err
			}
			if l["granted"] == true {
				break
			}
			if _, err := call("tazzy_wait", map[string]any{"until": "lease:" + files[0].(string), "timeout": 5}); err != nil {
				return err
			}
		}
		if id == dieOn && first("died", id) {
			log.Printf("dying on %s", id)
			os.Exit(3)
		}
		if id == hangOn && first("hung", id) {
			log.Printf("hanging on %s", id)
			time.Sleep(24 * time.Hour) // (select{} would trip the deadlock detector)
		}
		if claudeStream {
			if err := fakeWork(me, id, files); err != nil {
				return err
			}
		}
		if _, err := call("tazzy_progress", map[string]any{"task": id, "note": "working"}); err != nil {
			return err
		}
		time.Sleep(work)
		if _, err := call("tazzy_done", map[string]any{"task": id, "summary": "done by " + me, "files": files}); err != nil {
			return err
		}
	}
}

// first reports whether no fake worker has done what (died, hung) on task
// yet, recording that this one will.
func first(what, task string) bool {
	f, err := os.OpenFile(filepath.Join(".tazzyronth", "fakeagent-"+what+"-"+task), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

var (
	claudeStream bool
	streamSeq    int
	costSoFar    float64
)

// say prints one claude stream-json event.
func say(v map[string]any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func toolUse(name string, input map[string]any) string {
	streamSeq++
	id := fmt.Sprintf("toolu_%d", streamSeq)
	say(map[string]any{"type": "assistant", "message": map[string]any{
		"id": fmt.Sprintf("msg_%d", streamSeq), "role": "assistant",
		"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}},
		"usage":   map[string]any{"input_tokens": 100, "output_tokens": 10},
	}})
	return id
}

func toolResult(id, out string, isError bool) {
	say(map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": out, "is_error": isError}}}})
}

// fakeWork is what a claude worker would do on a task, as stream-json: read
// and edit each leased file (really editing it), run a failing test command,
// say something and finish the turn.
func fakeWork(me, task string, files []any) error {
	if streamSeq == 0 {
		say(map[string]any{"type": "system", "subtype": "init", "session_id": "fake-" + me, "tools": []string{"Read", "Edit", "Bash"}})
	}
	wd, _ := os.Getwd()
	for _, f := range files {
		path := filepath.Join(wd, filepath.FromSlash(f.(string)))
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		toolResult(toolUse("Read", map[string]any{"file_path": path}), string(b), false)
		old := string(b)
		lines := strings.SplitAfterN(old, "\n", 2)
		added := lines[0] + "// edited by " + me + "\n"
		edited := added
		if len(lines) > 1 {
			edited += lines[1]
		}
		id := toolUse("Edit", map[string]any{"file_path": path, "old_string": lines[0], "new_string": added})
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			return err
		}
		toolResult(id, "The file "+path+" has been updated.", false)
	}
	toolResult(toolUse("Bash", map[string]any{"command": "go test ./..."}), "--- FAIL: TestX\nexit status 1", true)
	streamSeq++
	say(map[string]any{"type": "assistant", "message": map[string]any{"id": fmt.Sprintf("msg_%d", streamSeq), "role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": "Edited " + fmt.Sprint(len(files)) + " file(s) for " + task}},
		"usage":   map[string]any{"input_tokens": 50, "output_tokens": 5}}})
	costSoFar += 0.25
	say(map[string]any{"type": "result", "subtype": "success", "num_turns": streamSeq, "total_cost_usd": costSoFar, "result": "done"})
	return nil
}
