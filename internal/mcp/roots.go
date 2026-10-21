package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"tazzyronth/internal/proto"
)

// roots finds the project through the client's MCP roots. Some hosts
// (Antigravity) start MCP servers in a directory of their own, so without
// -dir or TAZZY_PROJECT the shim asks the client where the project is.
type roots struct {
	fallback string        // project dir when the client has none
	set      chan struct{} // closed once dir is settled

	mu     sync.Mutex
	dir    string // settled project dir
	client bool   // dir came from the client's roots, not the fallback
	has    bool   // the client advertised the roots capability
	out    *proto.Writer
	calls  map[string]chan *proto.Message // our requests awaiting a response, by id
	nextID int
}

// rootsTimeout bounds how long Dir waits for the client to initialize and
// answer roots/list before settling on the fallback.
var rootsTimeout = 10 * time.Second

// UseRoots makes the server find the project through the client's MCP roots:
// once a client that advertises the roots capability is initialized, it is
// sent roots/list and its first file:// root becomes the project dir.
// fallback is used when the client has no roots capability or file root, or
// does not answer in time. Call before Serve.
func (srv *Server) UseRoots(fallback string) {
	srv.roots = &roots{fallback: fallback, set: make(chan struct{}), calls: map[string]chan *proto.Message{}}
}

// Dir returns the project dir found through the client's roots, waiting
// until it is known (at most rootsTimeout). Without UseRoots it returns "".
func (srv *Server) Dir(ctx context.Context) string {
	d, _ := srv.DirSource(ctx)
	return d
}

// DirSource is Dir that also reports whether the dir came from a file://
// root of the client (true) or is the fallback (false).
func (srv *Server) DirSource(ctx context.Context) (string, bool) {
	r := srv.roots
	if r == nil {
		return "", false
	}
	t := time.NewTimer(rootsTimeout)
	defer t.Stop()
	select {
	case <-r.set:
	case <-t.C:
		srv.log.Printf("no roots from the client within %s; using %s", rootsTimeout, r.fallback)
		r.settle(r.fallback, false)
	case <-ctx.Done():
		return r.fallback, false // unsettled: a later call may still learn the roots
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dir, r.client
}

// settle fixes the project dir; only the first call counts. client says it
// came from the client's roots.
func (r *roots) settle(dir string, client bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.set:
	default:
		r.dir, r.client = dir, client
		close(r.set)
	}
}

// initialized notes the client's capabilities from initialize. A client
// without roots settles the fallback at once.
func (r *roots) initialized(caps json.RawMessage) {
	var c struct {
		Roots json.RawMessage `json:"roots"`
	}
	json.Unmarshal(caps, &c)
	has := len(c.Roots) > 0 && string(c.Roots) != "null"
	r.mu.Lock()
	r.has = has
	r.mu.Unlock()
	if !has {
		r.settle(r.fallback, false)
	}
}

// response routes a response from the client to the request awaiting it.
func (r *roots) response(m *proto.Message) {
	r.mu.Lock()
	ch := r.calls[string(m.ID)]
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- m:
		default:
		}
	}
}

// ask sends roots/list (if the client supports it) and settles the project
// dir from the answer.
func (r *roots) ask(ctx context.Context, logf func(string, ...any)) {
	r.mu.Lock()
	has, out := r.has, r.out
	r.nextID++
	id := "tazzy-roots-" + strconv.Itoa(r.nextID)
	r.mu.Unlock()
	if !has {
		return
	}
	dir, client := r.fallback, false
	defer func() { r.settle(dir, client) }()
	req, err := proto.NewRequest(id, "roots/list", nil)
	if err != nil || out == nil {
		return
	}
	ch := make(chan *proto.Message, 1)
	r.mu.Lock()
	r.calls[string(req.ID)] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.calls, string(req.ID))
		r.mu.Unlock()
	}()
	if out.Write(req) != nil {
		return
	}
	t := time.NewTimer(rootsTimeout)
	defer t.Stop()
	var resp *proto.Message
	select {
	case resp = <-ch:
	case <-t.C:
		logf("roots/list: no answer within %s; using %s", rootsTimeout, dir)
		return
	case <-ctx.Done():
		return
	}
	if resp.Error != nil {
		logf("roots/list: %s; using %s", resp.Error.Message, dir)
		return
	}
	var res struct {
		Roots []struct {
			URI string `json:"uri"`
		} `json:"roots"`
	}
	json.Unmarshal(resp.Result, &res)
	for _, root := range res.Roots {
		if p, ok := fileURIPath(root.URI); ok {
			dir, client = p, true
			logf("project dir from the client's roots: %s", dir)
			return
		}
	}
	logf("roots/list: no file:// root; using %s", dir)
}

// NoProjectError is what connect returns when the shim refuses its project
// dir: the client reported no workspace root and the cwd it fell back to is
// not a project (no git work tree, no .tazzyronth). Tool calls that need the
// hub fail with its text, unwrapped, so the model can relay it.
type NoProjectError struct{ Dir string }

func (e *NoProjectError) Error() string {
	return fmt.Sprintf("no project folder: the client reported no workspace root and %s is not a project (no git repo, no .tazzyronth). "+
		"Open a folder in your editor, or add \"-dir\", \"<project>\" to the tazzyronth MCP server args.", e.Dir)
}

var winDrive = regexp.MustCompile(`^/[A-Za-z]:`)

// fileURIPath turns a file:// URI into a local path. "file:///C:/x" and
// "file:///c%3A/x" become "C:/x" (with the OS separator), and
// "file://host/share" a UNC path.
func fileURIPath(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return "", false
	}
	p := u.Path
	if p == "" {
		return "", false
	}
	if winDrive.MatchString(p) {
		p = p[1:]
	}
	if h := u.Host; h != "" && !strings.EqualFold(h, "localhost") {
		p = "//" + h + p
	}
	return filepath.Clean(filepath.FromSlash(p)), true
}
