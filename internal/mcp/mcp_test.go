package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/hub"
	"tazzyronth/internal/proto"
)

// client drives a Server over pipes, the way an MCP host would.
type client struct {
	t    *testing.T
	w    *proto.Writer
	r    *proto.Reader
	next int
}

func newClient(t *testing.T, connect func(context.Context) (*hub.Client, error)) *client {
	t.Helper()
	return serverClient(t, New(connect, "test", nil))
}

func serverClient(t *testing.T, srv *Server) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	t.Cleanup(func() {
		inW.Close()
		go io.Copy(io.Discard, outR) // unblock any final writes
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return &client{t: t, w: proto.NewWriter(inW), r: proto.NewReader(outR)}
}

// request sends a request and returns its response. One call at a time per client.
func (c *client) request(method string, params any) *proto.Message {
	c.t.Helper()
	c.next++
	m, err := proto.NewRequest(c.next, method, params)
	if err != nil {
		c.t.Fatal(err)
	}
	if err := c.w.Write(m); err != nil {
		c.t.Fatal(err)
	}
	resp, err := c.r.Read()
	if err != nil {
		c.t.Fatalf("%s: read response: %v", method, err)
	}
	if string(resp.ID) != string(m.ID) {
		c.t.Fatalf("%s: response id %s, want %s", method, resp.ID, m.ID)
	}
	return resp
}

func (c *client) notify(method string) {
	m, _ := proto.NewNotification(method, nil)
	c.w.Write(m)
}

// call invokes a tool and returns the decoded first content block, the inbox
// block (nil if absent) and whether the tool reported an error.
func (c *client) call(name string, args any) (res map[string]any, inbox map[string]any, isErr bool) {
	c.t.Helper()
	resp := c.request("tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		c.t.Fatalf("%s: rpc error %v", name, resp.Error)
	}
	var r toolResult
	if err := json.Unmarshal(resp.Result, &r); err != nil || len(r.Content) == 0 {
		c.t.Fatalf("%s: bad result %s", name, resp.Result)
	}
	if r.IsError {
		res = map[string]any{"error": r.Content[0].Text}
	} else if err := json.Unmarshal([]byte(r.Content[0].Text), &res); err != nil {
		c.t.Fatalf("%s: result is not JSON: %s", name, r.Content[0].Text)
	}
	if len(r.Content) > 1 {
		var b struct{ Inbox map[string]any }
		json.Unmarshal([]byte(r.Content[1].Text), &b)
		inbox = b.Inbox
	}
	return res, inbox, r.IsError
}

func (c *client) mustCall(name string, args any) (map[string]any, map[string]any) {
	c.t.Helper()
	res, inbox, isErr := c.call(name, args)
	if isErr {
		c.t.Fatalf("%s(%v): %v", name, args, res["error"])
	}
	return res, inbox
}

func noHub(context.Context) (*hub.Client, error) { return nil, errors.New("no hub in this test") }

func TestProtocol(t *testing.T) {
	c := newClient(t, noHub)

	resp := c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "test", "version": "0"},
	})
	var init struct {
		ProtocolVersion string
		Capabilities    map[string]json.RawMessage
		ServerInfo      struct{ Name string }
	}
	if err := json.Unmarshal(resp.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2025-06-18" || init.Capabilities["tools"] == nil || init.ServerInfo.Name != "tazzyronth" {
		t.Fatalf("initialize: %s", resp.Result)
	}
	c.notify("notifications/initialized")

	// An unknown version gets our latest.
	resp = c.request("initialize", map[string]any{"protocolVersion": "1999-01-01"})
	json.Unmarshal(resp.Result, &init)
	if init.ProtocolVersion != protocolVersions[0] {
		t.Errorf("negotiated %q for an unknown version", init.ProtocolVersion)
	}

	if resp := c.request("ping", nil); resp.Error != nil || string(resp.Result) != "{}" {
		t.Errorf("ping: %+v", resp)
	}

	resp = c.request("tools/list", nil)
	var list struct {
		Tools []struct {
			Name, Description string
			InputSchema       struct {
				Type       string
				Properties map[string]any
			}
		}
	}
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != len(tools) || len(tools) < 13 {
		t.Fatalf("tools/list returned %d tools", len(list.Tools))
	}
	for _, tl := range list.Tools {
		if !strings.HasPrefix(tl.Name, "tazzy_") || tl.Description == "" || tl.InputSchema.Type != "object" {
			t.Errorf("bad tool %+v", tl)
		}
	}

	// Calls that need registration fail in-band, without touching the hub.
	if res, _, isErr := c.call("tazzy_next", map[string]any{}); !isErr || !strings.Contains(res["error"].(string), "tazzy_join") {
		t.Errorf("unregistered tazzy_next: %v", res)
	}
	// Hub failures are tool errors too.
	if res, _, isErr := c.call("tazzy_status", nil); !isErr || !strings.Contains(res["error"].(string), "hub unavailable") {
		t.Errorf("tazzy_status without hub: %v", res)
	}

	if resp := c.request("tools/call", map[string]any{"name": "nope"}); resp.Error == nil || resp.Error.Code != proto.CodeInvalidParams {
		t.Errorf("unknown tool: %+v", resp)
	}
	if resp := c.request("resources/list", nil); resp.Error == nil || resp.Error.Code != proto.CodeMethodNotFound {
		t.Errorf("unknown method: %+v", resp)
	}
}

// TestSwarm runs a lead and two workers through one real hub.
func TestSwarm(t *testing.T) {
	dir := t.TempDir()
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	connect := func(context.Context) (*hub.Client, error) { return hub.Connect(dir) }

	lead, w1, w2 := newClient(t, connect), newClient(t, connect), newClient(t, connect)
	for _, c := range []*client{lead, w1, w2} {
		c.request("initialize", map[string]any{"protocolVersion": "2025-06-18"})
		c.notify("notifications/initialized")
	}

	if res, _ := lead.mustCall("tazzy_start", map[string]any{"goal": "ship it"}); res["agent"] != "lead" {
		t.Fatalf("start: %v", res)
	}
	lead.mustCall("tazzy_plan", map[string]any{"tasks": []map[string]any{
		{"id": "schema", "spec": "write the schema"},
		{"id": "api", "deps": []string{"schema"}},
	}})
	r1, _ := w1.mustCall("tazzy_join", map[string]any{"name": "one"})
	r2, _ := w2.mustCall("tazzy_join", map[string]any{"name": "two"})
	if r1["goal"] != "ship it" || r1["agent"] == r2["agent"] {
		t.Fatalf("join: %v / %v", r1, r2)
	}
	id1 := r1["agent"].(string)

	res, _ := w1.mustCall("tazzy_next", map[string]any{})
	if task, _ := res["task"].(map[string]any); task["id"] != "schema" {
		t.Fatalf("w1 next: %v", res)
	}
	if res, _ := w2.mustCall("tazzy_next", map[string]any{}); res["task"] != nil {
		t.Fatalf("w2 claimed %v while api is blocked", res)
	}

	// w2 blocks on the dep; it must not return until w1 finishes it.
	waited := make(chan map[string]any, 1)
	go func() {
		res, _, _ := w2.call("tazzy_wait", map[string]any{"until": "task:schema", "timeout": 10})
		waited <- res
	}()
	select {
	case res := <-waited:
		t.Fatalf("wait returned early: %v", res)
	case <-time.After(200 * time.Millisecond):
	}
	w1.mustCall("tazzy_progress", map[string]any{"task": "schema", "note": "half way", "files_touched": []string{"db.sql"}})
	w1.mustCall("tazzy_done", map[string]any{"task": "schema", "summary": "users table", "artifacts": map[string]string{"schema": "db.sql"}})
	select {
	case res := <-waited:
		if res["met"] != true || res["state"] != "done" {
			t.Fatalf("wait: %v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tazzy_wait did not unblock")
	}

	res, _ = w2.mustCall("tazzy_next", map[string]any{"wait": 2})
	if task, _ := res["task"].(map[string]any); task["id"] != "api" || res["dep_results"] == nil {
		t.Fatalf("w2 next: %v", res)
	}

	// The lead blocks on messages; w2's question wakes it and arrives piggybacked.
	waited = make(chan map[string]any, 1)
	var leadInbox map[string]any
	go func() {
		res, inbox, _ := lead.call("tazzy_wait", map[string]any{"until": "msg", "timeout": 10})
		leadInbox = inbox
		waited <- res
	}()
	time.Sleep(100 * time.Millisecond)
	w2.mustCall("tazzy_send", map[string]any{"to": "role:lead", "kind": "question", "body": "REST or gRPC?"})
	select {
	case res := <-waited:
		msgs, _ := leadInbox["messages"].([]any)
		if res["met"] != true || len(msgs) != 1 || msgs[0].(map[string]any)["body"] != "REST or gRPC?" {
			t.Fatalf("lead wait: %v inbox %v", res, leadInbox)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lead's tazzy_wait(msg) did not unblock")
	}
	// Acked: not shown again.
	if _, inbox := lead.mustCall("tazzy_status", map[string]any{}); inbox != nil {
		t.Errorf("inbox repeated: %v", inbox)
	}

	// A message to task:api reaches its owner on the next call of any tool.
	w1.mustCall("tazzy_send", map[string]any{"to": "task:api", "body": "schema has an email column"})
	_, inbox := w2.mustCall("tazzy_board", map[string]any{"op": "get", "key": "goal"})
	if msgs, _ := inbox["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["from"] != id1 {
		t.Fatalf("w2 inbox: %v", inbox)
	}

	// Cancelling w2's task shows up as a notice.
	lead.mustCall("tazzy_control", map[string]any{"action": "cancel", "target": "api"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, inbox := w2.mustCall("tazzy_status", map[string]any{})
		if n, _ := inbox["notices"].([]any); len(n) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no cancellation notice")
		}
		time.Sleep(20 * time.Millisecond)
	}

	res, _ = lead.mustCall("tazzy_status", map[string]any{})
	counts := res["counts"].(map[string]any)
	if counts["done"] != 1.0 || counts["cancelled"] != 1.0 {
		t.Errorf("status counts: %v", counts)
	}
	if res, _, isErr := lead.call("tazzy_control", map[string]any{"action": "halt"}); !isErr {
		t.Errorf("halt should be an unknown action: %v", res)
	}
}

// TestReconnect checks that a shim survives a hub restart.
func TestReconnect(t *testing.T) {
	dir := t.TempDir()
	h, err := hub.Start(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, func(context.Context) (*hub.Client, error) { return hub.Connect(dir) })
	c.mustCall("tazzy_join", map[string]any{})
	h.Close()
	if h, err = hub.Start(dir); err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if res, _ := c.mustCall("tazzy_status", map[string]any{}); len(res["agents"].([]any)) != 1 {
		t.Errorf("after restart: %v", res)
	}
}

// fileURI makes a file:// URI for a local path, the way a client would.
func fileURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/x -> /C:/x
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

func TestRootsProjectDir(t *testing.T) {
	project := t.TempDir()
	srv := New(noHub, "test", nil)
	srv.UseRoots("fallback")
	c := serverClient(t, srv)

	c.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{"roots": map[string]any{"listChanged": true}},
	})
	c.notify("notifications/initialized")

	req, err := c.r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !req.IsRequest() || req.Method != "roots/list" {
		t.Fatalf("server sent %+v, want a roots/list request", req)
	}
	resp, _ := proto.NewResult(req.ID, map[string]any{"roots": []map[string]string{
		{"uri": "https://example.com/not-a-dir"},
		{"uri": fileURI(project), "name": "project"},
		{"uri": fileURI(t.TempDir())},
	}})
	c.w.Write(resp)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got, client := srv.DirSource(ctx); got != filepath.Clean(project) || !client {
		t.Errorf("DirSource = %q %v, want %q from the client", got, client, project)
	}
	// The server keeps serving afterwards.
	if resp := c.request("ping", nil); resp.Error != nil {
		t.Errorf("ping: %+v", resp)
	}
}

func TestRootsFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("no roots capability", func(t *testing.T) {
		srv := New(noHub, "test", nil)
		srv.UseRoots("fallback")
		c := serverClient(t, srv)
		c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
		c.notify("notifications/initialized")
		if got, client := srv.DirSource(ctx); got != "fallback" || client {
			t.Errorf("DirSource = %q %v, want the fallback", got, client)
		}
		if resp := c.request("ping", nil); resp.Error != nil { // no roots/list was sent before this
			t.Errorf("ping: %+v", resp)
		}
	})

	t.Run("roots/list error", func(t *testing.T) {
		srv := New(noHub, "test", nil)
		srv.UseRoots("fallback")
		c := serverClient(t, srv)
		c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"roots": map[string]any{}}})
		c.notify("notifications/initialized")
		req, err := c.r.Read()
		if err != nil || req.Method != "roots/list" {
			t.Fatalf("read %+v, %v; want roots/list", req, err)
		}
		c.w.Write(proto.NewError(req.ID, &proto.Error{Code: proto.CodeMethodNotFound, Message: "no roots"}))
		if got := srv.Dir(ctx); got != "fallback" {
			t.Errorf("Dir = %q, want the fallback", got)
		}
	})

	t.Run("without UseRoots", func(t *testing.T) {
		if got := New(noHub, "test", nil).Dir(ctx); got != "" {
			t.Errorf("Dir = %q, want empty", got)
		}
	})
}

// TestNoProjectError: a refused project dir reaches the model as a plain
// tool error with the guard's own text, not "hub unavailable".
func TestNoProjectError(t *testing.T) {
	c := newClient(t, func(context.Context) (*hub.Client, error) { return nil, &NoProjectError{Dir: `C:\app`} })
	c.request("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	for _, name := range []string{"tazzy_start", "tazzy_join"} {
		res, _, isErr := c.call(name, map[string]any{"goal": "g"})
		msg, _ := res["error"].(string)
		if !isErr || !strings.HasPrefix(msg, `no project folder: the client reported no workspace root and C:\app is not a project`) {
			t.Errorf("%s = %v (error %v)", name, res, isErr)
		}
	}
}

func TestFileURIPath(t *testing.T) {
	cases := []struct {
		uri, want string
		ok        bool
	}{
		{"file:///home/me/proj", "/home/me/proj", true},
		{"file://localhost/home/me/proj", "/home/me/proj", true},
		{"file:///home/me/my%20proj/", "/home/me/my proj", true},
		{"https://example.com/x", "", false},
		{"file://", "", false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases[:0],
			struct {
				uri, want string
				ok        bool
			}{"file:///C:/Users/me/proj", `C:\Users\me\proj`, true},
			struct {
				uri, want string
				ok        bool
			}{"file:///c%3A/Users/me/my%20proj", `c:\Users\me\my proj`, true},
			struct {
				uri, want string
				ok        bool
			}{"file://server/share/proj", `\\server\share\proj`, true},
			struct {
				uri, want string
				ok        bool
			}{"https://example.com/x", "", false},
		)
	}
	for _, tc := range cases {
		got, ok := fileURIPath(tc.uri)
		if got != tc.want || ok != tc.ok {
			t.Errorf("fileURIPath(%q) = %q, %v; want %q, %v", tc.uri, got, ok, tc.want, tc.ok)
		}
	}
}
