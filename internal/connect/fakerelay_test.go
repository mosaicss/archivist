//go:build !windows

package connect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// fakeRelay is a minimal in-process stand-in for the 78.14 relay's daemon
// side: it accepts daemon sockets, stores and acks events (deduped by
// correlationId), validates every event with the mosaic-event/1 parser,
// redelivers queued commands on each connect until acked, and records
// capabilities and acks. The wrangler-backed e2e test covers the real relay.
type fakeRelay struct {
	t      *testing.T
	srv    *httptest.Server
	parser *mosaicevent.Parser

	mu       sync.Mutex
	user     *websocket.Conn
	sessions map[string]*websocket.Conn
	caps     [][]Capability
	events   map[string][]map[string]any
	seen     map[string]bool
	acks     []map[string]any
	queued   map[string][]map[string]any // session id ("" = user) -> unacked commands
	invalid  []string
	dials    map[string]int
}

func newFakeRelay(t *testing.T) *fakeRelay {
	p, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRelay{t: t, parser: p, sessions: map[string]*websocket.Conn{}, events: map[string][]map[string]any{},
		seen: map[string]bool{}, queued: map[string][]map[string]any{}, dials: map[string]int{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRelay) url() string { return "ws" + strings.TrimPrefix(r.srv.URL, "http") }

func (r *fakeRelay) serve(w http.ResponseWriter, req *http.Request) {
	if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer eyJ") || req.URL.Query().Get("role") != "daemon" {
		http.Error(w, `{"code":"MISSING_CREDENTIALS"}`, http.StatusUnauthorized)
		return
	}
	sid := ""
	switch {
	case req.URL.Path == "/users/"+testOwner+"/ws":
	case strings.HasPrefix(req.URL.Path, "/sessions/") && strings.HasSuffix(req.URL.Path, "/ws"):
		sid = strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/sessions/"), "/ws")
	default:
		http.Error(w, `{"code":"NOT_FOUND"}`, http.StatusNotFound)
		return
	}
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	r.mu.Lock()
	r.dials[sid]++
	var old *websocket.Conn
	if sid == "" {
		old, r.user = r.user, c
	} else {
		old, r.sessions[sid] = r.sessions[sid], c
	}
	pending := append([]map[string]any(nil), r.queued[sid]...)
	r.mu.Unlock()
	if old != nil {
		_ = old.Close(closeSuperseed, "superseded")
	}
	ctx := context.Background()
	for _, cmd := range pending {
		b, _ := json.Marshal(cmd)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if string(data) == "ping" {
			_ = c.Write(ctx, websocket.MessageText, []byte("pong"))
			continue
		}
		var x map[string]any
		if json.Unmarshal(data, &x) != nil {
			continue
		}
		switch x["kind"] {
		case "capabilities":
			var caps struct {
				Agents []Capability `json:"agents"`
			}
			_ = json.Unmarshal(data, &caps)
			r.mu.Lock()
			r.caps = append(r.caps, caps.Agents)
			r.mu.Unlock()
		case "command_ack":
			r.mu.Lock()
			r.acks = append(r.acks, x)
			cid, _ := x["correlationId"].(string)
			q := r.queued[sid]
			for i, cmd := range q {
				if cmd["correlationId"] == cid {
					r.queued[sid] = append(q[:i], q[i+1:]...)
					break
				}
			}
			r.mu.Unlock()
		case "event":
			cid, _ := x["correlationId"].(string)
			if len(data) > maxFrameBytes {
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"kind":"error","code":"FRAME_TOO_LARGE"}`))
				continue
			}
			if _, err := r.parser.Parse(data, "envelope"); err != nil || x["origin"] != "daemon" {
				r.mu.Lock()
				r.invalid = append(r.invalid, string(data))
				r.mu.Unlock()
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"kind":"error","code":"INVALID_EVENT"}`))
				continue
			}
			r.mu.Lock()
			dup := r.seen[cid]
			if !dup {
				r.seen[cid] = true
				r.events[sid] = append(r.events[sid], x)
			}
			seq := len(r.events[sid])
			r.mu.Unlock()
			ack, _ := json.Marshal(map[string]any{"kind": "ack", "correlationId": cid, "seq": seq, "duplicate": dup})
			_ = c.Write(ctx, websocket.MessageText, ack)
		}
	}
}

// send queues a command for the user ("" ) or a session socket and pushes it
// when that socket is connected.
func (r *fakeRelay) send(sid string, cmd map[string]any) {
	r.mu.Lock()
	r.queued[sid] = append(r.queued[sid], cmd)
	c := r.user
	if sid != "" {
		c = r.sessions[sid]
	}
	r.mu.Unlock()
	if c != nil {
		b, _ := json.Marshal(cmd)
		_ = c.Write(context.Background(), websocket.MessageText, b)
	}
}

// sendRaw writes raw bytes on a socket without queueing (closed-set tests).
func (r *fakeRelay) sendRaw(sid string, raw string) {
	r.mu.Lock()
	c := r.user
	if sid != "" {
		c = r.sessions[sid]
	}
	r.mu.Unlock()
	if c == nil {
		r.t.Fatalf("socket %q not connected", sid)
	}
	_ = c.Write(context.Background(), websocket.MessageText, []byte(raw))
}

func (r *fakeRelay) closeUser(code websocket.StatusCode) {
	r.mu.Lock()
	c := r.user
	r.mu.Unlock()
	if c != nil {
		_ = c.Close(code, "test")
	}
}

func (r *fakeRelay) payloads(sid string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, e := range r.events[sid] {
		p, _ := e["payload"].(map[string]any)
		out = append(out, p)
	}
	return out
}

// eventsOf returns envelopes of one payload type.
func (r *fakeRelay) eventsOf(sid, typ string) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, e := range r.events[sid] {
		if e["type"] == typ {
			out = append(out, e)
		}
	}
	return out
}

func (r *fakeRelay) statuses(sid string) []string {
	var out []string
	for _, p := range r.payloads(sid) {
		if p["type"] == "data-session-status" {
			d, _ := p["data"].(map[string]any)
			out = append(out, d["status"].(string))
		}
	}
	return out
}

// text concatenates text deltas from index from onward.
func (r *fakeRelay) text(sid string) string {
	var b strings.Builder
	for _, p := range r.payloads(sid) {
		if p["type"] == "text-delta" {
			b.WriteString(p["delta"].(string))
		}
	}
	return b.String()
}

func (r *fakeRelay) acked(cid string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.acks {
		if a["correlationId"] == cid {
			return true
		}
	}
	return false
}

func (r *fakeRelay) lastCaps() []Capability {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.caps) == 0 {
		return nil
	}
	return r.caps[len(r.caps)-1]
}

func (r *fakeRelay) dialCount(sid string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dials[sid]
}

// closeSession closes a session socket the way the relay does at end or
// expiry (4001).
func (r *fakeRelay) closeSession(sid string, code websocket.StatusCode) {
	r.mu.Lock()
	c := r.sessions[sid]
	r.mu.Unlock()
	if c != nil {
		_ = c.Close(code, "session ended")
	}
}

func (r *fakeRelay) waitStatus(t *testing.T, sid, status string, n int) {
	t.Helper()
	waitFor(t, 20*time.Second, "status "+status, func() bool {
		c := 0
		for _, s := range r.statuses(sid) {
			if s == status {
				c++
			}
		}
		return c >= n
	})
}
