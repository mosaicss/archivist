package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"sync"

	"github.com/sourcegraph/jsonrpc2"
)

// codexStream is a jsonrpc2.ObjectStream over a Codex app-server's stdio:
// one JSON object per line. jsonrpc2 closes the whole connection on a frame
// it cannot classify, so lines that are not a JSON-RPC message (banners,
// stray output) are skipped and logged here instead.
type codexStream struct {
	p   *Proc
	log *Logger
}

// ReadObject implements jsonrpc2.ObjectStream.
func (s *codexStream) ReadObject(v any) error {
	for line := range s.p.Lines() {
		if !jsonrpcFrame(line) {
			s.log.Printf("codex: skipped a stdout line that is not a JSON-RPC message (%d bytes)", len(line))
			continue
		}
		return json.Unmarshal(line, v)
	}
	return io.EOF
}

// WriteObject implements jsonrpc2.ObjectStream.
func (s *codexStream) WriteObject(obj any) error { return s.p.WriteJSON(obj) }

// Close implements jsonrpc2.ObjectStream: end of input for the app-server.
func (s *codexStream) Close() error {
	s.p.CloseStdin()
	return nil
}

// jsonrpcFrame reports whether line is one JSON-RPC message jsonrpc2 can
// classify: an object with a string method (request or notification, id
// absent, a non-negative integer or a string) or with result/error and an
// id (response).
func jsonrpcFrame(line []byte) bool {
	var f map[string]json.RawMessage
	if json.Unmarshal(line, &f) != nil || f == nil {
		return false
	}
	id, hasID := f["id"]
	if hasID && !validRPCID(id) {
		return false
	}
	if m, ok := f["method"]; ok {
		var method string
		_, hasResult := f["result"]
		_, hasError := f["error"]
		return json.Unmarshal(m, &method) == nil && method != "" && !hasResult && !hasError
	}
	_, hasResult := f["result"]
	errRaw, hasError := f["error"]
	if hasError && string(bytes.TrimSpace(errRaw)) == "null" {
		hasError = false
	}
	return hasID && (hasResult || hasError)
}

func validRPCID(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return true
	}
	_, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 64)
	return err == nil
}

// codexEvent is one Codex message for the session loop: a notification, a
// server request awaiting exactly one reply, or the completion of a call the
// daemon dispatched (Call set).
type codexEvent struct {
	Method string
	Notif  bool
	ID     jsonrpc2.ID
	Params json.RawMessage

	Call   string // tag of a dispatched call that completed
	Result json.RawMessage
	Err    error
}

// codexQueue is the unbounded, ordered hand-off from the jsonrpc2 read
// loop to the session loop. push never blocks, so a slow relay or a busy
// session can never stall Codex's reader.
type codexQueue struct {
	mu    sync.Mutex
	items []codexEvent
	wake  chan struct{}
}

func newCodexQueue() *codexQueue { return &codexQueue{wake: make(chan struct{}, 1)} }

func (q *codexQueue) push(e codexEvent) {
	q.mu.Lock()
	q.items = append(q.items, e)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// take removes and returns every queued event.
func (q *codexQueue) take() []codexEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.items
	q.items = nil
	return out
}

// unshift puts events back at the front, in order.
func (q *codexQueue) unshift(evs []codexEvent) {
	if len(evs) == 0 {
		return
	}
	q.mu.Lock()
	q.items = append(append([]codexEvent(nil), evs...), q.items...)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// codexHandler queues every request and notification; it runs inline in
// jsonrpc2's read loop and therefore never calls back into the connection.
type codexHandler struct{ q *codexQueue }

// Handle implements jsonrpc2.Handler.
func (h codexHandler) Handle(_ context.Context, _ *jsonrpc2.Conn, r *jsonrpc2.Request) {
	e := codexEvent{Method: r.Method, Notif: r.Notif, ID: r.ID}
	if r.Params != nil {
		e.Params = append(json.RawMessage(nil), *r.Params...)
	}
	h.q.push(e)
}

// codexRPC is the live JSON-RPC connection to one app-server.
type codexRPC struct {
	conn   *jsonrpc2.Conn
	q      *codexQueue
	cancel context.CancelFunc
}

func newCodexRPC(p *Proc, log *Logger) *codexRPC {
	ctx, cancel := context.WithCancel(context.Background())
	q := newCodexQueue()
	conn := jsonrpc2.NewConn(ctx, &codexStream{p: p, log: log}, codexHandler{q: q}, jsonrpc2.SetLogger(log))
	return &codexRPC{conn: conn, q: q, cancel: cancel}
}

// dispatch sends a call without waiting; its completion arrives on the
// queue as an event tagged tag.
func (c *codexRPC) dispatch(method string, params any, tag string) {
	w, err := c.conn.DispatchCall(context.Background(), method, params)
	if err != nil {
		c.q.push(codexEvent{Call: tag, Err: err})
		return
	}
	go func() {
		var raw json.RawMessage
		err := w.Wait(context.Background(), &raw)
		c.q.push(codexEvent{Call: tag, Result: raw, Err: err})
	}()
}

// reply answers a server request with a result.
func (c *codexRPC) reply(id jsonrpc2.ID, result any) error {
	return c.conn.Reply(context.Background(), id, result)
}

// replyError answers a server request with a JSON-RPC error.
func (c *codexRPC) replyError(id jsonrpc2.ID, code int64, msg string) error {
	return c.conn.ReplyWithError(context.Background(), id, &jsonrpc2.Error{Code: code, Message: msg})
}

func (c *codexRPC) close() {
	c.cancel()
	_ = c.conn.Close()
}

// rpcIDString renders a request id as the 78.13 approval ids do
// (String(f.id) in normalize.ts): decimal for numbers, the text for strings.
func rpcIDString(id jsonrpc2.ID) string {
	if id.IsString {
		return id.Str
	}
	return strconv.FormatUint(id.Num, 10)
}
