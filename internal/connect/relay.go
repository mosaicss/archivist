package connect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/mosaicss/archivist/internal/client"
)

// Relay timings (78.14): presence goes stale after 60 s without a frame, so
// the daemon pings every 20 s; tickets live at most 5 minutes, so sockets
// rotate before expiry.
const (
	rotateBefore   = 30 * time.Second
	writeTimeout   = 10 * time.Second
	readLimit      = 32 << 20
	stableAfter    = 10 * time.Second
	closeSuperseed = 4000
	closeExpired   = 4001
)

// pingInterval is the application ping cadence (a var so tests can shorten it).
var pingInterval = 20 * time.Second

// DefaultRelayURL is the production relay. ARCHIVIST_RELAY_URL overrides it.
const DefaultRelayURL = "wss://relay.mosaic-finance.com"

// API is the chat-api surface the daemon uses (Story 78.15 routes).
type API interface {
	MintRelayTicket(ctx context.Context, sessionID string) (*client.RelayTicket, error)
	MintTaskToken(ctx context.Context, sessionID string, scopes []string, ttlSeconds int) (*client.TaskToken, error)
	RevokeTaskToken(ctx context.Context, tokenID string) error
	GetAgentSession(ctx context.Context, sessionID string) (*client.AgentSession, error)
}

// ErrSuperseded means another daemon connected for this user (close 4000).
var ErrSuperseded = errors.New("another archivist connect took over")

// ErrSessionInactive means the relay or chat-api reports the session ended.
var ErrSessionInactive = errors.New("session is no longer active")

// FatalError stops the daemon with a typed exit (feature off, auth refused).
type FatalError struct {
	Code    string
	Message string
	Auth    bool
}

func (e *FatalError) Error() string { return e.Message }

// backoff is the archivist client schedule (250, 500, 1000 ms doubling,
// ±25% jitter) capped at 30 s.
type backoff struct{ n int }

func (b *backoff) next() time.Duration {
	d := 250 * time.Millisecond << min(b.n, 7)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	b.n++
	jitter := float64(d) * 0.25
	return d + time.Duration(rand.Int63n(int64(2*jitter)+1)-int64(jitter))
}

func (b *backoff) reset() { b.n = 0 }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ticketSubject reads the owner (sub) from a relay ticket's claims. The
// signature is the relay's to verify; the daemon only needs the route.
func ticketSubject(ticket string) (string, error) {
	payload, _, ok := strings.Cut(ticket, ".")
	if !ok {
		return "", fmt.Errorf("malformed relay ticket")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("malformed relay ticket claims")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub == "" {
		return "", fmt.Errorf("relay ticket has no subject")
	}
	return claims.Sub, nil
}

// classifyMint maps a chat-api mint failure: fatal (stop the daemon),
// gone (the session ended) or transient (retry with backoff).
func classifyMint(err error, session bool) (fatal *FatalError, gone bool) {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Code == "FEATURE_DISABLED":
			return &FatalError{Code: apiErr.Code, Message: "Mosaic agent sessions are not enabled on this server (FEATURE_DISABLED)."}, false
		case apiErr.Status == http.StatusUnauthorized:
			return &FatalError{Code: "UNAUTHORIZED", Auth: true, Message: "chat-api rejected the credential (invalid or revoked ak_ key)."}, false
		case apiErr.Status == http.StatusForbidden:
			return &FatalError{Code: apiErr.Code, Auth: true, Message: fmt.Sprintf("chat-api refused the credential: %s", apiErr.Error())}, false
		case session && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusConflict || apiErr.Status == http.StatusBadRequest):
			return nil, true
		case apiErr.Status == http.StatusBadRequest:
			return &FatalError{Code: apiErr.Code, Message: "chat-api refused the request: " + apiErr.Error()}, false
		}
		return nil, false
	}
	var exitErr *client.ExitCodeError
	if errors.As(err, &exitErr) && exitErr.APICode == "MIN_CLI_VERSION" {
		return &FatalError{Code: exitErr.APICode, Message: exitErr.Message}, false
	}
	return nil, false
}

// live is one open relay socket.
type live struct {
	ws      *websocket.Conn
	ctrl    chan []byte
	done    chan struct{}
	retired atomic.Bool
	errMu   sync.Mutex
	err     error

	mu       sync.Mutex
	inflight []string
}

// Send queues a control frame (ack, capabilities) on this socket.
func (l *live) Send(b []byte) bool {
	if l.retired.Load() {
		return false
	}
	select {
	case l.ctrl <- b:
		return true
	case <-l.done:
		return false
	default:
		return false
	}
}

func (l *live) closeErr() error {
	l.errMu.Lock()
	defer l.errMu.Unlock()
	return l.err
}

func (l *live) retire() {
	if l.retired.CompareAndSwap(false, true) {
		_ = l.ws.CloseNow()
	}
}

func (l *live) pushInflight(cid string) {
	l.mu.Lock()
	l.inflight = append(l.inflight, cid)
	l.mu.Unlock()
}

func (l *live) popInflight(cid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, c := range l.inflight {
		if c == cid {
			l.inflight = append(l.inflight[:i], l.inflight[i+1:]...)
			return
		}
	}
}

func (l *live) popHead() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.inflight) == 0 {
		return "", false
	}
	h := l.inflight[0]
	l.inflight = l.inflight[1:]
	return h, true
}

// permanentEventErrors are relay refusals of one event that a resend cannot
// fix; the event is dropped from the outbox.
var permanentEventErrors = map[string]bool{
	"INVALID_EVENT": true, "EVENT_CONFLICT": true, "EVENT_LIMIT": true,
	"APPROVAL_LIMIT": true, "APPROVAL_CONFLICT": true, "FRAME_TOO_LARGE": true,
}

// link maintains one logical relay socket (the user socket or one session
// socket): ticket minting, dialing, rotation before ticket expiry, backoff,
// ping, outbox resend and frame dispatch.
type link struct {
	name      string
	socket    Socket
	sessionID string
	api       API
	relayURL  string
	outbox    *Outbox
	log       *Logger
	onConnect func(l *live)
	onCommand func(l *live, in *Inbound)
	// onError sees a relay error first; true means it was handled.
	onError func(code string) bool
	dial    func(ctx context.Context, u string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	// connected is closed after the first successful dial (tests, status).
	connectedOnce sync.Once
	connected     chan struct{}
}

func newLink(name string, socket Socket, sessionID string, api API, relayURL string, outbox *Outbox, log *Logger) *link {
	return &link{name: name, socket: socket, sessionID: sessionID, api: api, relayURL: strings.TrimRight(relayURL, "/"),
		outbox: outbox, log: log, dial: websocket.Dial, connected: make(chan struct{})}
}

func (k *link) socketURL(ticket string) (string, error) {
	if k.socket == SessionSocket {
		return k.relayURL + "/sessions/" + url.PathEscape(k.sessionID) + "/ws?role=daemon", nil
	}
	sub, err := ticketSubject(ticket)
	if err != nil {
		return "", err
	}
	return k.relayURL + "/users/" + url.PathEscape(sub) + "/ws?role=daemon", nil
}

// run keeps the socket up until ctx ends or a terminal condition:
// ErrSuperseded, ErrSessionInactive or a *FatalError.
func (k *link) run(ctx context.Context) error {
	var b backoff
	var cur *live
	defer func() {
		if cur != nil {
			cur.retire()
		}
	}()
	retry := func() error {
		return sleepCtx(ctx, b.next())
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ticket, err := k.api.MintRelayTicket(ctx, k.sessionID)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fatal, gone := classifyMint(err, k.socket == SessionSocket)
			if fatal != nil {
				return fatal
			}
			if gone {
				return ErrSessionInactive
			}
			k.log.Printf("%s: relay ticket mint failed, retrying: %v", k.name, err)
			if err := retry(); err != nil {
				return err
			}
			continue
		}
		u, err := k.socketURL(ticket.Ticket)
		if err != nil {
			return &FatalError{Code: "BAD_TICKET", Message: err.Error()}
		}
		ws, resp, err := k.dial(ctx, u, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": []string{"Bearer " + ticket.Ticket}},
		})
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if k.socket == SessionSocket && (status == http.StatusNotFound || status == http.StatusConflict) {
				return ErrSessionInactive
			}
			k.log.Printf("%s: relay dial failed (HTTP %d, ticket fp:%s), retrying: %v", k.name, status, TicketFingerprint(ticket.Ticket), err)
			if err := retry(); err != nil {
				return err
			}
			continue
		}
		ws.SetReadLimit(readLimit)
		l := &live{ws: ws, ctrl: make(chan []byte, 256), done: make(chan struct{})}
		if cur != nil {
			cur.retire()
		}
		cur = l
		go k.read(ctx, l)
		go k.write(ctx, l)
		connectedAt := time.Now()
		k.log.Printf("%s: connected (ticket fp:%s)", k.name, TicketFingerprint(ticket.Ticket))
		k.connectedOnce.Do(func() { close(k.connected) })
		if k.onConnect != nil {
			k.onConnect(l)
		}
		rotateIn := time.Until(time.UnixMilli(ticket.ExpiresAt)) - rotateBefore
		if rotateIn < 5*time.Second {
			rotateIn = 5 * time.Second
		}
		timer := time.NewTimer(rotateIn)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			// Rotate: dial the replacement first; the relay supersedes this
			// socket (4000) once the new one is up, and we retire it then.
			b.reset()
			continue
		case <-l.done:
			timer.Stop()
		}
		code := websocket.CloseStatus(l.closeErr())
		if code == closeSuperseed {
			return ErrSuperseded
		}
		if time.Since(connectedAt) > stableAfter {
			b.reset()
		}
		k.log.Printf("%s: closed (code %d): %v", k.name, code, l.closeErr())
		if code == closeExpired && time.Since(connectedAt) > stableAfter {
			continue // ticket expiry or session end: re-mint now; the mint tells which
		}
		if err := retry(); err != nil {
			return err
		}
	}
}

func (k *link) read(ctx context.Context, l *live) {
	defer close(l.done)
	for {
		typ, data, err := l.ws.Read(ctx)
		if err != nil {
			l.errMu.Lock()
			l.err = err
			l.errMu.Unlock()
			return
		}
		if l.retired.Load() {
			continue
		}
		if typ != websocket.MessageText {
			k.log.Printf("%s: ignored binary relay frame", k.name)
			continue
		}
		if string(data) == "pong" {
			continue
		}
		in, err := Decode(k.socket, data)
		if err != nil {
			var de *DecodeError
			k.log.Printf("%s: %v (not executed)", k.name, err)
			if errors.As(err, &de) && de.CorrelationID != "" {
				sid := ""
				if uuidRe.MatchString(de.SessionID) {
					sid = de.SessionID
				}
				l.Send(commandAck(k.socket, de.CorrelationID, sid))
			}
			continue
		}
		switch in.Kind {
		case "ack":
			if k.outbox != nil {
				k.outbox.Ack(in.CorrelationID)
			}
			l.popInflight(in.CorrelationID)
		case "error":
			k.handleError(l, in.Code)
		case "presence":
		default:
			if k.onCommand != nil {
				k.onCommand(l, in)
			}
		}
	}
}

// handleError correlates a relay error by send order: only event sends get
// a reply on success, so an event-class error belongs to the oldest event
// still in flight on this socket.
func (k *link) handleError(l *live, code string) {
	if k.onError != nil && k.onError(code) {
		return
	}
	switch {
	case permanentEventErrors[code]:
		if cid, ok := l.popHead(); ok && k.outbox != nil {
			k.outbox.Drop(cid)
			k.log.Printf("%s: relay refused event %s (%s); dropped", k.name, cid, code)
			return
		}
	case code == "UNAVAILABLE":
		if cid, ok := l.popHead(); ok {
			// Later events may already be on the wire; reconnecting resends the
			// outbox in order from the first unacked event.
			k.log.Printf("%s: relay unavailable for event %s; reconnecting to resend", k.name, cid)
			_ = l.ws.CloseNow()
			return
		}
	}
	k.log.Printf("%s: relay error %s", k.name, code)
}

func (k *link) write(ctx context.Context, l *live) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	var lastSent int64
	send := func(b []byte) bool {
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		if err := l.ws.Write(wctx, websocket.MessageText, b); err != nil {
			if !l.retired.Load() && ctx.Err() == nil {
				k.log.Printf("%s: relay write failed: %v", k.name, err)
			}
			_ = l.ws.CloseNow()
			return false
		}
		return true
	}
	flush := func() bool {
		if k.outbox == nil || l.retired.Load() {
			return true
		}
		for _, e := range k.outbox.After(lastSent) {
			l.pushInflight(e.cid)
			if !send(e.raw) {
				return false
			}
			lastSent = e.seq
		}
		return true
	}
	if !flush() {
		return
	}
	var notify <-chan struct{}
	if k.outbox != nil {
		notify = k.outbox.Notify()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.done:
			return
		case b := <-l.ctrl:
			if !send(b) {
				return
			}
		case <-notify:
		case <-poll.C:
		case <-ping.C:
			if !send([]byte("ping")) {
				return
			}
		}
		if !flush() {
			return
		}
	}
}
