package connect

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// maxOutbox bounds unacked events held while the relay is unreachable; the
// relay itself admits about 4000 events per session.
const maxOutbox = 4096

// envelope is the relay event wire: exactly these eight fields.
type envelope struct {
	Kind          string `json:"kind"`
	SchemaVersion string `json:"schemaVersion"`
	Seq           int64  `json:"seq"`
	TS            int64  `json:"ts"`
	CorrelationID string `json:"correlationId"`
	Origin        string `json:"origin"`
	Type          string `json:"type"`
	Payload       Chunk  `json:"payload"`
}

type outEntry struct {
	seq int64
	cid string
	raw []byte
}

// Outbox orders a session's outgoing events. Each event is wrapped once,
// validated by the mosaic-event/1 parser and stored, so every resend is
// byte-identical; acknowledged events leave the outbox.
type Outbox struct {
	parser *mosaicevent.Parser
	log    *Logger
	now    func() time.Time

	mu      sync.Mutex
	runID   string
	seq     int64
	entries []*outEntry
	notify  chan struct{}
	drained chan struct{} // closed while entries is empty
	dropped int
}

// NewOutbox returns an outbox whose correlation ids are <runID>:<seq>.
func NewOutbox(parser *mosaicevent.Parser, log *Logger) *Outbox {
	o := &Outbox{parser: parser, log: log, now: time.Now, runID: newRunID(),
		notify: make(chan struct{}, 1), drained: make(chan struct{})}
	close(o.drained)
	return o
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RunID identifies this outbox's correlation id space.
func (o *Outbox) RunID() string { return o.runID }

// Emit wraps, validates and queues one chunk (possibly as several frames).
// It returns how many events were queued; invalid events are logged and
// dropped, never sent.
func (o *Outbox) Emit(c Chunk) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	typ, _ := c["type"].(string)
	// Probe envelopes use the widest seq so a size check never undercounts.
	wrap := func(p Chunk) ([]byte, error) {
		return json.Marshal(o.envelope(typ, 1<<52, p))
	}
	pieces, notes, err := fit(c, wrap)
	if err != nil {
		o.dropped++
		o.log.Printf("dropped %s event: %v", typ, err)
		return 0
	}
	for _, n := range notes {
		o.log.Printf("relay frame guard: %s", n)
	}
	queued := 0
	for _, p := range pieces {
		env := o.envelope(typ, o.seq+1, p)
		raw, err := json.Marshal(env)
		if err == nil && len(raw) > maxFrameBytes {
			err = fmt.Errorf("frame of %d bytes", len(raw))
		}
		if err == nil {
			_, err = o.parser.Parse(raw, "envelope")
		}
		if err != nil {
			o.dropped++
			o.log.Printf("dropped invalid %s event: %v", typ, err)
			continue
		}
		o.seq++
		if len(o.entries) >= maxOutbox {
			o.dropped++
			o.log.Printf("outbox full: dropped unacked event %s", o.entries[0].cid)
			o.entries = o.entries[1:]
		}
		if len(o.entries) == 0 {
			o.drained = make(chan struct{})
		}
		o.entries = append(o.entries, &outEntry{seq: o.seq, cid: env.CorrelationID, raw: raw})
		queued++
	}
	if queued > 0 {
		select {
		case o.notify <- struct{}{}:
		default:
		}
	}
	return queued
}

func (o *Outbox) envelope(typ string, seq int64, p Chunk) envelope {
	return envelope{Kind: "event", SchemaVersion: mosaicevent.Version, Seq: seq,
		TS: o.now().UnixMilli(), CorrelationID: fmt.Sprintf("%s:%d", o.runID, seq),
		Origin: "daemon", Type: typ, Payload: p}
}

// Notify signals new entries.
func (o *Outbox) Notify() <-chan struct{} { return o.notify }

// After returns the unacked entries with seq greater than after, in order.
func (o *Outbox) After(after int64) []*outEntry {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []*outEntry
	for _, e := range o.entries {
		if e.seq > after {
			out = append(out, e)
		}
	}
	return out
}

// Ack removes an acknowledged event. It reports whether cid was pending.
func (o *Outbox) Ack(cid string) bool { return o.remove(cid) }

// Drop removes an event the relay refused permanently.
func (o *Outbox) Drop(cid string) bool {
	if o.remove(cid) {
		o.mu.Lock()
		o.dropped++
		o.mu.Unlock()
		return true
	}
	return false
}

func (o *Outbox) remove(cid string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, e := range o.entries {
		if e.cid == cid {
			o.entries = append(o.entries[:i], o.entries[i+1:]...)
			if len(o.entries) == 0 {
				close(o.drained)
			}
			return true
		}
	}
	return false
}

// Len returns the number of unacked events.
func (o *Outbox) Len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.entries)
}

// Dropped returns how many events were never sent or refused.
func (o *Outbox) Dropped() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dropped
}

// WaitDrained waits until every event is acked or ctx ends.
func (o *Outbox) WaitDrained(ctx context.Context) bool {
	o.mu.Lock()
	ch := o.drained
	o.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}
