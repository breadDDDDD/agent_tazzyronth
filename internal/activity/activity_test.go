package activity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type line struct{ kind, summary string }

func decodeFile(t *testing.T, format, file string) []Activity {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Activity
	Decode(format, f, func(a Activity) { out = append(out, a) })
	return out
}

func checkLines(t *testing.T, got []Activity, want []line) {
	t.Helper()
	for i := range max(len(got), len(want)) {
		var g, w line
		if i < len(got) {
			g = line{got[i].Kind, OneLine(got[i].Summary, summaryMax)}
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, g, w)
		}
	}
}

func sum(as []Activity) Tokens {
	var t Tokens
	for _, a := range as {
		if a.Usage != nil {
			t.add(*a.Usage)
		}
	}
	return t
}

func TestDecodeClaude(t *testing.T) {
	got := decodeFile(t, "claude", "claude.jsonl")
	checkLines(t, got, []line{
		{Usage, "1215 tokens"},
		{Say, "I'll fix the refresh logic."},
		{Read, "Read /repo/src/auth/token.go"},
		{Edit, "Edit /repo/src/auth/refresh.go"},
		{Cmd, "Bash go test ./auth/..."},
		{CmdResult, "✖ Edit /repo/src/auth/refresh.go: <tool_use_error>String to replace not found in file.</tool_use_error>"},
		{CmdResult, "✖ Bash go test ./auth/...: --- FAIL: TestRotate (0.00s) FAIL"},
		{Search, `Grep "RefreshToken" in src`},
		{Tool, "TodoWrite"},
		{State, "run finished after 6 turns, $0.41"},
	})
	if len(got) > 3 && (len(got[2].Files) != 1 || got[2].Files[0] != "/repo/src/auth/token.go") {
		t.Errorf("Read files = %v", got[2].Files)
	}
	// Usage per message id once, topped up to the result's totals.
	want := Tokens{Input: 40, Output: 20, CacheRead: 1000, CacheWrite: 200, CostUSD: 0.4125}
	if u := sum(got); u != want {
		t.Errorf("usage = %+v, want %+v", u, want)
	}
}

func TestDecodeCodex(t *testing.T) {
	got := decodeFile(t, "codex", "codex.jsonl")
	checkLines(t, got, []line{
		{Cmd, "shell bash -lc 'go test ./...'"},
		{CmdResult, "✓ shell bash -lc 'go test ./...': exit 0 ok pkg"},
		{Edit, "patch update src/a.go, add src/b.go"},
		{Say, "All tests pass."},
		{Usage, "turn finished, 1050 tokens"},
		{Alert, "turn failed: rate limited"},
	})
	if u := sum(got); u != (Tokens{Input: 200, CacheRead: 800, Output: 50}) {
		t.Errorf("usage = %+v", u)
	}
}

func TestDecodeGemini(t *testing.T) {
	got := decodeFile(t, "gemini", "gemini.jsonl")
	checkLines(t, got, []line{
		{Say, "Looking at the code."},
		{Read, "read_file /repo/a.go"},
		{Cmd, "run_shell_command make test"},
		{CmdResult, "✖ run_shell_command make test: exit code 2"},
		{Say, "Tests fail."},
		{State, "run success, 300 tokens"},
	})
}

// TestDecodeHostile: garbage, huge lines and unknown formats are drained
// without a panic.
func TestDecodeHostile(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","input":"not an object"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":{"weird":true}}]}}`,
		`{"type":"result","usage":null,"total_cost_usd":"free"}`,
		`{` + strings.Repeat("x", 1<<20),
		`{"type":"assistant","message":{"content":[{"type":"text","text":"still alive"}]}}`,
	}, "\n")
	for format := range Formats {
		Decode(format, strings.NewReader(in), func(Activity) {})
	}
	var got []Activity
	Decode("claude", strings.NewReader(in), func(a Activity) { got = append(got, a) })
	if len(got) == 0 || got[len(got)-1].Summary != "still alive" {
		t.Errorf("got %+v", got)
	}
	Decode("nope", strings.NewReader(in), func(Activity) { t.Error("emit for unknown format") })
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	big := json.RawMessage(fmt.Sprintf(`{"diff":%q}`, strings.Repeat("+line\n", 2000)))
	for i := range 5 {
		if _, err := s.Add(Activity{Agent: "a1", Kind: Cmd, Summary: fmt.Sprintf("cmd %d\nsecond line", i), Usage: &Tokens{Input: 10, CostUSD: 0.5}}); err != nil {
			t.Fatal(err)
		}
	}
	b, err := s.Add(Activity{Agent: "a2", Kind: Edit, Summary: "edit", Detail: big})
	if err != nil {
		t.Fatal(err)
	}
	if b.Detail != nil || b.DetailSize != len(big) {
		t.Errorf("big detail kept inline: size %d", b.DetailSize)
	}
	if _, err := s.Add(Activity{Kind: Cmd}); err == nil {
		t.Error("activity without agent accepted")
	}

	check := func(s *Store) {
		t.Helper()
		r := s.Recent("a1", 0, 0)
		if len(r) != 3 || r[0].Summary != "cmd 2 second line" || r[2].ID != 5 {
			t.Errorf("ring a1 = %+v", r)
		}
		if m := s.Recent("", 4, 2); len(m) != 2 || m[0].ID != 5 || m[1].Agent != "a2" {
			t.Errorf("merged = %+v", m)
		}
		d, err := s.Detail(b.ID)
		if err != nil || string(d) != string(big) {
			t.Errorf("detail: %v (%d bytes)", err, len(d))
		}
		if tot := s.Totals(); tot.Swarm.Input != 50 || tot.Agents["a1"].CostUSD != 2.5 {
			t.Errorf("totals = %+v", tot)
		}
	}
	check(s)
	s.Close()
	s, err = Open(dir, 3) // reload: ring, ids and the full tally survive
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	check(s)
	if a, _ := s.Add(Activity{Agent: "a1", Kind: Say, Summary: "x"}); a.ID != 7 {
		t.Errorf("id after reload = %d", a.ID)
	}
}

func TestFromHook(t *testing.T) {
	join := `{"session_id":"S","agent_id":"sub-1","hook_event_name":"PostToolUse","tool_name":"mcp__tazzyronth__tazzy_join",
		"tool_input":{"name":"x"},"tool_response":[{"type":"text","text":"{\"agent\":\"a3\",\"next\":\"call tazzy_next\"}"}]}`
	h := FromHook([]byte(join))
	if h.Key != "sub-1" || h.Agent != "a3" || h.Activity.Kind != "" {
		t.Errorf("join: %+v", h)
	}
	h = FromHook([]byte(`{"session_id":"S","hook_event_name":"PreToolUse","tool_name":"mcp__t__tazzy_next","tool_input":{"agent":"lead"}}`))
	if h.Key != "S" || h.Agent != "lead" || h.Activity.Kind != "" {
		t.Errorf("tazzy call: %+v", h)
	}
	h = FromHook([]byte(`{"session_id":"S","agent_id":"sub-1","hook_event_name":"PreToolUse","tool_name":"Write","tool_input":{"file_path":"/p/x.go","content":"x"}}`))
	if h.Key != "sub-1" || h.Agent != "" || h.Activity.Kind != Edit || h.Activity.Files[0] != "/p/x.go" || h.Activity.Source != SrcHook {
		t.Errorf("edit: %+v", h)
	}
	h = FromHook([]byte(`{"session_id":"S","hook_event_name":"PostToolUse","tool_name":"Bash","tool_input":{"command":"ls"},"tool_response":{"stdout":"a b","stderr":"","interrupted":false}}`))
	if h.Activity.Kind != CmdResult || h.Activity.Summary != "✓ Bash ls: a b" {
		t.Errorf("bash result: %+v", h.Activity)
	}
	h = FromHook([]byte(`{"session_id":"S","hook_event_name":"PostToolUse","tool_name":"Read","tool_input":{"file_path":"a"},"tool_response":{"content":"..."}}`))
	if h.Activity.Kind != "" {
		t.Errorf("successful read result recorded: %+v", h.Activity)
	}
	h = FromHook([]byte(`{"session_id":"S","agent_id":"sub-2","agent_type":"Explore","hook_event_name":"SubagentStop"}`))
	if h.Activity.Kind != State || h.Activity.Summary != "subagent Explore stopped" {
		t.Errorf("subagent: %+v", h.Activity)
	}
	if h.Transcript != "" {
		t.Errorf("subagent stop without paths: transcript %q", h.Transcript)
	}
	h = FromHook([]byte(`{"session_id":"S","agent_id":"sub-2","hook_event_name":"SubagentStop","transcript_path":"/p/S.jsonl","agent_transcript_path":"/p/S/subagents/agent-sub-2.jsonl"}`))
	if h.Transcript != "/p/S/subagents/agent-sub-2.jsonl" {
		t.Errorf("subagent stop transcript: %q", h.Transcript)
	}
	h = FromHook([]byte(`{"session_id":"S","agent_id":"sub-2","hook_event_name":"PostToolUse","tool_name":"Read","transcript_path":"/p/S.jsonl"}`))
	if h.Transcript != SubagentTranscript("/p/S.jsonl", "sub-2") {
		t.Errorf("subagent tool transcript: %q", h.Transcript)
	}
	h = FromHook([]byte(`{"session_id":"S","hook_event_name":"Stop","transcript_path":"/p/S.jsonl"}`))
	if h.Key != "S" || h.Transcript != "/p/S.jsonl" || h.Activity.Kind != "" {
		t.Errorf("stop: %+v", h)
	}
	h = FromHook([]byte(`{"session_id":"S","hook_event_name":"PreToolUse","tool_name":"Read","transcript_path":"/p/S.jsonl"}`))
	if h.Transcript != "" {
		t.Errorf("pre tool use read the transcript: %q", h.Transcript)
	}
	if h := FromHook([]byte(`nope`)); h.Key != "" || h.Activity.Kind != "" {
		t.Errorf("garbage: %+v", h)
	}
}
