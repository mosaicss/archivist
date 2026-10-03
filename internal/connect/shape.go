package connect

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// Relay bounds (78.14 protocol.ts): a text frame is at most 65536 bytes and
// finite JSON of depth <= 32 with <= 20000 values.
const (
	maxFrameBytes = 65536
	maxDepth      = 32
	maxValues     = 20000
	truncMarker   = "\n…[truncated by archivist connect: kept %d of %d bytes]"
	previewBytes  = 16000
)

// deltaKey returns (field, id) for chunk types that coalesce and split.
func deltaKey(c Chunk) (field, id string, ok bool) {
	switch c["type"] {
	case "text-delta", "reasoning-delta":
		id, _ := c["id"].(string)
		return "delta", id, true
	case "tool-input-delta":
		id, _ := c["toolCallId"].(string)
		return "inputTextDelta", id, true
	}
	return "", "", false
}

// Coalescer merges consecutive deltas of one stream after translation, so a
// long answer costs a few relay events instead of one per token. The relay
// caps a session at about 4000 events.
type Coalescer struct {
	MaxBytes int
	pending  Chunk
}

// Add returns the chunks ready to send; a delta may be held back.
func (c *Coalescer) Add(ch Chunk) []Chunk {
	field, id, isDelta := deltaKey(ch)
	if c.pending != nil {
		pf, pid, _ := deltaKey(c.pending)
		if isDelta && ch["type"] == c.pending["type"] && pid == id && pf == field {
			merged := c.pending[field].(string) + ch[field].(string)
			if len(merged) <= c.limit() {
				c.pending[field] = merged
				return nil
			}
		}
	}
	out := c.Flush()
	if isDelta {
		if s, _ := ch[field].(string); len(s) < c.limit() {
			c.pending = copyChunk(ch)
			return out
		}
	}
	return append(out, ch)
}

// Flush releases a held delta.
func (c *Coalescer) Flush() []Chunk {
	if c.pending == nil {
		return nil
	}
	p := c.pending
	c.pending = nil
	return []Chunk{p}
}

// Pending reports whether a delta is held.
func (c *Coalescer) Pending() bool { return c.pending != nil }

func (c *Coalescer) limit() int {
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return 16 * 1024
}

func copyChunk(c Chunk) Chunk {
	out := make(Chunk, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// fit reshapes one chunk so each piece fits a relay frame once wrapped by
// wrap. Deltas split on UTF-8 boundaries; large tool payloads are truncated
// with a visible marker. An error means the chunk cannot be made to fit.
func fit(c Chunk, wrap func(Chunk) ([]byte, error)) (pieces []Chunk, notes []string, err error) {
	raw, err := wrap(c)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) <= maxFrameBytes && bounded(c) {
		return []Chunk{c}, nil, nil
	}
	if field, _, ok := deltaKey(c); ok {
		s, _ := c[field].(string)
		if len(s) < 2 {
			return nil, nil, fmt.Errorf("%v chunk of %d bytes cannot be split", c["type"], len(raw))
		}
		mid := len(s) / 2
		for mid > 0 && !utf8.RuneStart(s[mid]) {
			mid--
		}
		if mid == 0 {
			mid = len(s) / 2
		}
		a, b := copyChunk(c), copyChunk(c)
		a[field], b[field] = s[:mid], s[mid:]
		for _, part := range []Chunk{a, b} {
			p, n, err := fit(part, wrap)
			if err != nil {
				return nil, nil, err
			}
			pieces, notes = append(pieces, p...), append(notes, n...)
		}
		return pieces, append(notes, fmt.Sprintf("split %v", c["type"])), nil
	}
	reduced := copyChunk(c)
	switch c["type"] {
	case "tool-output-available":
		reduced["output"] = truncateValue(c["output"])
	case "tool-output-error", "tool-input-error", "error":
		if s, ok := c["errorText"].(string); ok {
			reduced["errorText"] = truncateString(s, previewBytes)
		}
		if in, ok := c["input"]; ok {
			reduced["input"] = truncateValue(in)
		}
	case "tool-input-available":
		reduced["input"] = truncateValue(c["input"])
	case "tool-approval-request":
		reduced["approvalDescriptor"] = reduceDescriptor(c["approvalDescriptor"])
	default:
		return nil, nil, fmt.Errorf("%v chunk of %d bytes exceeds the relay frame", c["type"], len(raw))
	}
	raw, err = wrap(reduced)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > maxFrameBytes || !bounded(reduced) {
		return nil, nil, fmt.Errorf("%v chunk still exceeds the relay frame after truncation", c["type"])
	}
	return []Chunk{reduced}, []string{fmt.Sprintf("truncated %v", c["type"])}, nil
}

// truncateValue renders v as JSON text cut to previewBytes with a marker.
func truncateValue(v any) any {
	if s, ok := v.(string); ok {
		return truncateString(s, previewBytes)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "[unrepresentable value removed by archivist connect]"
	}
	return truncateString(string(raw), previewBytes)
}

func truncateString(s string, keep int) string {
	if len(s) <= keep {
		return s
	}
	cut := keep
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf(truncMarker, cut, len(s))
}

// reduceDescriptor keeps the fields an approval card needs and truncates the
// tool input: Claude's can_use_tool keys, and Codex's command, cwd, kind,
// reason, availableDecisions, ids and the changed paths of a file change.
func reduceDescriptor(v any) any {
	d, ok := v.(map[string]any)
	if !ok {
		return truncateValue(v)
	}
	out := map[string]any{}
	for _, k := range []string{"subtype", "tool_name", "display_name", "tool_use_id", "blocked_path",
		"kind", "threadId", "turnId", "itemId", "approvalId", "serverName", "availableDecisions"} {
		if x, ok := d[k]; ok {
			out[k] = x
		}
	}
	for _, k := range []string{"description", "command", "cwd", "reason", "message"} {
		if s, ok := d[k].(string); ok {
			out[k] = truncateString(s, 2000)
		}
	}
	if in, ok := d["input"]; ok {
		out["input"] = truncateValue(in)
	}
	if changes, ok := d["changes"].([]any); ok {
		paths := []any{}
		for _, c := range changes {
			if m, ok := c.(map[string]any); ok && len(paths) < 200 {
				paths = append(paths, map[string]any{"path": m["path"], "kind": m["kind"]})
			}
		}
		out["changes"] = paths
	}
	out["truncated"] = true
	return out
}

// bounded mirrors the relay's finite JSON bound on depth and value count for
// a chunk that will sit one level down, inside the envelope's payload.
func bounded(v any) bool {
	budget := maxValues - 16
	var walk func(v any, depth int) bool
	walk = func(v any, depth int) bool {
		budget--
		if depth > maxDepth || budget < 0 {
			return false
		}
		switch x := v.(type) {
		case map[string]any:
			for _, e := range x {
				if !walk(e, depth+1) {
					return false
				}
			}
		case []any:
			for _, e := range x {
				if !walk(e, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return walk(v, 1)
}
