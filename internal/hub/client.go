package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"tazzyronth/internal/proto"
	"tazzyronth/internal/store"
)

const dialTimeout = 5 * time.Second

// Client is an authenticated hub connection. Call is safe for concurrent use.
type Client struct {
	nc      net.Conn
	w       *proto.Writer
	closing chan struct{}
	once    sync.Once
	done    chan struct{} // closed when the read loop exits; err is then set

	mu      sync.Mutex
	err     error
	next    int64
	pending map[int64]chan *proto.Message
	events  chan store.Event
}

// Connect discovers the hub for projectDir via hub.json and connects to it.
func Connect(projectDir string) (*Client, error) {
	info, err := ReadInfo(projectDir)
	if err != nil {
		return nil, err
	}
	return Dial(info.Addr(), info.Token)
}

// Dial connects to addr and authenticates with token.
func Dial(addr, token string) (*Client, error) {
	nc, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		return nil, err
	}
	c := &Client{
		nc:      nc,
		w:       proto.NewWriter(nc),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
		pending: map[int64]chan *proto.Message{},
	}
	go c.readLoop()
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	if err := c.Call(ctx, "hello", HelloParams{Token: token}, nil); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) readLoop() {
	r := proto.NewReader(c.nc)
	var err error
	for {
		var m *proto.Message
		if m, err = r.Read(); err != nil {
			if errors.As(err, new(*proto.Error)) {
				continue
			}
			break
		}
		switch {
		case m.IsResponse():
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- m // buffered
			}
		case m.Method == "event":
			var ev store.Event
			c.mu.Lock()
			ch := c.events
			c.mu.Unlock()
			if ch == nil || json.Unmarshal(m.Params, &ev) != nil {
				continue
			}
			select {
			case ch <- ev:
			case <-c.closing:
			}
		}
	}
	c.mu.Lock()
	c.err = fmt.Errorf("hub connection closed: %w", err)
	if c.events != nil {
		close(c.events)
	}
	c.mu.Unlock()
	close(c.done)
}

// Call sends a request and decodes its result into result (which may be nil).
// A hub-side failure is returned as a *proto.Error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.next++
	id := c.next
	ch := make(chan *proto.Message, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	req, err := proto.NewRequest(id, method, params)
	if err != nil {
		return err
	}
	if err := c.w.Write(req); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result == nil {
			return nil
		}
		return json.Unmarshal(resp.Result, result)
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Subscribe asks the hub to stream every subsequent event. The channel must
// be drained; it is closed when the connection ends.
func (c *Client) Subscribe(ctx context.Context) (<-chan store.Event, error) {
	return c.subscribe(ctx, nil)
}

// SubscribeActivity is Subscribe plus the unlogged "activity" events, whose
// Data is an activity.Activity.
func (c *Client) SubscribeActivity(ctx context.Context) (<-chan store.Event, error) {
	return c.subscribe(ctx, EventsParams{Activity: true})
}

func (c *Client) subscribe(ctx context.Context, params any) (<-chan store.Event, error) {
	c.mu.Lock()
	if c.events == nil && c.err == nil {
		c.events = make(chan store.Event, subBuffer)
	}
	ch := c.events
	c.mu.Unlock()
	if err := c.Call(ctx, "events.subscribe", params, nil); err != nil {
		return nil, err
	}
	return ch, nil
}

func (c *Client) Close() error {
	var err error
	c.once.Do(func() {
		close(c.closing)
		err = c.nc.Close()
		<-c.done
	})
	return err
}
