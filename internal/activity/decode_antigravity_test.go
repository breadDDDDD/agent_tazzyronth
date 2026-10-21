package activity

import (
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
		{Alert, "denied (headless, no allow rule): command"},
		{State, "run success, 16333 tokens"},
	})
	if len(got) > 3 && (len(got[3].Files) != 1 || got[3].Files[0] != `C:\repo\b.go`) {
		t.Errorf("Edit files = %v", got[3].Files)
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
