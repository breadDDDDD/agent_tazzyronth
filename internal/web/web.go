// Package web is the optional browser dashboard (plan §6.4): one embedded
// page, JSON endpoints for the initial state and Server-Sent Events for live
// updates, served on loopback behind the hub's token.
package web

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"tazzyronth/assets"
	"tazzyronth/internal/store"
)

//go:embed index.html
var page []byte

// Source is what the server shows; the hub implements it in-process.
type Source interface {
	Status() any                                       // the hub snapshot incl. usage
	Activity(agent string, after int64, limit int) any // like activity.list
	Detail(id int64) (json.RawMessage, error)          // like activity.detail
	// Tap streams every hub event plus activity events until cancel is
	// called. The channel is closed if the reader falls behind.
	Tap() (events <-chan store.Event, cancel func())
}

const (
	statusDelay = 300 * time.Millisecond // a fresh status follows logged events
	pingEvery   = 15 * time.Second
)

type Server struct {
	src    Source
	token  string
	cookie string
	ln     net.Listener
	http   *http.Server
}

// Start serves src on a random 127.0.0.1 port.
func Start(token string, src Source) (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{src: src, token: token, ln: ln}
	// Cookies ignore the port, so each hub gets its own name.
	s.cookie = "tazzy_" + strconv.Itoa(s.Port())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(assets.LogoPNG)
	})
	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, src.Status()) })
	mux.HandleFunc("GET /api/activity", s.activity)
	mux.HandleFunc("GET /api/detail", s.detail)
	mux.HandleFunc("GET /api/events", s.events)
	s.http = &http.Server{Handler: s.auth(mux), ReadHeaderTimeout: 10 * time.Second}
	go s.http.Serve(ln)
	return s, nil
}

func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// URL opens the page; it carries the token, which the page swaps for a cookie.
func (s *Server) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/?token=%s", s.Port(), s.token)
}

// Close drops every connection, live event streams included.
func (s *Server) Close() error { return s.http.Close() }

// auth accepts the token as ?token= (then set as a cookie) or the cookie.
// The Host check stops DNS-rebinding pages from reading the API.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if host, _, err := net.SplitHostPort(r.Host); err != nil || host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		tok := r.URL.Query().Get("token")
		if tok == "" {
			if c, err := r.Cookie(s.cookie); err == nil {
				tok = c.Value
			}
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized: open the dashboard URL from tazzy_start or hub.json (it carries the token)", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Has("token") {
			http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			if r.URL.Path == "/" { // keep the token out of the address bar and history
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	writeJSON(w, s.src.Activity(q.Get("agent"), after, min(limit, 1000)))
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	d, err := s.src.Detail(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, d)
}

// events streams "status" (the snapshot: first, then shortly after any
// logged change), "activity" (an activity.Activity) and "event" (a logged
// store.Event) messages.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.src.Tap()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(name string, data any) error {
		b, ok := data.(json.RawMessage)
		if !ok {
			var err error
			if b, err = json.Marshal(data); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	}
	if send("status", s.src.Status()) != nil {
		return
	}
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	var refresh <-chan time.Time
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // hub closing, or we fell behind: the browser reconnects
			}
			if ev.Type == "activity" {
				err = send("activity", ev.Data)
				break
			}
			err = send("event", ev)
			if refresh == nil {
				refresh = time.After(statusDelay)
			}
		case <-refresh:
			refresh = nil
			err = send("status", s.src.Status())
		case <-ping.C:
			if _, err = fmt.Fprint(w, ": ping\n\n"); err == nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
