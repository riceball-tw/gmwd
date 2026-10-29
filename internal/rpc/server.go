package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// message is any JSON-RPC 2.0 message: request, response or notification.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// maxLine bounds one message; a topic prompt is the biggest thing we send.
const maxLine = 16 << 20

// Handler serves one method. params is null when the caller sent none.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Server dispatches requests to handlers and pushes events to subscribers.
type Server struct {
	mu       sync.Mutex
	handlers map[string]Handler
	conns    map[*serverConn]struct{}
}

func NewServer() *Server {
	return &Server{handlers: map[string]Handler{}, conns: map[*serverConn]struct{}{}}
}

// Handle registers h for method. Call before Serve.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Serve accepts connections until ctx is done, then closes the listener
// and every connection.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer func() {
		s.mu.Lock()
		for c := range s.conns {
			c.close()
		}
		s.mu.Unlock()
		wg.Wait()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		c := &serverConn{nc: nc, out: make(chan []byte, 256), done: make(chan struct{})}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.serveConn(ctx, c)
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
		}()
	}
}

// Notify sends ev to every subscribed client. A client too slow to keep up
// is disconnected rather than allowed to block the daemon.
func (s *Server) Notify(ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	params, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line, err := json.Marshal(message{JSONRPC: "2.0", Method: NotifyEvent, Params: params})
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if c.subscribed.Load() {
			c.send(line, true)
		}
	}
}

type serverConn struct {
	nc         net.Conn
	out        chan []byte
	done       chan struct{}
	closeOnce  sync.Once
	subscribed atomic.Bool
}

func (c *serverConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.nc.Close()
	})
}

// send queues a line. With dropSlow, a full queue disconnects the client.
func (c *serverConn) send(line []byte, dropSlow bool) {
	if dropSlow {
		select {
		case c.out <- line:
		case <-c.done:
		default:
			c.close()
		}
		return
	}
	select {
	case c.out <- line:
	case <-c.done:
	}
}

func (s *Server) serveConn(ctx context.Context, c *serverConn) {
	defer c.close()
	go func() {
		w := bufio.NewWriter(c.nc)
		for {
			select {
			case line := <-c.out:
				w.Write(line)
				w.WriteByte('\n')
				if len(c.out) == 0 {
					if w.Flush() != nil {
						c.close()
						return
					}
				}
			case <-c.done:
				return
			}
		}
	}()

	var reqs sync.WaitGroup
	defer reqs.Wait()
	sc := bufio.NewScanner(c.nc)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			c.reply(nil, nil, &Error{Code: CodeParse, Message: "parse error: " + err.Error()})
			continue
		}
		if m.Method == "" || m.JSONRPC != "2.0" {
			c.reply(m.ID, nil, &Error{Code: CodeInvalidRequest, Message: "invalid request"})
			continue
		}
		reqs.Add(1)
		go func() {
			defer reqs.Done()
			res, err := s.dispatch(ctx, c, m)
			if m.ID == nil {
				return // notification: no reply
			}
			if err != nil {
				c.reply(m.ID, nil, toError(err))
				return
			}
			c.reply(m.ID, res, nil)
		}()
	}
}

func (s *Server) dispatch(ctx context.Context, c *serverConn, m message) (any, error) {
	if m.Method == MethodSubscribe {
		c.subscribed.Store(true)
		return nil, nil
	}
	s.mu.Lock()
	h, ok := s.handlers[m.Method]
	s.mu.Unlock()
	if !ok {
		return nil, &Error{Code: CodeMethodNotFound, Message: "unknown method " + m.Method}
	}
	params := m.Params
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	return h(ctx, params)
}

func (c *serverConn) reply(id *int64, result any, rerr *Error) {
	m := message{JSONRPC: "2.0", ID: id, Error: rerr}
	if rerr == nil {
		raw, err := json.Marshal(result)
		if err != nil {
			m.Error = &Error{Code: CodeInternal, Message: "encode result: " + err.Error()}
		} else {
			m.Result = raw
		}
	}
	line, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.send(line, false)
}

// Decode unmarshals params into v, mapping failures to CodeInvalidParams.
func Decode(params json.RawMessage, v any) error {
	if err := json.Unmarshal(params, v); err != nil {
		return &Error{Code: CodeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	return nil
}
