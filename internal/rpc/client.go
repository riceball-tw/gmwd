package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
)

// ErrClosed is returned by calls on a closed or broken connection.
var ErrClosed = errors.New("rpc: connection closed")

// Client is a connection to the daemon. It is safe for concurrent use.
type Client struct {
	nc      net.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan message
	events  chan Event
	err     error // set once the connection is gone
	done    chan struct{}
}

// Dial connects to the daemon's socket.
func Dial(path string) (*Client, error) {
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	c := &Client{nc: nc, pending: map[int64]chan message{}, done: make(chan struct{})}
	go c.read()
	return c, nil
}

func (c *Client) Close() error {
	err := c.nc.Close()
	<-c.done
	return err
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Call invokes method and decodes the result into result (which may be nil).
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		raw = b
	}
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	line, err := json.Marshal(message{JSONRPC: "2.0", ID: &id, Method: method, Params: raw})
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	_, err = c.nc.Write(append(line, '\n'))
	c.writeMu.Unlock()
	if err != nil {
		c.forget(id)
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}

	select {
	case m, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	}
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Subscribe asks for events. The channel is closed when the connection
// ends. Events are dropped if the caller doesn't keep up.
func (c *Client) Subscribe(ctx context.Context) (<-chan Event, error) {
	c.mu.Lock()
	if c.events == nil {
		c.events = make(chan Event, 256)
	}
	ch := c.events
	c.mu.Unlock()
	if err := c.Call(ctx, MethodSubscribe, nil, nil); err != nil {
		return nil, err
	}
	return ch, nil
}

func (c *Client) read() {
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.ID == nil {
			if m.Method == NotifyEvent {
				c.deliver(m.Params)
			}
			continue
		}
		c.mu.Lock()
		ch := c.pending[*m.ID]
		delete(c.pending, *m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
	c.mu.Lock()
	c.err = ErrClosed
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	if c.events != nil {
		close(c.events)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) deliver(params json.RawMessage) {
	var ev Event
	if json.Unmarshal(params, &ev) != nil {
		return
	}
	c.mu.Lock()
	ch := c.events
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
	}
}
