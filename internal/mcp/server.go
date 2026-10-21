// Package mcp is the stdio MCP server behind "tazzy mcp". It speaks MCP
// (JSON-RPC 2.0, one message per line) on stdin/stdout and turns tool calls
// into hub RPCs. Apart from the caller's agent id it holds no state.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"slices"
	"sync"

	"tazzyronth/internal/hub"
	"tazzyronth/internal/proto"
)

// Protocol versions we can speak, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const instructions = "Agent Tazzyronth coordinates a swarm. Lead: tazzy_start, tazzy_plan, then watch with tazzy_status/tazzy_wait. " +
	"Worker: tazzy_join, then loop tazzy_next -> tazzy_lease the files -> work (tazzy_progress) -> tazzy_done or tazzy_fail. " +
	"Pass your agent id as agent on every call after joining. " +
	"Use tazzy_wait instead of polling. Every result may carry an inbox of messages for you."

type Server struct {
	version string
	log     *log.Logger
	s       *session
	roots   *roots // set by UseRoots
}

// New returns a server that reaches the hub through connect. connect is
// called lazily, on the first tool call that needs the hub, and again if the
// connection drops. logger may be nil.
func New(connect func(context.Context) (*hub.Client, error), version string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Server{version: version, log: logger, s: newSession(connect, logger)}
}

// SetAgent presets the default identity: a hub-spawned worker's shim is
// started with the id the hub pre-registered, and tazzy_join keeps it.
func (srv *Server) SetAgent(id string) {
	srv.s.mu.Lock()
	defer srv.s.mu.Unlock()
	srv.s.agent, srv.s.preset = id, id
}

// SetLauncher sets what tazzy_start runs (once per shim) to open the
// dashboard. f must not block; it returns a sentence for the tool result.
func (srv *Server) SetLauncher(f func() string) { srv.s.launch = f }

// Serve handles messages from r until EOF, writing responses to w. Each tool
// call runs on its own goroutine: one shim may serve several agents (host
// subagents share it), so one agent's blocking tazzy_wait must not stall the
// others, nor pings and cancellation.
func (srv *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	in, out := proto.NewReader(r), proto.NewWriter(w)
	if srv.roots != nil {
		srv.roots.mu.Lock()
		srv.roots.out = out
		srv.roots.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(ctx)
	var (
		mu       sync.Mutex
		inflight = map[string]context.CancelFunc{} // running tool calls by request id
		wg       sync.WaitGroup
	)
	defer srv.s.close()
	defer wg.Wait()
	defer cancel() // runs first: abort running calls once input ends

	for {
		m, err := in.Read()
		if err != nil {
			var pe *proto.Error
			if errors.As(err, &pe) {
				out.Write(proto.NewError(nil, pe))
				continue
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		switch {
		case m.IsResponse():
			if srv.roots != nil {
				srv.roots.response(m) // our only requests: roots/list
			}
			continue
		case m.IsNotification():
			if m.Method == "notifications/initialized" && srv.roots != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					srv.roots.ask(ctx, srv.log.Printf)
				}()
			}
			if m.Method == "notifications/cancelled" {
				var p struct {
					RequestID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(m.Params, &p) == nil {
					mu.Lock()
					if f := inflight[string(p.RequestID)]; f != nil {
						f()
					}
					mu.Unlock()
				}
			}
			continue // notifications/initialized and others need no action
		case m.Method != "tools/call":
			res, err := srv.handle(ctx, m)
			srv.reply(out, m.ID, res, err)
			continue
		}
		cctx, ccancel := context.WithCancel(ctx)
		mu.Lock()
		inflight[string(m.ID)] = ccancel
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := srv.handle(cctx, m)
			mu.Lock()
			delete(inflight, string(m.ID))
			mu.Unlock()
			ccancel()
			srv.reply(out, m.ID, res, err)
		}()
	}
}

func (srv *Server) reply(out *proto.Writer, id json.RawMessage, res any, err error) {
	if err == nil {
		var resp *proto.Message
		if resp, err = proto.NewResult(id, res); err == nil {
			out.Write(resp)
			return
		}
	}
	var pe *proto.Error
	if !errors.As(err, &pe) {
		pe = &proto.Error{Code: proto.CodeInternalError, Message: err.Error()}
	}
	out.Write(proto.NewError(id, pe))
}

func (srv *Server) handle(ctx context.Context, m *proto.Message) (any, error) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
			ClientInfo      struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
			Capabilities json.RawMessage `json:"capabilities"`
		}
		json.Unmarshal(m.Params, &p)
		if srv.roots != nil {
			srv.roots.initialized(p.Capabilities)
		}
		srv.s.mu.Lock()
		srv.s.client = p.ClientInfo.Name
		srv.s.mu.Unlock()
		v := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}},
			"serverInfo":      map[string]string{"name": "tazzyronth", "version": srv.version},
			"instructions":    instructions,
		}, nil
	case "ping":
		return struct{}{}, nil
	case "prompts/list":
		return map[string]any{"prompts": prompts}, nil
	case "prompts/get":
		return getPrompt(m.Params)
	case "tools/list":
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &proto.Error{Code: proto.CodeInvalidParams, Message: err.Error()}
		}
		t := findTool(p.Name)
		if t == nil {
			return nil, &proto.Error{Code: proto.CodeInvalidParams, Message: "unknown tool " + p.Name}
		}
		return srv.s.callTool(ctx, t, p.Arguments), nil
	}
	return nil, &proto.Error{Code: proto.CodeMethodNotFound, Message: "unknown method " + m.Method}
}
