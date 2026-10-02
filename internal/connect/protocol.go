package connect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
)

// Relay wire (Story 78.14). Only the closed command set below is ever
// executed; every other frame is logged and, when it carries a correlation
// id, acknowledged so the relay stops redelivering it.

// Socket identifies which relay socket a frame arrived on.
type Socket int

const (
	UserSocket Socket = iota
	SessionSocket
)

func (s Socket) String() string {
	if s == UserSocket {
		return "user"
	}
	return "session"
}

// Inbound is one decoded relay frame.
type Inbound struct {
	Kind            string
	CorrelationID   string
	SessionID       string
	Agent           string
	Prompt          string
	Text            string
	ApprovalID      string
	Decision        string
	Reason          string
	Scope           string
	TerminalReceipt bool
	Code            string
	Seq             int64
	Duplicate       bool
	Online          bool
}

// DecodeError is a frame outside the closed set. CorrelationID/SessionID are
// filled when the frame carried them as strings, so the caller can ack it.
type DecodeError struct {
	Reason        string
	Kind          string
	CorrelationID string
	SessionID     string
}

func (e *DecodeError) Error() string {
	if e.Kind != "" {
		return fmt.Sprintf("refused %q frame: %s", e.Kind, e.Reason)
	}
	return "refused frame: " + e.Reason
}

// uuidRe matches the relay's session UUID rule (protocol.ts UUID).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

const resolvedPrefix = "resolved:"

// field sets per (socket, kind): required fields and optional fields.
type shape struct{ required, optional []string }

var commonFields = []string{"kind", "correlationId", "sessionId"}

var userShapes = map[string]shape{
	"presence":          {required: []string{"kind", "online", "agents"}},
	"start_session":     {required: append(append([]string{}, commonFields...), "agent", "prompt")},
	"approval_response": {required: append(append([]string{}, commonFields...), "approvalId", "decision", "reason", "terminalReceipt"), optional: []string{"scope"}},
	"error":             {required: []string{"kind", "code"}},
}

var sessionShapes = map[string]shape{
	"user_message":      {required: append(append([]string{}, commonFields...), "text")},
	"interrupt":         {required: commonFields},
	"stop_session":      {required: commonFields},
	"approval_response": {required: append(append([]string{}, commonFields...), "approvalId", "decision", "reason"), optional: []string{"scope"}},
	"ack":               {required: []string{"kind", "correlationId", "seq", "duplicate"}},
	"error":             {required: []string{"kind", "code"}},
}

// Decode strictly decodes one text frame from the given socket.
func Decode(socket Socket, raw []byte) (*Inbound, error) {
	var fields map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return nil, &DecodeError{Reason: "not a JSON object"}
	}
	if dec.More() {
		return nil, &DecodeError{Reason: "trailing data after the JSON object"}
	}
	derr := &DecodeError{}
	_ = json.Unmarshal(fields["correlationId"], &derr.CorrelationID)
	_ = json.Unmarshal(fields["sessionId"], &derr.SessionID)
	if err := json.Unmarshal(fields["kind"], &derr.Kind); err != nil || derr.Kind == "" {
		derr.Reason = "missing kind"
		return nil, derr
	}
	shapes := userShapes
	if socket == SessionSocket {
		shapes = sessionShapes
	}
	sh, ok := shapes[derr.Kind]
	if !ok {
		derr.Reason = "kind is not in the closed " + socket.String() + " socket set"
		return nil, derr
	}
	if reason := exactFields(fields, sh); reason != "" {
		derr.Reason = reason
		return nil, derr
	}
	in := &Inbound{Kind: derr.Kind}
	fail := func(reason string) (*Inbound, error) {
		derr.Reason = reason
		return nil, derr
	}
	str := func(name string, dst *string) bool {
		raw, ok := fields[name]
		if !ok {
			return true
		}
		return json.Unmarshal(raw, dst) == nil
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{"correlationId", &in.CorrelationID}, {"sessionId", &in.SessionID}, {"agent", &in.Agent},
		{"prompt", &in.Prompt}, {"text", &in.Text}, {"approvalId", &in.ApprovalID},
		{"decision", &in.Decision}, {"reason", &in.Reason}, {"scope", &in.Scope}, {"code", &in.Code},
	} {
		if !str(f.name, f.dst) {
			return fail(f.name + " must be a string")
		}
	}

	switch in.Kind {
	case "presence":
		if json.Unmarshal(fields["online"], &in.Online) != nil {
			return fail("online must be a boolean")
		}
		var agents []json.RawMessage
		if json.Unmarshal(fields["agents"], &agents) != nil {
			return fail("agents must be an array")
		}
		return in, nil
	case "error":
		if in.Code == "" {
			return fail("error code is empty")
		}
		return in, nil
	case "ack":
		if json.Unmarshal(fields["seq"], &in.Seq) != nil || in.Seq < 1 {
			return fail("seq must be a positive integer")
		}
		if json.Unmarshal(fields["duplicate"], &in.Duplicate) != nil {
			return fail("duplicate must be a boolean")
		}
		if !validCID(in.CorrelationID, false) {
			return fail("invalid correlationId")
		}
		return in, nil
	}

	// Commands.
	if !uuidRe.MatchString(in.SessionID) {
		return fail("sessionId is not a session UUID")
	}
	if in.Kind == "approval_response" {
		if !strings.HasPrefix(in.CorrelationID, resolvedPrefix) || !validCID(strings.TrimPrefix(in.CorrelationID, resolvedPrefix), false) {
			return fail("approval responses carry resolved:<request id>")
		}
		if in.ApprovalID == "" || len(in.ApprovalID) > 400 {
			return fail("invalid approvalId")
		}
		if in.Decision != "allow" && in.Decision != "deny" {
			return fail("decision must be allow or deny")
		}
		if in.Reason != "user" && in.Reason != "timeout" {
			return fail("reason must be user or timeout")
		}
		if _, ok := fields["scope"]; ok {
			allowed := map[string]bool{"allow_once": in.Decision == "allow", "allow_always": in.Decision == "allow",
				"reject_once": in.Decision == "deny", "reject_always": in.Decision == "deny"}
			if !allowed[in.Scope] {
				return fail("scope does not match the decision")
			}
		}
		if socket == UserSocket {
			if json.Unmarshal(fields["terminalReceipt"], &in.TerminalReceipt) != nil || !in.TerminalReceipt {
				return fail("user socket approval responses must be terminal receipts")
			}
		}
		return in, nil
	}
	if !validCID(in.CorrelationID, false) {
		return fail("invalid correlationId")
	}
	switch in.Kind {
	case "start_session":
		if in.Agent != "claude" && in.Agent != "codex" {
			return fail("agent must be claude or codex")
		}
		if !textOK(in.Prompt) {
			return fail("prompt must be 1..32000 characters")
		}
	case "user_message":
		if !textOK(in.Text) {
			return fail("text must be 1..32000 characters")
		}
	}
	return in, nil
}

// exactFields returns "" when fields has every required key and nothing
// beyond required plus optional.
func exactFields(fields map[string]json.RawMessage, sh shape) string {
	allowed := map[string]bool{}
	for _, k := range sh.required {
		allowed[k] = true
		if _, ok := fields[k]; !ok {
			return "missing field " + k
		}
	}
	for _, k := range sh.optional {
		allowed[k] = true
	}
	var extra []string
	for k := range fields {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return "unexpected fields " + strings.Join(extra, ",")
	}
	return ""
}

// validCID mirrors the relay's correlation id rule: 1..200 UTF-16 units and
// not reserved (unless allowResolved).
func validCID(id string, allowResolved bool) bool {
	n := len(utf16.Encode([]rune(id)))
	if n == 0 || n > 200 {
		return false
	}
	return allowResolved || !strings.HasPrefix(id, resolvedPrefix)
}

func textOK(s string) bool {
	n := len(utf16.Encode([]rune(s)))
	return n > 0 && n <= 32000
}

// Outbound control frames.

func capabilitiesFrame(caps []Capability) []byte {
	b, _ := json.Marshal(struct {
		Kind   string       `json:"kind"`
		Agents []Capability `json:"agents"`
	}{"capabilities", caps})
	return b
}

// commandAck acknowledges a command. The user socket adds sessionId (required
// for resolved: terminal receipts); the session socket ack is exactly
// {kind, correlationId}.
func commandAck(socket Socket, correlationID, sessionID string) []byte {
	if socket == UserSocket && sessionID != "" {
		b, _ := json.Marshal(struct {
			Kind          string `json:"kind"`
			CorrelationID string `json:"correlationId"`
			SessionID     string `json:"sessionId"`
		}{"command_ack", correlationID, sessionID})
		return b
	}
	b, _ := json.Marshal(struct {
		Kind          string `json:"kind"`
		CorrelationID string `json:"correlationId"`
	}{"command_ack", correlationID})
	return b
}
