package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Claude card carries the session scope; the translator builds it the
// same way as the daemon's size check.
func TestClaudeApprovalCardScope(t *testing.T) {
	line := []byte(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash",` +
		`"display_name":"Bash","input":{"command":"git status -s","timeout":5},"tool_use_id":"toolu_1"}}`)
	chunks := NewTranslator().In(line)
	if len(chunks) != 1 || chunks[0]["type"] != "tool-approval-request" || chunks[0]["approvalId"] != "r1" || chunks[0]["toolCallId"] != "toolu_1" {
		t.Fatalf("chunks %v", chunks)
	}
	d := chunks[0]["approvalDescriptor"].(map[string]any)
	scope, _ := d[sessionScopeKey].(map[string]any)
	if scope["kind"] != scopePrefix || scope["value"] != "git status" || d["tool_name"] != "Bash" {
		t.Fatalf("descriptor %v", d)
	}
	var f map[string]json.RawMessage
	_ = json.Unmarshal(line, &f)
	card, ok := claudeCardFor("r1", f["request"])
	a, _ := json.Marshal(card)
	b, _ := json.Marshal(chunks[0])
	if !ok || string(a) != string(b) {
		t.Fatalf("size check card %s, translator card %s", a, b)
	}
	// No tool_use_id: the request id names the call.
	c := claudeApprovalCard("r2", map[string]any{"subtype": "can_use_tool", "tool_name": "Read", "input": map[string]any{}})
	if c["toolCallId"] != "r2" || c["approvalDescriptor"].(map[string]any)[sessionScopeKey].(map[string]any)["kind"] != scopeTool {
		t.Fatalf("card %v", c)
	}
	// A reduced descriptor keeps the scope.
	if r := reduceDescriptor(d).(map[string]any); r[sessionScopeKey] == nil {
		t.Fatalf("reduced descriptor %v", r)
	}
}

func TestCardRefusal(t *testing.T) {
	o := newTestOutbox(t)
	bash := func(n int) Chunk {
		return claudeApprovalCard("r", map[string]any{"subtype": "can_use_tool", "tool_name": "Bash",
			"input": map[string]any{"command": strings.Repeat("a", n)}})
	}
	if msg := cardRefusal(bash(previewBytes), o.Fits); msg != "" {
		t.Fatalf("a %d byte command refused: %s", previewBytes, msg)
	}
	if msg := cardRefusal(bash(previewBytes+1), o.Fits); msg != cardCommandTooLong {
		t.Fatalf("a long command: %q", msg)
	}
	write := claudeApprovalCard("r", map[string]any{"subtype": "can_use_tool", "tool_name": "Write",
		"input": map[string]any{"file_path": "/s/a.md", "content": strings.Repeat("x", 70000)}})
	if o.Fits(write) || cardRefusal(write, o.Fits) != cardTooLarge {
		t.Fatal("a card over a frame was not refused")
	}
	small := claudeApprovalCard("r", map[string]any{"subtype": "can_use_tool", "tool_name": "Write",
		"input": map[string]any{"file_path": "/s/a.md", "content": strings.Repeat("x", 40000)}})
	if !o.Fits(small) || cardRefusal(small, o.Fits) != "" {
		t.Fatal("a card that fits was refused")
	}
	// Codex command approvals: the command field.
	codex := Chunk{"type": "tool-approval-request", "approvalId": "t:approval:1", "toolCallId": "exec-1",
		"approvalDescriptor": map[string]any{"kind": "command", "command": strings.Repeat("echo x; ", 2500), "cwd": "/w"}}
	if cardRefusal(codex, o.Fits) != cardCommandTooLong {
		t.Fatal("a long Codex command was not refused")
	}
	// Codex cards without a long command (a file change, an MCP tool
	// approval) that do not fit one frame unreduced.
	file := Chunk{"type": "tool-approval-request", "approvalId": "t:approval:2", "toolCallId": "patch-1",
		"approvalDescriptor": map[string]any{"itemId": "patch-1", "reason": "edit",
			"changes": []any{map[string]any{"path": "/w/a.md", "kind": map[string]any{"type": "add"}, "diff": strings.Repeat("+x\n", 30000)}}}}
	mcp := Chunk{"type": "tool-approval-request", "approvalId": "t:approval:3", "toolCallId": "mcp-1",
		"approvalDescriptor": map[string]any{"serverName": "archivist", "message": "Allow?",
			"_meta": map[string]any{"codex_approval_kind": "mcp_tool_call", "tool_params": map[string]any{"q": strings.Repeat("y", 70000)}}}}
	for _, c := range []Chunk{file, mcp} {
		if cardCommand(c) != "" || cardRefusal(c, o.Fits) != cardTooLarge {
			t.Fatalf("a Codex card over a frame was not refused: %v", c["approvalId"])
		}
	}
	// The refusal messages are plain copy.
	for _, msg := range []string{cardCommandTooLong, cardTooLarge} {
		if strings.ContainsAny(msg, "-—") {
			t.Errorf("message %q has a hyphen or em dash", msg)
		}
	}
	if len(o.After(0)) != 0 {
		t.Fatal("Fits queued an event")
	}
}

// Acceptance: a 20 KB Bash command is denied with the message and no card
// reaches the relay; a Write whose card would not fit one frame is denied
// too, even after an "Allow for session" on its path.
func TestClaudeCardTooLargeIsDenied(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	sid := h.startSession("bash " + strings.Repeat("a", 20000))
	h.waitFinishes(sid, 1)
	if n := h.cards(sid); n != 0 {
		t.Fatalf("%d cards for a 20 KB command", n)
	}
	if !strings.Contains(h.relay.text(sid), "denied: "+cardCommandTooLong) {
		t.Fatalf("text %q", h.relay.text(sid))
	}
	h.message(sid, "bigwrite 70000")
	h.waitFinishes(sid, 2)
	if n := h.cards(sid); n != 0 || !strings.Contains(h.relay.text(sid), "denied: "+cardTooLarge) {
		t.Fatalf("cards %d text %q", n, h.relay.text(sid))
	}
	h.message(sid, "bigwrite 10")
	h.approve(sid, 1, "allow", "user", "allow_always")
	h.waitFinishes(sid, 3)
	h.message(sid, "bigwrite 70000")
	h.waitFinishes(sid, 4)
	if n := h.cards(sid); n != 1 || strings.Count(h.relay.text(sid), "denied: "+cardTooLarge) != 2 {
		t.Fatalf("cards %d text %q", n, h.relay.text(sid))
	}
	if len(h.relay.invalid) != 0 {
		t.Fatalf("relay saw invalid events: %v", h.relay.invalid)
	}
}

// A Codex command too long for the card is declined without a card.
func TestCodexCardTooLongIsDeclined(t *testing.T) {
	h := newCodexHarness(t, nil)
	h.start()
	sid := h.startCodexSession("cmd " + strings.Repeat("a", 20000))
	h.waitFinishes(sid, 1)
	if n := h.cards(sid); n != 0 || !strings.Contains(h.relay.text(sid), "declined aaaa") {
		t.Fatalf("cards %d text %.200q", n, h.relay.text(sid))
	}
	h.message(sid, "cmd ls")
	h.approve(sid, 1, "allow", "user", "")
	h.waitFinishes(sid, 2)
	if n := h.cards(sid); n != 1 {
		t.Fatalf("cards %d", n)
	}
}

// Story 94.4: a Codex that asks anyway for a Mosaic tool (mcpApproval) gets
// an accept without a card in every mode, Read only included, whether its
// approval names the tool or not (codex-cli 0.160.0 leaves tool_name out).
func TestCodexMosaicToolApprovalAccepted(t *testing.T) {
	for _, elicitTool := range []string{"", "-"} {
		h := newCodexHarness(t, map[string]any{"mcpApproval": true, "elicitTool": elicitTool})
		h.start()
		for i, mode := range []Mode{ModeReadOnly, ModeAsk} {
			sid := h.startWith("codex", "echo hi", map[string]any{"mode": string(mode)})
			h.waitFinishes(sid, 1)
			h.writeCwd(sid, "a.md", []byte("# a\n"))
			h.message(sid, "publish a.md")
			h.waitFinishes(sid, 2)
			h.message(sid, "mcp")
			h.waitFinishes(sid, 3)
			if n := h.cards(sid); n != 0 {
				t.Fatalf("tool name %q, %s: %d approval cards", elicitTool, mode, n)
			}
			if text := h.relay.text(sid); !strings.Contains(text, "published") || !strings.Contains(text, "tools: ") {
				t.Fatalf("tool name %q, %s: text %q", elicitTool, mode, text)
			}
			if _, n := h.api.uploads(); n != i+1 {
				t.Fatalf("tool name %q, %s: uploads %d", elicitTool, mode, n)
			}
		}
		b, _ := os.ReadFile(filepath.Join(h.home, ".fakecodex", "elicitation-reply.json"))
		if !strings.Contains(string(b), `"action":"accept"`) || strings.Contains(string(b), "persist") {
			t.Fatalf("tool name %q: reply %s", elicitTool, b)
		}
	}
}

func TestCodexMosaicToolApprovalMatch(t *testing.T) {
	el := func(server, kind, tool string, meta bool) codexElicitation {
		var e codexElicitation
		raw := `{"serverName":"` + server + `"`
		if meta {
			raw += `,"_meta":{"codex_approval_kind":"` + kind + `"`
			if tool != "" {
				raw += `,"tool_name":"` + tool + `"`
			}
			raw += `}`
		}
		_ = json.Unmarshal([]byte(raw+"}"), &e)
		return e
	}
	for _, row := range []struct {
		e    codexElicitation
		want bool
	}{
		{el("archivist", "mcp_tool_call", "publish_artifact", true), true},
		{el("archivist", "mcp_tool_call", "search", true), true},
		{el("archivist", "mcp_tool_call", "", true), true},
		{el("archivist", "mcp_tool_call", "other_tool", true), false},
		{el("other", "mcp_tool_call", "publish_artifact", true), false},
		{el("archivist", "browser_auth", "publish_artifact", true), false},
		{el("archivist", "", "", false), false},
	} {
		if got := codexMosaicToolApproval(row.e); got != row.want {
			t.Errorf("%+v: %v, want %v", row.e, got, row.want)
		}
	}
}
