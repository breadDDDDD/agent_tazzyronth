package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"tazzyronth/assets"
	"tazzyronth/internal/activity"
	"tazzyronth/internal/hub"
)

const frameEvery = 100 * time.Millisecond // at most 10 fps

// Options configure Run.
type Options struct {
	Dir    string // project directory (where the hub's hub.json lives)
	Color  bool
	Splash bool
}

// Run shows the dashboard on the terminal (stdin/stdout) until q, Ctrl+C
// or ctx ends. The terminal is restored on every exit, including a panic.
// The caller must have enabled VT processing on Windows.
func Run(ctx context.Context, opt Options) error {
	in, out := os.Stdin, os.Stdout
	inFd, outFd := int(in.Fd()), int(out.Fd())
	if !term.IsTerminal(inFd) || !term.IsTerminal(outFd) {
		return errors.New("tazzy tui needs an interactive terminal (try tazzy status or tazzy watch --json)")
	}
	old, err := term.MakeRaw(inFd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			out.WriteString("\x1b[0m\x1b[2J\x1b[?25h\x1b[?1049l")
			term.Restore(inFd, old)
		})
	}
	defer restore()
	defer func() {
		if r := recover(); r != nil {
			restore()
			panic(r)
		}
	}()
	out.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J")

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	keys := make(chan string, 64)
	go readKeys(in, keys)

	if opt.Splash {
		splash(ctx, out, outFd, opt.Color, keys)
	}

	dir, _ := filepath.Abs(opt.Dir)
	m := NewModel(opt.Color)
	m.Dir, m.Conn = dir, "connecting to the hub…"
	updates := make(chan func(*Model), 256)
	d := &dataLoop{dir: dir, up: updates, requested: map[int64]bool{}}
	go d.run(ctx)

	scr := &screen{out: out}
	tick := time.NewTicker(frameEvery)
	defer tick.Stop()
	dirty, lastClock := true, time.Time{}
	draw := func() {
		w, h, err := term.GetSize(outFd)
		if err != nil {
			w, h = 80, 24
		}
		m.Now = time.Now()
		scr.draw(Render(m, w, h), w, h)
		d.fetchDetails(ctx, m.NeedDetails())
		dirty, lastClock = false, m.Now
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case k, ok := <-keys:
			if !ok {
				return nil // stdin closed
			}
			act := m.Key(k)
			if act.Quit {
				return nil
			}
			d.act(ctx, act)
			draw()
		case f := <-updates:
			f(m)
			dirty = true
		case <-tick.C:
			if m.stale {
				m.stale = !d.refresh(ctx)
			}
			if w, h, err := term.GetSize(outFd); err == nil && (w != scr.w || h != scr.h) {
				dirty = true
			}
			if dirty || time.Since(lastClock) >= time.Second {
				draw()
			}
		}
	}
}

// screen writes frames, rewriting only the lines that changed.
type screen struct {
	out  io.Writer
	prev []string
	w, h int
}

func (s *screen) draw(frame []string, w, h int) {
	var b strings.Builder
	if w != s.w || h != s.h {
		b.WriteString("\x1b[0m\x1b[2J")
		s.prev, s.w, s.h = nil, w, h
	}
	for i, l := range frame {
		if i < len(s.prev) && s.prev[i] == l {
			continue
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[0m\x1b[K", i+1, l)
	}
	s.prev = frame
	if b.Len() > 0 {
		io.WriteString(s.out, b.String())
	}
}

// splash shows the full logo for about a second; any key skips it.
func splash(ctx context.Context, out io.Writer, fd int, color bool, keys <-chan string) {
	logo := assets.LogoFullPlain
	if color {
		logo = assets.LogoFull
	}
	lines := strings.Split(strings.TrimRight(logo, "\n"), "\n")
	w, h, err := term.GetSize(fd)
	if err != nil || h < len(lines)+3 || w < 42 {
		return
	}
	var b strings.Builder
	top := (h - len(lines) - 2) / 2
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		fmt.Fprintf(&b, "\x1b[%d;%dH%s", top+i+1, max(1, (w-width(l))/2+1), l)
	}
	title := painter(color).paint(stTitle, "AGENT TAZZYRONTH")
	fmt.Fprintf(&b, "\x1b[%d;%dH%s", top+len(lines)+2, max(1, (w-16)/2+1), title)
	io.WriteString(out, b.String())
	select {
	case <-keys:
	case <-time.After(time.Second):
	case <-ctx.Done():
	}
}

// readKeys turns raw stdin bytes into key names until stdin fails.
func readKeys(r io.Reader, keys chan<- string) {
	buf := make([]byte, 256)
	for {
		n, err := r.Read(buf)
		for _, k := range parseKeys(buf[:n]) {
			keys <- k
		}
		if err != nil {
			close(keys)
			return
		}
	}
}

var csiKeys = map[string]string{
	"A": "up", "B": "down", "C": "right", "D": "left", "H": "home", "F": "end",
	"1~": "home", "7~": "home", "4~": "end", "8~": "end", "5~": "pgup", "6~": "pgdn",
}

// parseKeys splits one read of raw input into key names. A lone ESC is
// "esc"; escape sequences arrive whole in one read in practice.
func parseKeys(b []byte) []string {
	var out []string
	for i := 0; i < len(b); {
		c := b[i]
		switch {
		case c == 0x1b:
			if i+1 >= len(b) || (b[i+1] != '[' && b[i+1] != 'O') {
				out = append(out, "esc")
				i++
				continue
			}
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j >= len(b) {
				return out
			}
			if k, ok := csiKeys[string(b[i+2:j+1])]; ok {
				out = append(out, k)
			}
			i = j + 1
		case c == 3:
			out, i = append(out, "ctrl+c"), i+1
		case c == '\r' || c == '\n':
			out, i = append(out, "enter"), i+1
			if c == '\r' && i < len(b) && b[i] == '\n' {
				i++
			}
		case c == 0x7f || c == 8:
			out, i = append(out, "backspace"), i+1
		case c == '\t':
			out, i = append(out, "tab"), i+1
		case c < 0x20:
			i++
		default:
			r, size := utf8.DecodeRune(b[i:])
			out, i = append(out, string(r)), i+size
		}
	}
	return out
}

// dataLoop keeps a hub connection and feeds the model.
type dataLoop struct {
	dir string // guarded by mu: the user can attach to another project
	up  chan<- func(*Model)

	mu         sync.Mutex
	c          *hub.Client
	requested  map[int64]bool // details asked for (UI goroutine only)
	refreshing atomic.Bool
}

func (d *dataLoop) send(ctx context.Context, f func(*Model)) {
	select {
	case d.up <- f:
	case <-ctx.Done():
	}
}

func (d *dataLoop) client() *hub.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.c
}

func (d *dataLoop) project() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dir
}

func (d *dataLoop) setClient(c *hub.Client) {
	d.mu.Lock()
	d.c = c
	d.mu.Unlock()
}

func (d *dataLoop) run(ctx context.Context) {
	var lastID int64
	connected := false
	for ctx.Err() == nil {
		dir := d.project()
		c, err := hub.Connect(dir)
		if err != nil {
			msg := "hub not running — waiting for it"
			if connected {
				msg = "hub connection lost — reconnecting"
			}
			others := hub.OtherHubs(dir)
			d.send(ctx, func(m *Model) {
				if m.Dir == dir { // not just attached elsewhere
					m.Conn = msg
					m.SetOthers(others)
				}
			})
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		events, err := c.SubscribeActivity(ctx)
		var snap hub.Snapshot
		var acts []activity.Activity
		if err == nil {
			err = c.Call(ctx, "status", nil, &snap)
		}
		if err == nil {
			err = c.Call(ctx, "activity.list", hub.ActivityListParams{After: lastID, Limit: 500}, &acts)
		}
		if err != nil {
			c.Close()
			continue
		}
		connected = true
		for _, a := range acts {
			lastID = max(lastID, a.ID)
		}
		d.setClient(c)
		d.send(ctx, func(m *Model) {
			m.Conn, m.Others = "", nil
			m.SetSnapshot(snap)
			m.AddActivity(acts...)
		})
		for ev := range events {
			if ev.Type == "activity" {
				var a struct{ ID int64 }
				json.Unmarshal(ev.Data, &a)
				lastID = max(lastID, a.ID)
			}
			d.send(ctx, func(m *Model) {
				if m.AddEvent(ev) {
					m.stale = true
				}
			})
		}
		d.setClient(nil)
		c.Close()
		d.send(ctx, func(m *Model) { m.Conn = "hub connection lost — reconnecting" })
	}
}

// refresh re-reads the snapshot in the background; false if it can't now.
func (d *dataLoop) refresh(ctx context.Context) bool {
	c := d.client()
	if c == nil || !d.refreshing.CompareAndSwap(false, true) {
		return c == nil // nothing to refresh from; the next connect reloads
	}
	go func() {
		defer d.refreshing.Store(false)
		var snap hub.Snapshot
		if c.Call(ctx, "status", nil, &snap) == nil {
			d.send(ctx, func(m *Model) { m.Snap = snap })
		}
	}()
	return true
}

func (d *dataLoop) fetchDetails(ctx context.Context, ids []int64) {
	c := d.client()
	for _, id := range ids {
		if c == nil || d.requested[id] {
			continue
		}
		d.requested[id] = true
		go func() {
			var raw json.RawMessage
			if c.Call(ctx, "activity.detail", hub.ActivityDetailParams{ID: id}, &raw) != nil {
				raw = json.RawMessage(`{}`)
			}
			d.send(ctx, func(m *Model) { m.SetDetail(id, raw) })
		}()
	}
}

// act runs a key's side effect against the hub in the background.
func (d *dataLoop) act(ctx context.Context, a Action) {
	if a.Attach != "" {
		d.mu.Lock()
		d.dir = a.Attach
		d.mu.Unlock()
		return
	}
	c := d.client()
	if c == nil || a.Send == nil && a.Retire == "" && a.Control == "" && a.Agent == "" {
		if a.Send != nil || a.Retire != "" || a.Control != "" {
			d.send(ctx, func(m *Model) { m.Flash = "not connected to the hub" })
		}
		return
	}
	go func() {
		var flash string
		switch {
		case a.Send != nil:
			flash = "sent " + a.Send.Kind + " to " + strings.TrimPrefix(a.Send.To, "agent:")
			if err := c.Call(ctx, "msg.send", a.Send, nil); err != nil {
				flash = "send failed: " + err.Error()
			}
		case a.Retire != "":
			flash = "retired " + a.Retire
			if err := c.Call(ctx, "agent.retire", hub.RetireParams{Agent: a.Retire, Reason: "retired from the dashboard"}, nil); err != nil {
				flash = "retire failed: " + err.Error()
			}
		case a.Control != "":
			var res hub.ControlResult
			if err := c.Call(ctx, "swarm.control", hub.ControlParams{Action: a.Control, By: "human"}, &res); err != nil {
				flash = a.Control + " failed: " + err.Error()
			} else {
				flash = "swarm " + res.State
				d.send(ctx, func(m *Model) { m.Snap.State = res.State })
			}
		case a.Agent != "":
			var acts []activity.Activity
			if c.Call(ctx, "activity.list", hub.ActivityListParams{Agent: a.Agent, Limit: 500}, &acts) == nil {
				d.send(ctx, func(m *Model) { m.AddActivity(acts...) })
			}
			return
		}
		d.send(ctx, func(m *Model) { m.Flash = flash; m.stale = true })
	}()
}

// Frame renders one frame of the dashboard from the hub's current state
// without a terminal (tazzy tui -frame): the overview, or agent's detail.
func Frame(ctx context.Context, dir string, w, h int, agent string, color bool) ([]string, error) {
	c, err := hub.Connect(dir)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	m := NewModel(color)
	m.Dir, _ = filepath.Abs(dir)
	var snap hub.Snapshot
	var acts []activity.Activity
	if err := c.Call(ctx, "status", nil, &snap); err != nil {
		return nil, err
	}
	if err := c.Call(ctx, "activity.list", hub.ActivityListParams{Agent: agent, Limit: 500}, &acts); err != nil {
		return nil, err
	}
	m.SetSnapshot(snap)
	m.AddActivity(acts...)
	if agent != "" {
		m.openAgent(agent)
	}
	frame := Render(m, w, h)
	if ids := m.NeedDetails(); len(ids) > 0 {
		for _, id := range ids {
			var raw json.RawMessage
			if c.Call(ctx, "activity.detail", hub.ActivityDetailParams{ID: id}, &raw) == nil {
				m.SetDetail(id, raw)
			}
		}
		frame = Render(m, w, h)
	}
	return frame, nil
}
