package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/config"
	"tazzyronth/internal/graph"
)

func TestWebView(t *testing.T) {
	cfg := config.Default()
	dir := t.TempDir()
	h, err := StartWith(dir, Options{Config: &cfg, Web: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	info, err := ReadInfo(dir)
	if err != nil || info.Web == 0 || info.Web != h.Info().Web {
		t.Fatalf("hub.json web port: %+v, %v", info, err)
	}
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	var res WebResult
	if err := c.Call(ctx, "web.start", nil, &res); err != nil || !strings.Contains(res.URL, "token="+info.Token) {
		t.Fatalf("web.start: %+v, %v", res, err)
	}
	if again, _ := h.StartWeb(); again != res.URL {
		t.Errorf("second start gave %s", again)
	}

	api := strings.Replace(res.URL, "/?", "/api/snapshot?", 1)
	resp, err := http.Get(api)
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	for _, k := range []string{"agents", "tasks", "swarm", "usage", "leases", "queues"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot has no %q: %v", k, snap)
		}
	}

	resp, err = http.Get(strings.Replace(res.URL, "/?", "/api/events?", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %q", want)
				}
				if strings.Contains(l, want) {
					return
				}
			case <-deadline:
				t.Fatalf("no %q on the stream", want)
			}
		}
	}
	waitFor("event: status")
	if err := c.Call(ctx, "task.add", TaskAddParams{Tasks: []graph.Task{{ID: "web-task"}}}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(`"web-task"`)
	waitFor("event: status")

	// An open stream counts as a connection for idle shutdown.
	h.mu.Lock()
	taps := len(h.taps)
	h.mu.Unlock()
	if taps != 1 {
		t.Errorf("taps = %d", taps)
	}
}

func TestWorkerAdapterFollowsHost(t *testing.T) {
	cfg := config.Default()
	cfg.SpawnMode = config.SpawnHost // never spawn a real CLI here
	dir := t.TempDir()
	h, err := StartWith(dir, Options{Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	c, err := Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	set := func(p SwarmParams) string {
		var res SwarmParams
		if err := c.Call(context.Background(), "swarm.set", p, &res); err != nil {
			t.Fatal(err)
		}
		return res.Adapter
	}
	if got := set(SwarmParams{Client: "codex-mcp-client"}); got != "codex" {
		t.Errorf("codex lead: adapter %q", got)
	}
	if got := set(SwarmParams{Client: "gemini-cli-mcp-client"}); got != "gemini" {
		t.Errorf("gemini lead: adapter %q", got)
	}
	if got := set(SwarmParams{Client: "codex-mcp-client", Adapter: "claude"}); got != "claude" {
		t.Errorf("explicit adapter: %q", got)
	}
	if got := set(SwarmParams{Client: "some-ide"}); got != "claude" {
		t.Errorf("unknown host: %q", got)
	}
}
