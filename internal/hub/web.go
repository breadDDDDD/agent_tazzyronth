package hub

import (
	"encoding/json"
	"errors"

	"tazzyronth/internal/store"
	"tazzyronth/internal/web"
)

// WebResult is the result of web.start.
type WebResult struct {
	URL string `json:"url"` // carries the token
}

// StartWeb starts the web view (plan §6.4) if it isn't running and returns
// its URL. The port is recorded in hub.json.
func (h *Hub) StartWeb() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", errors.New("hub is closing")
	}
	if h.web == nil {
		s, err := web.Start(h.info.Token, webSource{h})
		if err != nil {
			return "", err
		}
		h.web = s
		h.info.Web = s.Port()
		if err := writeInfo(h.dir, h.info); err != nil {
			return "", err
		}
	}
	return h.web.URL(), nil
}

// webSource exposes the hub to the web server in-process.
type webSource struct{ h *Hub }

func (s webSource) Status() any {
	res, _ := status(s.h, nil, nil)
	return res
}

func (s webSource) Activity(agent string, after int64, limit int) any {
	return s.h.act.Recent(agent, after, limit)
}

func (s webSource) Detail(id int64) (json.RawMessage, error) { return s.h.act.Detail(id) }

// Tap is an in-process events.subscribe {activity: true}. Like a connection,
// an open tap keeps the hub from going idle.
func (s webSource) Tap() (<-chan store.Event, func()) {
	h := s.h
	ch := make(chan store.Event, subBuffer)
	h.mu.Lock()
	if h.closed {
		close(ch)
	} else {
		h.taps[ch] = struct{}{}
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		h.untapLocked(ch)
		h.mu.Unlock()
	}
}

// untapLocked removes and closes a tap once. Called with h.mu held.
func (h *Hub) untapLocked(ch chan store.Event) {
	if _, ok := h.taps[ch]; ok {
		delete(h.taps, ch)
		close(ch)
	}
}

func webStart(h *Hub, _ *conn, _ json.RawMessage) (any, error) {
	url, err := h.StartWeb()
	if err != nil {
		return nil, err
	}
	return WebResult{URL: url}, nil
}
