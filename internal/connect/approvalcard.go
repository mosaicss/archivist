package connect

import (
	"encoding/json"

	"github.com/mosaicss/archivist/internal/taskscope"
)

// Approval cards (Story 94.4). The daemon never sends a cut down approval
// card: an approval whose card would be cut to fit (a shell command longer
// than the tool input preview, or a card that does not fit one relay frame
// as it is) is denied without a card, so an answer never covers input the
// card's data left out.

// sessionScopeKey is the approval descriptor key stating what an "Allow for
// session" on a Claude card covers: {"kind", "value"} (sessionrules.go).
const sessionScopeKey = "archivist_session_scope"

// Card refusal messages (the harness reports them as the tool result).
const (
	cardCommandTooLong = "Denied: this command is too long to show on the approval card. Run it in smaller steps."
	cardTooLarge       = "Denied: this tool call is too large to show on the approval card. Split it into smaller steps."
)

// claudeApprovalCard is the tool-approval-request for one Claude
// can_use_tool request (request decoded with decodeNumbers): the request
// itself as the descriptor, plus the session scope.
func claudeApprovalCard(requestID string, request map[string]any) Chunk {
	toolCallID, _ := request["tool_use_id"].(string)
	if toolCallID == "" {
		toolCallID = requestID
	}
	descriptor := make(map[string]any, len(request)+1)
	for k, v := range request {
		descriptor[k] = v
	}
	tool, _ := request["tool_name"].(string)
	kind, value := scopeOf(tool, request["input"])
	descriptor[sessionScopeKey] = map[string]any{"kind": kind, "value": value}
	return Chunk{"type": "tool-approval-request", "approvalId": requestID, "toolCallId": toolCallID, "approvalDescriptor": descriptor}
}

// claudeCardFor builds the card of a raw can_use_tool request; ok is false
// when the request is not a JSON object.
func claudeCardFor(requestID string, request json.RawMessage) (Chunk, bool) {
	var m map[string]any
	if decodeNumbers(request, &m) != nil || m == nil {
		return nil, false
	}
	return claudeApprovalCard(requestID, m), true
}

// cardRefusal says why an approval card cannot be shown whole ("" when it
// can): a shell command longer than previewBytes, or a card that does not
// fit one relay frame unreduced (fits: Outbox.Fits).
func cardRefusal(c Chunk, fits func(Chunk) bool) string {
	if len(cardCommand(c)) > previewBytes {
		return cardCommandTooLong
	}
	if !fits(c) {
		return cardTooLarge
	}
	return ""
}

// cardCommand is the shell command an approval card shows: a Claude Bash
// call's command, or a Codex command approval's command.
func cardCommand(c Chunk) string {
	d, _ := c["approvalDescriptor"].(map[string]any)
	if d == nil {
		return ""
	}
	if d["tool_name"] == "Bash" {
		in, _ := d["input"].(map[string]any)
		if s, ok := in["command"].(string); ok {
			return s
		}
	}
	s, _ := d["command"].(string)
	return s
}

// claudeCardRefusal checks the card a can_use_tool request would raise
// (the one the translator builds from the same request) before any
// remembered rule answers it; "" when the card can be shown whole.
func (s *session) claudeCardRefusal(requestID string, request json.RawMessage) string {
	card, ok := claudeCardFor(requestID, request)
	if !ok {
		return cardTooLarge
	}
	msg := cardRefusal(card, s.outbox.Fits)
	if msg != "" {
		s.log.Printf("approval card %s cannot be shown whole; denied", requestID)
	}
	return msg
}

// codexMosaicToolApproval reports a Codex MCP tool approval for a Mosaic
// tool (Story 94.4), which is accepted without a card in every mode. Codex
// already runs the archivist tools without asking (approval mode approve);
// this covers a Codex that asks anyway. The archivist server serves only
// Mosaic task tools, and a session with any other MCP server fails its
// startup proof, so an approval from the archivist server is one, unless
// it names a tool that is not a Mosaic tool (then it stays a card).
func codexMosaicToolApproval(e codexElicitation) bool {
	if e.ServerName != "archivist" || e.Meta == nil || e.Meta.ApprovalKind != "mcp_tool_call" {
		return false
	}
	return e.Meta.ToolName == "" || taskscope.IsMosaicTool(e.Meta.ToolName)
}
