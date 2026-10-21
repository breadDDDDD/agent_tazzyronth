package activity

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestDecodeAntigravity(t *testing.T) {
	got := decodeFile(t, "antigravity", "antigravity.jsonl")
	checkLines(t, got, []line{
		{Say, "Joining the swarm."},
		{Usage, "6999 tokens"},
		{Read, `read_file C:\repo\a.go`},
		{Edit, `write_file C:\repo\b.go`},
		{Usage, "8338 tokens"},
		{Cmd, "run_command go test ./..."},
		{CmdResult, `✖ run_command go test ./...: permission check failed for command "go test ./...": user denied permission to …`},
		{Cmd, "run_command Test-Path bye.txt"},
		{CmdResult, `✖ run_command Test-Path bye.txt: permission check failed for unsandboxed "Test-Path bye.txt": user denied permis…`},
		{Cmd, "run_command go test ./..."},
		{CmdResult, `✖ run_command go test ./...: permission check failed for command "go test ./...": user denied permission to …`},
		// Denied steps are named, deduped, in order (board decision AGY DENIAL FIX).
		{Alert, "denied (headless, no allow rule): command go test ./..., command Test-Path bye.txt"},
		{State, "run success, 16333 tokens"},
	})
	if len(got) > 3 && (len(got[3].Files) != 1 || got[3].Files[0] != `C:\repo\b.go`) {
		t.Errorf("Edit files = %v", got[3].Files)
	}
	var det struct{ Denied []AgyDenial }
	if len(got) > 11 {
		json.Unmarshal(got[11].Detail, &det)
	}
	if want := []AgyDenial{{"command", "go test ./..."}, {"command", "Test-Path bye.txt"}}; !slices.Equal(det.Denied, want) {
		t.Errorf("denial detail = %+v", det.Denied)
	}
	// Step usages plus the result's top-up add up to the result's totals.
	if u := sum(got); u != (Tokens{Input: 15480 - 6000, CacheRead: 6000, Output: 853}) {
		t.Errorf("usage = %+v", u)
	}
}

// TestDecodeAntigravityTopUp: usage reported only on the result (no step
// usage) is still counted, once; a failed run raises an alert.
func TestDecodeAntigravityTopUp(t *testing.T) {
	in := `{"event":"step_update","step_update":{"step_index":1,"state":"DONE","step_type":"agent_response","text_delta":"hi"}}
{"event":"result","result":{"status":"ERROR","error":{"message":"authentication required"},"usage":{"input_tokens":10,"output_tokens":5,"cache_read_tokens":0}}}`
	var out []Activity
	Decode("antigravity", strings.NewReader(in), func(a Activity) { out = append(out, a) })
	checkLines(t, out, []line{
		{Say, "hi"},
		{Alert, "error: authentication required"},
		{State, "run error, 15 tokens"},
	})
	if u := sum(out); u != (Tokens{Input: 10, Output: 5}) {
		t.Errorf("usage = %+v", u)
	}
}

// TestDecodeAntigravityDenialFallback: denied_actions with no denied step
// seen still alerts, naming the actions; non-command denials name the tool
// and its file; long commands are truncated.
func TestDecodeAntigravityDenialFallback(t *testing.T) {
	var out []Activity
	Decode("antigravity", strings.NewReader(`{"event":"result","result":{"status":"SUCCESS","denied_actions":[{"action":"command"},{"action":"command"}]}}`),
		func(a Activity) { out = append(out, a) })
	checkLines(t, out, []line{
		{Alert, "denied (headless, no allow rule): command"},
		{State, "run success"},
	})
	long := strings.Repeat("x", 120)
	in := `{"event":"step_update","step_update":{"step_index":1,"state":"ERROR","step_type":"tool","tool_name":"write_file","tool_info":{"name":"write_file","parameters":{"Path":"C:/r/a.go"},"error":{"type":"TOOL_ERROR","message":"permission check failed for write"}}}}
{"event":"step_update","step_update":{"step_index":2,"state":"ERROR","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"` + long + `"},"error":{"type":"TOOL_ERROR","message":"user denied permission to run command"}}}}
{"event":"step_update","step_update":{"step_index":3,"state":"ERROR","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"go vet"},"error":{"type":"TOOL_ERROR","message":"exit status 1"}}}}
{"event":"result","result":{"status":"SUCCESS","denied_actions":[{"action":"write"},{"action":"command"}]}}`
	out = nil
	Decode("antigravity", strings.NewReader(in), func(a Activity) { out = append(out, a) })
	var alert Activity
	for _, a := range out {
		if a.Kind == Alert {
			alert = a
		}
	}
	want := `denied (headless, no allow rule): write_file C:/r/a.go, command ` + strings.Repeat("x", 79) + "…"
	if alert.Summary != want {
		t.Errorf("alert = %q, want %q", alert.Summary, want)
	}
}
