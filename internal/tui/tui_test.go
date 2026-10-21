package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/activity"
	"tazzyronth/internal/board"
	"tazzyronth/internal/bus"
	"tazzyronth/internal/graph"
	"tazzyronth/internal/hub"
	"tazzyronth/internal/lease"
	"tazzyronth/internal/store"
)

var t0 = time.Date(2026, 10, 17, 12, 28, 0, 0, time.Local)

const fixtureDiff = "--- a/src/auth/refresh.go\n+++ b/src/auth/refresh.go\n@@ -40,4 +40,7 @@\n \n func Rotate(t Token) (Token, error) {\n+\tif t.Expired() {\n+\t\treturn Token{}, ErrExpired\n+\t}\n-\treturn t, nil\n+\treturn issue(t.UserID)\n }\n"

// fixture is a swarm in the middle of the §6 mockup's story.
func fixture(color bool) *Model {
	m := NewModel(color)
	m.Dir = `C:\work\oauth`
	m.Now = t0.Add(12*time.Minute + 41*time.Second)
	m.SetSnapshot(hub.Snapshot{
		Agents: []hub.Agent{
			{ID: "lead", Role: "lead", Joined: t0},
			{ID: "w1", Role: "worker", Adapter: "claude", Joined: t0.Add(time.Minute)},
			{ID: "w2", Role: "worker", Adapter: "codex", Joined: t0.Add(2 * time.Minute)},
		},
		Tasks: []graph.Task{
			{ID: "schema", State: graph.Done, Owner: "w1", Attempts: 1, Result: &graph.Result{Summary: "User has provider"}},
			{ID: "backend-auth", Deps: []string{"schema"}, State: graph.Running, Owner: "w1", Attempts: 1,
				Checkpoints: []graph.Note{{Agent: "w1", Text: "rotation half done"}}},
			{ID: "frontend-login", Deps: []string{"schema"}, State: graph.Blocked, Owner: "w2", Attempts: 2, MaxAttempts: 3,
				Failures: []graph.Note{{Agent: "w3", Text: "agent w3 removed: exited: exit status 1"}}},
			{ID: "integration-tests", Deps: []string{"backend-auth", "frontend-login"}, State: graph.Pending},
			{ID: "docs", Deps: []string{"integration-tests"}, State: graph.Pending},
		},
		CriticalPath: []string{"schema", "backend-auth", "integration-tests", "docs"},
		Queues: map[string][]bus.Message{"agent:lead": {{ID: "m9", From: "hub", To: "role:lead", Kind: "blocker",
			Body: "task frontend-login failed twice", Time: t0.Add(11 * time.Minute)}}},
		Board:     []board.Entry{{Key: "goal", Value: "Add OAuth login with tests", By: "lead", Version: 1}},
		Decisions: []board.Decision{{N: 1, Text: "tokens rotate on every refresh", By: "lead", Time: t0.Add(3 * time.Minute)}},
		Leases: []lease.Lease{
			{Path: "src/auth/**", Mode: lease.Write, Holder: "w1"},
			{Path: "web/login.tsx", Mode: lease.Write, Holder: "w2"},
		},
		Swarm: hub.SwarmParams{MaxAgents: 5},
		Usage: &activity.Totals{Agents: map[string]activity.Tokens{
			"w1": {Input: 30000, Output: 8200, CostUSD: 0.41}, "w2": {Input: 9000, Output: 1000, CostUSD: 0.12},
		}, Swarm: activity.Tokens{Input: 39000, Output: 9200, CostUSD: 0.53}},
	})
	id := int64(0)
	add := func(agent string, sec int, kind, src, sum string, files []string, detail any) {
		id++
		a := activity.Activity{ID: id, Agent: agent, TS: t0.Add(time.Duration(sec) * time.Second), Kind: kind, Source: src, Summary: sum, Files: files}
		if detail != nil {
			a.Detail, _ = json.Marshal(detail)
		}
		m.AddActivity(a)
	}
	add("w1", 602, activity.Msg, activity.SrcHub, `→ task:frontend-login "User now has provider field"`, nil,
		hub.SendParams{From: "w1", To: "task:frontend-login", Body: "User now has provider field"})
	add("w1", 620, activity.Search, activity.SrcStream, `Grep "RefreshToken"`, nil, nil)
	add("w1", 631, activity.Read, activity.SrcStream, `Read C:\work\oauth\src\auth\token.go`, []string{"src/auth/token.go"}, nil)
	add("w1", 640, activity.Intent, activity.SrcHub, "Implementing refresh-token rotation in src/auth/refresh.go", nil, nil)
	add("w1", 655, activity.Cmd, activity.SrcStream, "Bash go test ./auth/...", nil, nil)
	add("w1", 656, activity.CmdResult, activity.SrcStream, "✖ Bash go test ./auth/...: --- FAIL: TestRotateExpired (0.00s)", nil,
		map[string]any{"output": "--- FAIL: TestRotateExpired (0.00s)\n    refresh_test.go:31: want ErrExpired, got nil\nFAIL"})
	add("w3", 659, activity.State, activity.SrcHub, "left: exited: exit status 1", nil, nil)
	add("w1", 667, activity.Edit, activity.SrcStream, `Edit C:\work\oauth\src\auth\refresh.go`, []string{"src/auth/refresh.go"}, nil)
	add("w1", 668, activity.Edit, activity.SrcDiff, "src/auth/refresh.go +4 −1", []string{"src/auth/refresh.go"},
		map[string]any{"path": "src/auth/refresh.go", "diff": fixtureDiff, "added": 4, "removed": 1})
	add("w2", 669, activity.Edit, activity.SrcDiff, "web/login.tsx +2 −0", []string{"web/login.tsx"},
		map[string]any{"path": "web/login.tsx", "diff": "--- a/web/login.tsx\n+++ b/web/login.tsx\n@@ -1 +1,3 @@\n a\n+b\n+c\n"})
	add("hub", 670, activity.Alert, activity.SrcDiff, "⚠ unattributed edit: README.md changed but nobody leased it", []string{"README.md"}, nil)
	add("w2", 671, activity.Say, activity.SrcStream, "Waiting for the schema artifact before wiring the form", nil, nil)
	return m
}

var sizes = [][2]int{{80, 24}, {120, 40}}

var viewChecks = map[view][]string{
	vOverview: {"AGENT TAZZYRONTH", "swarm 3/5 agents", "1/5 tasks", "w1", "backend-auth", "src/auth/**", "✖ gone",
		"TASK GRAPH", "schema ✔", "MESSAGES", "User now has provider field", "quit"},
	vAgent:    {"w1", "NOW ▸", "refresh-token rotation", "ACTIVITY", "LIVE DIFF", "src/auth/refresh.go", "+4", "ErrExpired", "FILES", "OUTPUT", "want ErrExpired"},
	vActivity: {"SWARM ACTIVITY", "w2", "Waiting for the schema", "unattributed"},
	vFiles:    {"FILES", "src/auth/refresh.go", "web/login.tsx", "README.md", "unattributed", "DIFF"},
	vGraph:    {"TASK GRAPH", "integration-tests", "pending", "2/3", "rotation half done"},
	vMessages: {"MESSAGES", "w1→task:frontend-login", "[blocker]", "failed twice"},
	vBoard:    {"BOARD", "goal", "Add OAuth login", "tokens rotate"},
	vUsage:    {"USAGE", "38.2k", "~$0.41", "swarm"},
}

func render(m *Model, v view, w, h int) []string {
	m.view = v
	if v == vAgent {
		m.agent = "w1"
	}
	return Render(m, w, h)
}

func TestViewsFit(t *testing.T) {
	for v, wants := range viewChecks {
		for _, sz := range sizes {
			for _, color := range []bool{false, true} {
				w, h := sz[0], sz[1]
				frame := render(fixture(color), v, w, h)
				name := fmt.Sprintf("view %d %dx%d color=%v", v, w, h, color)
				if len(frame) != h {
					t.Errorf("%s: %d lines", name, len(frame))
				}
				for i, l := range frame {
					lim := w
					if i == h-1 {
						lim = w - 1
					}
					if width(l) > lim {
						t.Errorf("%s: line %d is %d wide: %q", name, i, width(l), stripStyle(l))
					}
					if strings.ContainsAny(stripStyle(l), "\t\r\n") {
						t.Errorf("%s: line %d has control characters", name, i)
					}
				}
				text := stripStyle(strings.Join(frame, "\n"))
				if color != strings.Contains(strings.Join(frame, ""), "\x1b[") {
					t.Errorf("%s: escapes present = %v", name, !color)
				}
				for _, want := range wants {
					if !strings.Contains(text, want) {
						t.Errorf("%s: missing %q", name, want)
					}
				}
			}
		}
	}
}

func TestFramesSmallAndOffline(t *testing.T) {
	m := NewModel(false)
	m.Conn, m.Dir = "hub not running — waiting for it", "/p"
	frame := Render(m, 80, 24)
	if text := strings.Join(frame, "\n"); !strings.Contains(text, "hub not running") || !strings.Contains(text, "tazzy hub") {
		t.Errorf("offline frame:\n%s", text)
	}
	if f := Render(fixture(false), 30, 8); len(f) != 8 || !strings.Contains(f[0], "too small") {
		t.Errorf("tiny frame = %q", f)
	}
	// Connection lost after data arrived: the data stays, the footer says so.
	m = fixture(false)
	m.Conn = "hub connection lost — reconnecting"
	if f := Render(m, 80, 24); !strings.Contains(f[23], "reconnecting") || !strings.Contains(f[1], "swarm") {
		t.Errorf("reconnecting frame:\n%s", strings.Join(f, "\n"))
	}
}

func TestFeedMergesEdits(t *testing.T) {
	m := fixture(false)
	var edits []entry
	for _, e := range feed(m.Acts, nil) {
		if e.Kind == activity.Edit {
			edits = append(edits, e)
		}
	}
	// w1: tool call + diff → one line with counts; w2: diff only → kept.
	if len(edits) != 2 {
		t.Fatalf("edit entries = %+v", edits)
	}
	if e := edits[0]; e.Agent != "w1" || e.Source != activity.SrcStream || !e.counted || e.plus != 4 || e.minus != 1 {
		t.Errorf("merged edit = %+v", e)
	}
	if e := edits[1]; e.Agent != "w2" || e.Source != activity.SrcDiff || e.plus != 2 {
		t.Errorf("diff-only edit = %+v", e)
	}
	line := stripStyle(m.feedLine(edits[0], nil, false))
	if !strings.Contains(line, "✎ Edit   src/auth/refresh.go  +4 −1") {
		t.Errorf("edit line = %q", line)
	}
	// A second diff for the same file with no new call is its own line.
	m.AddActivity(activity.Activity{ID: 100, Agent: "w1", Kind: activity.Edit, Source: activity.SrcDiff,
		Summary: "src/auth/refresh.go +5 −1", Files: []string{"src/auth/refresh.go"}})
	n := 0
	for _, e := range feed(m.Acts, nil) {
		if e.Kind == activity.Edit && e.Agent == "w1" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("w1 edit lines = %d, want 2", n)
	}
}

func TestKeys(t *testing.T) {
	m := fixture(false)
	m.Key("down") // w1
	if a := m.Key("enter"); a.Agent != "w1" || m.view != vAgent {
		t.Fatalf("enter: %+v view %d", a, m.view)
	}
	m.Key("m")
	for _, k := range []string{"h", "i", "tab"} {
		m.Key(k)
	}
	a := m.Key("enter")
	if a.Send == nil || a.Send.To != "agent:w1" || a.Send.Body != "hi" || a.Send.Kind != "info" || a.Send.From != "human" {
		t.Errorf("send = %+v", a.Send)
	}
	m.Key("k")
	if a := m.Key("n"); a.Retire != "" {
		t.Error("retire without confirmation")
	}
	m.Key("k")
	if a := m.Key("y"); a.Retire != "w1" {
		t.Errorf("retire = %+v", a)
	}
	m.Key("up")
	if m.follow {
		t.Error("scrolling up should stop following")
	}
	m.Key("enter")
	if !m.expand || !strings.Contains(strings.Join(Render(m, 120, 40), "\n"), "EVENT #") {
		t.Error("enter should expand the selected event")
	}
	m.Key("esc")
	m.Key("esc")
	if m.view != vOverview || m.sel != 1 {
		t.Errorf("back: view %d sel %d", m.view, m.sel)
	}
	for k, v := range map[string]view{"a": vActivity, "F": vFiles, "g": vGraph, "b": vBoard, "u": vUsage} {
		m.Key(k)
		if m.view != v {
			t.Errorf("%s: view %d", k, m.view)
		}
	}
	m.Key("esc")
	m.Key("m")
	if m.view != vMessages {
		t.Error("m on the overview opens messages")
	}
	if !m.Key("q").Quit || !m.Key("ctrl+c").Quit {
		t.Error("q / ctrl+c should quit")
	}
}

func TestDetailFetchedOnDemand(t *testing.T) {
	m := fixture(false)
	m.AddActivity(activity.Activity{ID: 200, Agent: "w1", TS: m.Now, Kind: activity.Edit, Source: activity.SrcDiff,
		Summary: "src/auth/big.go +900 −0", Files: []string{"src/auth/big.go"}, DetailSize: 50000})
	frame := strings.Join(render(m, vAgent, 120, 40), "\n")
	if !strings.Contains(frame, "loading…") {
		t.Error("big diff should show loading")
	}
	if ids := m.NeedDetails(); len(ids) != 1 || ids[0] != 200 {
		t.Fatalf("NeedDetails = %v", ids)
	}
	m.SetDetail(200, json.RawMessage(`{"diff":"@@ -0,0 +1 @@\n+package big\n"}`))
	if frame := strings.Join(Render(m, 120, 40), "\n"); !strings.Contains(frame, "+ package big") {
		t.Errorf("detail not rendered:\n%s", frame)
	}
}

func TestAddEvent(t *testing.T) {
	m := NewModel(false)
	a, _ := json.Marshal(activity.Activity{ID: 7, Agent: "w1", Kind: activity.Cmd, Summary: "Bash ls"})
	if m.AddEvent(store.Event{Type: "activity", Data: a}) || len(m.Acts) != 1 {
		t.Error("activity event")
	}
	m.AddEvent(store.Event{Type: "activity", Data: a})
	if len(m.Acts) != 1 {
		t.Error("duplicate activity kept")
	}
	p, _ := json.Marshal(hub.SendParams{From: "hub", To: "agent:w1", Kind: "directive", Body: "still there?"})
	if !m.AddEvent(store.Event{Seq: 3, Type: "msg.send", Data: p}) || len(m.Msgs) != 1 {
		t.Error("hub message event")
	}
}

func TestParseKeys(t *testing.T) {
	got := parseKeys([]byte("q\x1b[A\x1b[B\x1b\r\x7f\x03é\x1b[5~\x1b[1;5C"))
	want := []string{"q", "up", "down", "esc", "enter", "backspace", "ctrl+c", "é", "pgup"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("parseKeys = %q, want %q", got, want)
	}
}

func TestFitAndWidth(t *testing.T) {
	for _, tt := range []struct {
		in   string
		w    int
		want string
	}{
		{"abc", 5, "abc  "},
		{"abcdef", 4, "abc…"},
		{"日本語", 4, "日… "},
		{"\x1b[1mbold\x1b[0m", 6, "\x1b[1mbold\x1b[0m  "},
		{"\x1b[1mbold text\x1b[0m", 5, "\x1b[1mbold…\x1b[0m"},
	} {
		if got := fit(tt.in, tt.w); got != tt.want {
			t.Errorf("fit(%q, %d) = %q, want %q", tt.in, tt.w, got, tt.want)
		}
	}
	if got := clean("a\tb\x1b[31mc\n"); got != "a b[31mc " {
		t.Errorf("clean = %q", got)
	}
}

func TestStatus(t *testing.T) {
	m := fixture(false)
	text := StatusText(m.Snap, m.Acts, m.Now)
	for _, want := range []string{"AGENT TAZZYRONTH", "swarm 3/5 agents", "w1", "backend-auth", "TASKS", "RECENT"} {
		if !strings.Contains(text, want) {
			t.Errorf("status text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Error("status text has escapes")
	}
	line := StatusLine(m.Snap, m.Acts)
	if line != "tazzy 3/5 agents · 1/5 tasks · 1 blocked · ✎ w2 web/login.tsx" {
		t.Errorf("status line = %q", line)
	}
}

// TestDump prints NO_COLOR frames of every view for eyeballing:
// TAZZY_DUMP=1 go test ./internal/tui -run TestDump -v
func TestDump(t *testing.T) {
	if os.Getenv("TAZZY_DUMP") == "" {
		t.Skip("set TAZZY_DUMP=1")
	}
	for v := vOverview; v <= vUsage; v++ {
		for _, sz := range sizes {
			fmt.Printf("=== view %d %dx%d\n", v, sz[0], sz[1])
			for _, l := range render(fixture(false), v, sz[0], sz[1]) {
				fmt.Println(strings.TrimRight(l, " ") + "|")
			}
		}
	}
}

func TestOfflineHubPicker(t *testing.T) {
	m := NewModel(false)
	m.Conn, m.Dir = "hub not running — waiting for it", `C:\Users\me`
	m.SetOthers([]hub.Running{{Dir: `C:\work\a`, PID: 11}, {Dir: `C:\work\b`, PID: 22}})
	text := strings.Join(Render(m, 80, 24), "\n")
	if !strings.Contains(text, "hubs running elsewhere") || !strings.Contains(text, `▸ C:\work\a  (pid 11)`) || !strings.Contains(text, "enter attach") {
		t.Fatalf("picker frame:\n%s", text)
	}
	m.Key("down")
	m.SetOthers([]hub.Running{{Dir: `C:\work\0`}, {Dir: `C:\work\a`}, {Dir: `C:\work\b`}}) // selection follows b
	if a := m.Key("enter"); a.Attach != `C:\work\b` || m.Dir != `C:\work\b` || m.Others != nil {
		t.Fatalf("enter = %+v, dir %q, others %v", a, m.Dir, m.Others)
	}
	if a := m.Key("q"); !a.Quit {
		t.Fatal("q should quit while offline")
	}
}

func TestPauseStop(t *testing.T) {
	m := fixture(false)
	if a := m.Key("p"); a.Control != hub.ActionPause {
		t.Fatalf("p on a running swarm = %+v", a)
	}
	frame := strings.Join(Render(m, 160, 40), "\n")
	if !strings.Contains(frame, "[p] pause") || !strings.Contains(frame, "[x] stop") {
		t.Errorf("footer lacks pause/stop keys:\n%s", frame)
	}
	m.Snap.State = hub.RunPaused
	if a := m.Key("p"); a.Control != hub.ActionResume {
		t.Errorf("p on a paused swarm = %+v", a)
	}
	if frame := strings.Join(Render(m, 160, 40), "\n"); !strings.Contains(frame, "PAUSED") || !strings.Contains(frame, "[p] resume") {
		t.Errorf("paused frame lacks badge or resume key:\n%s", frame)
	}
	m.Key("x")
	if a := m.Key("n"); a.Control != "" {
		t.Error("stop without confirmation")
	}
	m.Key("x")
	if a := m.Key("y"); a.Control != hub.ActionStop {
		t.Errorf("stop = %+v", a)
	}
	m.Snap.State = hub.RunStopped
	if !strings.Contains(strings.Join(Render(m, 160, 40), "\n"), "STOPPED") {
		t.Error("no STOPPED badge")
	}
	if a := m.Key("p"); a.Control != "" || m.Flash == "" {
		t.Errorf("p on a stopped swarm = %+v, flash %q", a, m.Flash)
	}
	m.Key("x")
	if m.input != nil {
		t.Error("x on a stopped swarm should not ask")
	}
	if l := StatusLine(m.Snap, nil); !strings.Contains(l, "stopped") {
		t.Errorf("status line = %q", l)
	}
}
