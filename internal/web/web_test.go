package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"tazzyronth/internal/store"
)

type fakeSource struct{ ch chan store.Event }

func (f *fakeSource) Status() any { return map[string]any{"agents": []string{"a1"}} }
func (f *fakeSource) Activity(agent string, after int64, limit int) any {
	return []map[string]any{{"id": after + 1, "agent": agent, "limit": limit}}
}
func (f *fakeSource) Detail(id int64) (json.RawMessage, error) {
	if id != 7 {
		return nil, errors.New("no such activity")
	}
	return json.RawMessage(`{"path":"x.go"}`), nil
}
func (f *fakeSource) Tap() (<-chan store.Event, func()) { return f.ch, func() {} }

func start(t *testing.T) (*Server, *fakeSource) {
	t.Helper()
	src := &fakeSource{ch: make(chan store.Event, 8)}
	s, err := Start("secret", src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, src
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func get(t *testing.T, url string, mod func(*http.Request)) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if mod != nil {
		mod(req)
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestAuth(t *testing.T) {
	s, _ := start(t)
	if a := s.ln.Addr().String(); !strings.HasPrefix(a, "127.0.0.1:") {
		t.Errorf("listening on %s, want loopback only", a)
	}
	base := strings.TrimSuffix(s.URL(), "/?token=secret")

	for _, path := range []string{"/", "/api/snapshot", "/api/events", "/logo.png", "/?token=wrong"} {
		if r := get(t, base+path, nil); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: %d", path, r.StatusCode)
		}
	}
	if r := get(t, s.URL(), func(r *http.Request) { r.Host = "evil.example:80" }); r.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Host: %d", r.StatusCode)
	}

	// The launched URL sets the cookie and drops the token from the address bar.
	r := get(t, s.URL(), nil)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("token URL: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == s.cookie {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != "secret" || !cookie.HttpOnly {
		t.Fatalf("cookie = %+v", cookie)
	}
	withCookie := func(r *http.Request) { r.AddCookie(cookie) }
	r = get(t, base+"/", withCookie)
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(body), "AGENT TAZZYRONTH") {
		t.Errorf("page with cookie: %d", r.StatusCode)
	}
	if len(page) > 25<<10 {
		t.Errorf("page is %d bytes", len(page))
	}
	if r := get(t, base+"/logo.png", withCookie); r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" {
		t.Errorf("logo: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	if r := get(t, base+"/api/snapshot?token=secret", nil); r.StatusCode != 200 {
		t.Errorf("api with token param: %d", r.StatusCode)
	}
}

func TestJSON(t *testing.T) {
	s, _ := start(t)
	base := strings.TrimSuffix(s.URL(), "/?token=secret")
	decode := func(path string, v any) int {
		r := get(t, base+path+"?token=secret", nil)
		if r.StatusCode == 200 {
			if err := json.NewDecoder(r.Body).Decode(v); err != nil {
				t.Errorf("%s: %v", path, err)
			}
		}
		return r.StatusCode
	}
	var snap struct{ Agents []string }
	if code := decode("/api/snapshot", &snap); code != 200 || len(snap.Agents) != 1 {
		t.Errorf("snapshot: %d %+v", code, snap)
	}
	r := get(t, base+"/api/activity?token=secret&agent=w1&after=4&limit=5000", nil)
	var acts []struct {
		ID    int64
		Agent string
		Limit int
	}
	json.NewDecoder(r.Body).Decode(&acts)
	if len(acts) != 1 || acts[0].ID != 5 || acts[0].Agent != "w1" || acts[0].Limit != 1000 {
		t.Errorf("activity: %+v", acts)
	}
	var d struct{ Path string }
	if r := get(t, base+"/api/detail?token=secret&id=7", nil); r.StatusCode != 200 || json.NewDecoder(r.Body).Decode(&d) != nil || d.Path != "x.go" {
		t.Errorf("detail: %d %+v", r.StatusCode, d)
	}
	if r := get(t, base+"/api/detail?token=secret&id=8", nil); r.StatusCode != 404 {
		t.Errorf("missing detail: %d", r.StatusCode)
	}
}

// sse reads "event: name" / "data: ..." pairs.
func sse(t *testing.T, body io.Reader) <-chan [2]string {
	out := make(chan [2]string, 16)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		var name string
		for sc.Scan() {
			line := sc.Text()
			if n, ok := strings.CutPrefix(line, "event: "); ok {
				name = n
			} else if d, ok := strings.CutPrefix(line, "data: "); ok {
				out <- [2]string{name, d}
			}
		}
	}()
	return out
}

func next(t *testing.T, ch <-chan [2]string) [2]string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE message")
	}
	return [2]string{}
}

func TestEvents(t *testing.T) {
	s, src := start(t)
	r := get(t, strings.Replace(s.URL(), "/?", "/api/events?", 1), nil)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("events: %d %s", r.StatusCode, r.Header.Get("Content-Type"))
	}
	msgs := sse(t, r.Body)
	if m := next(t, msgs); m[0] != "status" || !strings.Contains(m[1], `"a1"`) {
		t.Errorf("first: %v", m)
	}
	src.ch <- store.Event{Type: "activity", Data: json.RawMessage(`{"id":3}`)}
	if m := next(t, msgs); m != [2]string{"activity", `{"id":3}`} {
		t.Errorf("activity: %v", m)
	}
	src.ch <- store.Event{Seq: 9, Type: "task.add", Data: json.RawMessage(`{}`)}
	if m := next(t, msgs); m[0] != "event" || !strings.Contains(m[1], `"task.add"`) {
		t.Errorf("event: %v", m)
	}
	if m := next(t, msgs); m[0] != "status" {
		t.Errorf("after a logged event: %v", m)
	}
}
