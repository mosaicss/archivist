package connect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CodexTranslator turns Codex app-server messages into mosaic-event/1
// chunks. It mirrors the 78.13 reference mapping (vendor/1/contract/
// normalize.ts normalizeCodex) message for message, so replaying the
// retained raw fixtures reproduces the canonical goldens. Unlike the
// replay-only reference, it never fails: unknown methods, items and
// mismatches are skipped and counted in Skipped.
//
// Beyond the goldens it maps, with existing schemas only: reasoning
// summary deltas (reasoning-*), turn/plan/updated (data-plan), fileChange
// items (tool-input-available, data-patch per change, outputs) and the
// archivist MCP approval elicitation (tool-approval-request).
type CodexTranslator struct {
	// Model names data-usage.model.
	Model string

	text      map[string]string // open agentMessage items: text seen so far
	textOrder []string          // open agentMessage ids in start order (JS Map order)
	reasoning map[string]bool   // open reasoning items
	rOrder    []string
	files     map[string]json.RawMessage // fileChange item id -> changes
	Skipped   int
}

// NewCodexTranslator returns an empty translator.
func NewCodexTranslator() *CodexTranslator {
	t := &CodexTranslator{Model: "codex"}
	t.reset()
	return t
}

// reset clears per-process stream state.
func (t *CodexTranslator) reset() {
	t.text = map[string]string{}
	t.textOrder = nil
	t.reasoning = map[string]bool{}
	t.rOrder = nil
	t.files = map[string]json.RawMessage{}
}

// In translates one notification (or approval request) from Codex. rpcID
// is the JSON-RPC id of a server request ("" for notifications).
func (t *CodexTranslator) In(method, rpcID string, params json.RawMessage) []Chunk {
	var p map[string]json.RawMessage
	if json.Unmarshal(params, &p) != nil {
		t.Skipped++
		return nil
	}
	str := func(m map[string]json.RawMessage, k string) string {
		var s string
		_ = json.Unmarshal(m[k], &s)
		return s
	}
	switch method {
	case "turn/started":
		var turn map[string]json.RawMessage
		_ = json.Unmarshal(p["turn"], &turn)
		id := str(turn, "id")
		if id == "" {
			t.Skipped++
			return nil
		}
		return []Chunk{startChunk(id)}
	case "item/started", "item/completed":
		return t.item(p["item"], method == "item/completed")
	case "item/agentMessage/delta":
		id, delta := str(p, "itemId"), str(p, "delta")
		seen, ok := t.text[id]
		if !ok {
			t.Skipped++
			return nil
		}
		t.text[id] = seen + delta
		return []Chunk{{"type": "text-delta", "id": id, "delta": delta}}
	case "item/reasoning/summaryTextDelta":
		id, delta := str(p, "itemId"), str(p, "delta")
		if id == "" {
			t.Skipped++
			return nil
		}
		var out []Chunk
		if !t.reasoning[id] {
			t.reasoning[id] = true
			t.rOrder = append(t.rOrder, id)
			out = append(out, Chunk{"type": "reasoning-start", "id": id})
		}
		return append(out, Chunk{"type": "reasoning-delta", "id": id, "delta": delta})
	case "turn/plan/updated":
		var plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		}
		if json.Unmarshal(p["plan"], &plan) != nil {
			t.Skipped++
			return nil
		}
		entries := []any{}
		for _, s := range plan {
			status := map[string]string{"pending": "pending", "inProgress": "in_progress", "completed": "completed"}[s.Status]
			if status == "" {
				status = "pending"
			}
			entries = append(entries, map[string]any{"content": s.Step, "priority": "medium", "status": status})
		}
		return []Chunk{{"type": "data-plan", "data": map[string]any{"entries": entries}}}
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		approvalID := codexApprovalID(str(p, "turnId"), rpcID)
		var descriptor any
		if method == "item/fileChange/requestApproval" {
			descriptor = t.fileDescriptor(p)
		} else if decodeNumbers(params, &descriptor) != nil {
			t.Skipped++
			return nil
		}
		return []Chunk{{"type": "tool-approval-request", "approvalId": approvalID,
			"toolCallId": str(p, "itemId"), "approvalDescriptor": descriptor}}
	case "thread/tokenUsage/updated":
		var tu struct {
			TokenUsage struct {
				Total map[string]json.RawMessage `json:"total"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &tu) != nil || tu.TokenUsage.Total == nil {
			t.Skipped++
			return nil
		}
		n := func(k string) (any, bool) {
			raw, ok := tu.TokenUsage.Total[k]
			if !ok {
				return nil, false
			}
			var v json.Number
			if decodeNumbers(raw, &v) != nil {
				return nil, false
			}
			return v, true
		}
		data := map[string]any{"model": t.Model}
		for _, k := range [][2]string{{"inputTokens", "inputTokens"}, {"outputTokens", "outputTokens"},
			{"cachedInputTokens", "cachedInputTokens"}, {"reasoningOutputTokens", "reasoningTokens"}} {
			if v, ok := n(k[0]); ok {
				data[k[1]] = v
			}
		}
		for _, k := range []string{"inputTokens", "outputTokens"} {
			if _, ok := data[k]; !ok {
				data[k] = 0
			}
		}
		return []Chunk{{"type": "data-usage", "data": data}}
	case "turn/completed":
		var turn map[string]json.RawMessage
		_ = json.Unmarshal(p["turn"], &turn)
		var out []Chunk
		for _, id := range t.textOrder {
			out = append(out, Chunk{"type": "text-end", "id": id})
		}
		for _, id := range t.rOrder {
			out = append(out, Chunk{"type": "reasoning-end", "id": id})
		}
		t.text, t.textOrder = map[string]string{}, nil
		t.reasoning, t.rOrder = map[string]bool{}, nil
		switch str(turn, "status") {
		case "interrupted":
			out = append(out, Chunk{"type": "abort", "reason": "interrupted"})
		case "failed":
			out = append(out, Chunk{"type": "error", "errorText": jsStringify(turn["error"])},
				Chunk{"type": "finish", "finishReason": "error"})
		case "completed":
			out = append(out, Chunk{"type": "finish", "finishReason": "stop"})
		default:
			t.Skipped++
		}
		return out
	case "error":
		// A retried error is not the end of the turn; Codex reports again
		// (or completes the turn as failed) when retries run out.
		var retry bool
		_ = json.Unmarshal(p["willRetry"], &retry)
		if retry {
			t.Skipped++
			return nil
		}
		return []Chunk{{"type": "error", "errorText": jsStringify(params)}}
	}
	t.Skipped++
	return nil
}

// ApprovalResponse is the chunk for the daemon's answer to an approval
// request: approved for accept/acceptForSession, the decision as reason.
func (t *CodexTranslator) ApprovalResponse(approvalID, decision string) Chunk {
	return Chunk{"type": "tool-approval-response", "approvalId": approvalID,
		"approved": decision == "accept" || decision == "acceptForSession", "reason": decision}
}

// codexApprovalID is the 78.13 approval id: <turnId>:approval:<rpc id>.
func codexApprovalID(turnID, rpcID string) string {
	if turnID == "" {
		turnID = "codex"
	}
	return turnID + ":approval:" + rpcID
}

// fileDescriptor is the approval card for a file change: the request
// carries no diff, so the changes come from the matching fileChange item.
func (t *CodexTranslator) fileDescriptor(p map[string]json.RawMessage) map[string]any {
	d := map[string]any{}
	var itemID string
	_ = json.Unmarshal(p["itemId"], &itemID)
	d["itemId"] = itemID
	var reason *string
	if json.Unmarshal(p["reason"], &reason) == nil && reason != nil {
		d["reason"] = *reason
	}
	if raw, ok := t.files[itemID]; ok {
		var changes any
		if decodeNumbers(raw, &changes) == nil {
			d["changes"] = changes
		}
	}
	return d
}

func (t *CodexTranslator) item(raw json.RawMessage, completed bool) []Chunk {
	var item map[string]json.RawMessage
	var head codexItemHead
	if json.Unmarshal(raw, &item) != nil || json.Unmarshal(raw, &head) != nil || head.ID == "" {
		t.Skipped++
		return nil
	}
	id := head.ID
	switch head.Type {
	case "userMessage":
		return nil // input is retained, not assistant output
	case "agentMessage":
		if !completed {
			if _, open := t.text[id]; !open {
				t.textOrder = append(t.textOrder, id)
			}
			t.text[id] = ""
			return []Chunk{{"type": "text-start", "id": id}}
		}
		seen, open := t.text[id]
		if !open {
			t.Skipped++
			return nil
		}
		var final string
		_ = json.Unmarshal(item["text"], &final)
		var out []Chunk
		if seen == "" && final != "" {
			out = append(out, Chunk{"type": "text-delta", "id": id, "delta": final})
		}
		t.closeText(id)
		return append(out, Chunk{"type": "text-end", "id": id})
	case "reasoning":
		if !completed || !t.reasoning[id] {
			if completed {
				t.Skipped++
			}
			return nil
		}
		delete(t.reasoning, id)
		t.rOrder = remove(t.rOrder, id)
		return []Chunk{{"type": "reasoning-end", "id": id}}
	case "commandExecution", "mcpToolCall":
		if !completed {
			name := "commandExecution"
			var input any
			if head.Type == "commandExecution" {
				in := map[string]any{}
				for _, k := range []string{"command", "cwd"} {
					if v, ok := rawValue(item, k); ok {
						in[k] = v
					}
				}
				input = in
			} else {
				var server, tool string
				_ = json.Unmarshal(item["server"], &server)
				_ = json.Unmarshal(item["tool"], &tool)
				name = server + "." + tool
				input, _ = rawValue(item, "arguments")
			}
			return []Chunk{{"type": "tool-input-available", "toolCallId": id, "toolName": name, "input": input}}
		}
		errRaw, hasErr := nonNull(item, "error")
		resRaw, hasRes := nonNull(item, "result")
		switch {
		case head.Status == "declined":
			return []Chunk{{"type": "tool-output-denied", "toolCallId": id}}
		case head.Type == "mcpToolCall" && head.Status == "failed":
			text := "MCP tool call failed"
			switch {
			case hasErr && hasRes:
				text = `{"error":` + jsStringify(errRaw) + `,"result":` + jsStringify(resRaw) + `}`
			case hasErr:
				text = jsStringify(errRaw)
			case hasRes:
				text = jsStringify(resRaw)
			}
			return []Chunk{{"type": "tool-output-error", "toolCallId": id, "errorText": text}}
		case hasErr && truthy(errRaw):
			return []Chunk{{"type": "tool-output-error", "toolCallId": id, "errorText": jsStringify(errRaw)}}
		}
		var output any
		if head.Type == "commandExecution" {
			out := map[string]any{}
			for _, k := range [][2]string{{"aggregatedOutput", "stdout"}, {"exitCode", "exitCode"}, {"status", "status"}, {"durationMs", "durationMs"}} {
				if v, ok := rawValue(item, k[0]); ok {
					out[k[1]] = v
				}
			}
			output = out
		} else {
			output, _ = rawValue(item, "result")
		}
		return []Chunk{{"type": "tool-output-available", "toolCallId": id, "output": output}}
	case "fileChange":
		changesRaw := item["changes"]
		if !completed {
			t.files[id] = changesRaw
			var changes []struct {
				Path string `json:"path"`
				Diff string `json:"diff"`
				Kind struct {
					Type string `json:"type"`
				} `json:"kind"`
			}
			_ = json.Unmarshal(changesRaw, &changes)
			var input any = map[string]any{"changes": []any{}}
			if v, ok := rawValue(item, "changes"); ok {
				input = map[string]any{"changes": v}
			}
			out := []Chunk{{"type": "tool-input-available", "toolCallId": id, "toolName": "fileChange", "input": input}}
			for _, c := range changes {
				if c.Path == "" || (c.Kind.Type != "add" && c.Kind.Type != "update" && c.Kind.Type != "delete") {
					continue
				}
				out = append(out, Chunk{"type": "data-patch", "data": map[string]any{
					"toolCallId": id, "path": c.Path, "diff": c.Diff, "operation": c.Kind.Type}})
			}
			return out
		}
		delete(t.files, id)
		switch head.Status {
		case "declined":
			return []Chunk{{"type": "tool-output-denied", "toolCallId": id}}
		case "failed":
			return []Chunk{{"type": "tool-output-error", "toolCallId": id, "errorText": "file change failed"}}
		}
		return []Chunk{{"type": "tool-output-available", "toolCallId": id, "output": map[string]any{"status": head.Status}}}
	}
	t.Skipped++
	return nil
}

func (t *CodexTranslator) closeText(id string) {
	delete(t.text, id)
	t.textOrder = remove(t.textOrder, id)
}

func remove(list []string, s string) []string {
	for i, v := range list {
		if v == s {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return list
}

// rawValue decodes field k (numbers kept exact) when present.
func rawValue(m map[string]json.RawMessage, k string) (any, bool) {
	raw, ok := m[k]
	if !ok {
		return nil, false
	}
	var v any
	if decodeNumbers(raw, &v) != nil {
		return nil, false
	}
	return v, true
}

// nonNull returns field k when present and not JSON null (JS `!= null`).
func nonNull(m map[string]json.RawMessage, k string) (json.RawMessage, bool) {
	raw, ok := m[k]
	if !ok || string(bytes.TrimSpace(raw)) == "null" {
		return nil, false
	}
	return raw, true
}

// truthy mirrors JavaScript truthiness for a JSON value.
func truthy(raw json.RawMessage) bool {
	switch s := string(bytes.TrimSpace(raw)); s {
	case "", "null", "false", "0", `""`, "-0":
		return false
	default:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f != 0 && !math.IsNaN(f)
		}
		return true
	}
}

// jsStringify renders a JSON value the way JavaScript's JSON.stringify
// does after JSON.parse: compact, object keys in document order, strings
// with JSON.stringify escaping. Invalid input renders as "null".
func jsStringify(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if err := jsWrite(dec, &b); err != nil {
		return "null"
	}
	return b.String()
}

func jsWrite(dec *json.Decoder, b *strings.Builder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			b.WriteByte('{')
			first := true
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := kt.(string)
				if !ok {
					return fmt.Errorf("object key")
				}
				if !first {
					b.WriteByte(',')
				}
				first = false
				jsQuote(b, key)
				b.WriteByte(':')
				if err := jsWrite(dec, b); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			b.WriteByte('}')
		case '[':
			b.WriteByte('[')
			first := true
			for dec.More() {
				if !first {
					b.WriteByte(',')
				}
				first = false
				if err := jsWrite(dec, b); err != nil {
					return err
				}
			}
			if _, err := dec.Token(); err != nil {
				return err
			}
			b.WriteByte(']')
		}
	case string:
		jsQuote(b, v)
	case json.Number:
		b.WriteString(jsNumber(v))
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case nil:
		b.WriteString("null")
	}
	return nil
}

// jsQuote writes s with JSON.stringify's escaping: only quote, backslash
// and control characters are escaped (lone surrogates cannot occur in Go
// strings decoded from JSON).
func jsQuote(b *strings.Builder, s string) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// jsNumber formats a JSON number as JavaScript prints the parsed double.
func jsNumber(n json.Number) string {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if _, err := strconv.ParseInt(s, 10, 64); err == nil && len(strings.TrimPrefix(s, "-")) <= 15 {
			if s == "-0" {
				return "0"
			}
			return s
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		out := strconv.FormatFloat(f, 'e', -1, 64)
		// JavaScript writes 1e+21 and 1e-7 (no zero padding).
		out = strings.Replace(out, "e+0", "e+", 1)
		out = strings.Replace(out, "e-0", "e-", 1)
		return out
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}
