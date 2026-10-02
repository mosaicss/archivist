package connect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// Chunk is one AI SDK UI message stream chunk (mosaic-event/1 payload).
type Chunk = map[string]any

// Translator turns Claude Code stream-json frames into mosaic-event/1 chunks.
// It mirrors the 78.13 reference mapping (vendor/1/contract/normalize.ts
// normalizeClaude) frame for frame, so replaying the retained raw fixtures
// reproduces the canonical goldens. Unlike the replay-only reference, a
// runtime translator never fails on unknown input: unknown frames, blocks
// and deltas are skipped and counted in Skipped.
type Translator struct {
	// TurnID names the start chunk's messageId for turn n (1-based).
	TurnID func(n int) string
	// FallbackModel names data-usage.model when the result has no modelUsage.
	FallbackModel string

	turn      int
	messageID string
	streamed  map[string]bool
	blocks    map[int]*block
	order     []int // block indexes in insertion order (JS Map order)
	Skipped   int
}

type block struct {
	kind    string // text | reasoning | tool_use
	id      string
	name    string
	input   string
	initial any
}

// NewTranslator returns a translator with the reference turn ids.
func NewTranslator() *Translator {
	return &Translator{
		TurnID:        func(n int) string { return "claude-turn-" + strconv.Itoa(n) },
		FallbackModel: "claude",
		streamed:      map[string]bool{},
		blocks:        map[int]*block{},
	}
}

func startChunk(id string) Chunk {
	return Chunk{"type": "start", "messageId": id, "messageMetadata": map[string]any{"schemaVersion": mosaicevent.Version}}
}

// Out translates a frame the daemon wrote to Claude's stdin.
func (t *Translator) Out(frame map[string]any) []Chunk {
	switch frame["type"] {
	case "user":
		t.turn++
		return []Chunk{startChunk(t.TurnID(t.turn))}
	case "control_response":
		response, _ := frame["response"].(map[string]any)
		body, _ := response["response"].(map[string]any)
		behavior, _ := body["behavior"].(string)
		if behavior != "allow" && behavior != "deny" {
			t.Skipped++
			return nil
		}
		c := Chunk{"type": "tool-approval-response", "approvalId": response["request_id"], "approved": behavior == "allow"}
		if msg, ok := body["message"].(string); ok && msg != "" {
			c["reason"] = msg
		}
		return []Chunk{c}
	}
	return nil
}

// In translates one stdout line from Claude Code.
func (t *Translator) In(line []byte) []Chunk {
	var f map[string]any
	if err := decodeNumbers(line, &f); err != nil {
		t.Skipped++
		return nil
	}
	var out []Chunk
	emit := func(c ...Chunk) { out = append(out, c...) }
	switch f["type"] {
	case "stream_event":
		e, _ := f["event"].(map[string]any)
		index := intOf(e["index"])
		switch e["type"] {
		case "message_start":
			m, _ := e["message"].(map[string]any)
			if id, ok := m["id"].(string); ok {
				t.messageID = id
				t.streamed[id] = true
			}
		case "content_block_start":
			b, _ := e["content_block"].(map[string]any)
			kind, _ := b["type"].(string)
			if kind == "thinking" {
				kind = "reasoning"
			}
			if kind != "text" && kind != "reasoning" && kind != "tool_use" {
				t.Skipped++
				return nil
			}
			id := fmt.Sprintf("%s:%d", t.messageID, index)
			if kind == "tool_use" {
				id, _ = b["id"].(string)
			}
			if id == "" {
				t.Skipped++
				return nil
			}
			name, _ := b["name"].(string)
			initial, ok := b["input"]
			if !ok || initial == nil {
				initial = map[string]any{}
			}
			if _, dup := t.blocks[index]; !dup {
				t.order = append(t.order, index)
			}
			t.blocks[index] = &block{kind: kind, id: id, name: name, initial: initial}
			if kind == "tool_use" {
				emit(Chunk{"type": "tool-input-start", "toolCallId": id, "toolName": name})
			} else {
				emit(Chunk{"type": kind + "-start", "id": id})
			}
			text, _ := b["text"].(string)
			if kind == "reasoning" {
				text, _ = b["thinking"].(string)
			}
			if text != "" {
				emit(Chunk{"type": kind + "-delta", "id": id, "delta": text})
			}
		case "content_block_delta":
			b := t.blocks[index]
			d, _ := e["delta"].(map[string]any)
			if b == nil {
				t.Skipped++
				return nil
			}
			switch d["type"] {
			case "input_json_delta":
				delta, _ := d["partial_json"].(string)
				b.input += delta
				emit(Chunk{"type": "tool-input-delta", "toolCallId": b.id, "inputTextDelta": delta})
			case "text_delta", "thinking_delta":
				delta, ok := d["text"].(string)
				if !ok {
					delta, _ = d["thinking"].(string)
				}
				emit(Chunk{"type": b.kind + "-delta", "id": b.id, "delta": delta})
			case "signature_delta":
			default:
				t.Skipped++
			}
		case "content_block_stop":
			if c := t.close(index); c != nil {
				emit(c)
			}
		case "message_delta", "message_stop":
		default:
			t.Skipped++
		}
	case "assistant":
		m, _ := f["message"].(map[string]any)
		mid, _ := m["id"].(string)
		// Snapshots repeat partial output. A snapshot without partials remains visible.
		if mid == "" || t.streamed[mid] {
			break
		}
		content, _ := m["content"].([]any)
		for index, value := range content {
			b, _ := value.(map[string]any)
			id := fmt.Sprintf("%s:%d", mid, index)
			switch b["type"] {
			case "text", "thinking":
				kind, key := "text", "text"
				if b["type"] == "thinking" {
					kind, key = "reasoning", "thinking"
				}
				text, _ := b[key].(string)
				emit(Chunk{"type": kind + "-start", "id": id}, Chunk{"type": kind + "-delta", "id": id, "delta": text},
					Chunk{"type": kind + "-end", "id": id})
			case "tool_use":
				input := b["input"]
				if input == nil {
					input = map[string]any{}
				}
				emit(Chunk{"type": "tool-input-available", "toolCallId": b["id"], "toolName": b["name"], "input": input})
			default:
				t.Skipped++
			}
		}
	case "control_request":
		request, _ := f["request"].(map[string]any)
		if request["subtype"] != "can_use_tool" {
			t.Skipped++
			break
		}
		toolCallID, _ := request["tool_use_id"].(string)
		reqID, _ := f["request_id"].(string)
		if toolCallID == "" {
			toolCallID = reqID
		}
		emit(Chunk{"type": "tool-approval-request", "approvalId": reqID, "toolCallId": toolCallID, "approvalDescriptor": request})
	case "user":
		m, _ := f["message"].(map[string]any)
		content, ok := m["content"].([]any)
		if !ok {
			break
		}
		for _, value := range content {
			b, _ := value.(map[string]any)
			switch b["type"] {
			case "tool_result":
				if isErr, _ := b["is_error"].(bool); isErr {
					text, ok := b["content"].(string)
					if !ok {
						raw, _ := json.Marshal(b["content"])
						text = string(raw)
					}
					emit(Chunk{"type": "tool-output-error", "toolCallId": b["tool_use_id"], "errorText": text})
				} else {
					emit(Chunk{"type": "tool-output-available", "toolCallId": b["tool_use_id"], "output": b["content"]})
				}
			case "text":
				// "[Request interrupted by user]" and replayed prompts are not output.
			default:
				t.Skipped++
			}
		}
	case "result":
		// Interrupt captures omit block_stop. Close only content blocks.
		for _, index := range append([]int(nil), t.order...) {
			if c := t.close(index); c != nil {
				emit(c)
			}
		}
		emit(Chunk{"type": "data-usage", "data": t.usage(f, line)})
		isErr, hasErr := f["is_error"].(bool)
		aborted := f["terminal_reason"] == "aborted_streaming"
		if hasErr && isErr {
			if aborted {
				emit(Chunk{"type": "abort", "reason": "aborted_streaming"})
			} else {
				text, ok := f["result"].(string)
				if !ok {
					errs := f["errors"]
					if errs == nil {
						errs = []any{}
					}
					raw, _ := json.Marshal(errs)
					text = string(raw)
				}
				emit(Chunk{"type": "error", "errorText": text})
			}
		}
		if !aborted {
			reason := "stop"
			if isErr {
				reason = "error"
			}
			emit(Chunk{"type": "finish", "finishReason": reason})
		}
	case "control_response":
		// Receipt for our own interrupt request, not a permission outcome.
	default:
		// system/*, rate_limit_event, hook frames and anything newer.
	}
	return out
}

// close ends the content block at index.
func (t *Translator) close(index int) Chunk {
	b, ok := t.blocks[index]
	if !ok {
		t.Skipped++
		return nil
	}
	delete(t.blocks, index)
	for i, v := range t.order {
		if v == index {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
	if b.kind == "tool_use" {
		input := b.initial
		if b.input != "" {
			if err := decodeNumbers([]byte(b.input), &input); err != nil {
				input = b.input // incomplete JSON at an interrupt: keep the text
			}
		}
		return Chunk{"type": "tool-input-available", "toolCallId": b.id, "toolName": b.name, "input": input}
	}
	return Chunk{"type": b.kind + "-end", "id": b.id}
}

// usage builds data-usage.data from a result frame.
func (t *Translator) usage(f map[string]any, line []byte) map[string]any {
	u, _ := f["usage"].(map[string]any)
	n := func(key string) int64 { return intOf64(u[key]) }
	data := map[string]any{
		"inputTokens":  n("input_tokens") + n("cache_read_input_tokens") + n("cache_creation_input_tokens"),
		"outputTokens": n("output_tokens"),
		"model":        firstKey(line, "modelUsage", t.FallbackModel),
	}
	if _, ok := u["cache_read_input_tokens"]; ok {
		data["cachedInputTokens"] = n("cache_read_input_tokens")
	}
	if details, ok := u["output_tokens_details"].(map[string]any); ok {
		if v, ok := details["thinking_tokens"]; ok {
			data["reasoningTokens"] = intOf64(v)
		}
	}
	return data
}

// firstKey returns the first key (document order) of the object field name
// in the JSON object line, or fallback. Go maps lose order; the reference
// takes Object.keys(...)[0].
func firstKey(line []byte, name, fallback string) string {
	var top map[string]json.RawMessage
	if json.Unmarshal(line, &top) != nil {
		return fallback
	}
	raw, ok := top[name]
	if !ok {
		return fallback
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fallback
	}
	if tok, err := dec.Token(); err == nil {
		if key, ok := tok.(string); ok && key != "" {
			return key
		}
	}
	return fallback
}

func decodeNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

func intOf(v any) int { return int(intOf64(v)) }

func intOf64(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		if f, err := x.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	}
	return 0
}

// reset clears per-process stream state (a new Claude process starts with new
// message and block ids); the turn counter continues.
func (t *Translator) reset() {
	t.messageID = ""
	t.blocks = map[int]*block{}
	t.order = nil
}
