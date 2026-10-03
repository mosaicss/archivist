package connect

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mosaicss/archivist/internal/taskscope"
)

// Claude Code invocation (Story 78.16). Every flag is fixed here; the relay
// supplies only the prompt text. Never --permission-mode, --bare or
// --dangerously-skip-permissions.

// ClaudeBuiltinTools are the built-in tools a session may use; every
// non-read action still asks through the approval card.
const ClaudeBuiltinTools = "Bash,Read,Edit,Write,Glob,Grep"

// DefaultSettingSources keeps user, project and local settings out of the
// session (no settings env, apiKeyHelper, hooks or plugins). Claude Code
// 2.1.280 accepts the empty list (verified live, ledger start 1).
const DefaultSettingSources = ""

// ClaudeConfig is the local, non-relay configuration of the adapter.
type ClaudeConfig struct {
	Bin            string
	Model          string
	Effort         string
	SettingSources string
	// Executable is this archivist binary, run as the MCP server.
	Executable string
	// BaseURL is passed to `mcp serve` only when ARCHIVIST_BASE_URL is set.
	BaseURL string
}

// archivistAllowedTools pre-allows the task mode archivist tools, except
// the approval-required ones (publish_artifact): those reach the
// permission prompt tool (can_use_tool) on every call.
func archivistAllowedTools() string {
	tools := taskscope.AutoAllowedTools()
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = "mcp__archivist__" + t
	}
	return strings.Join(out, ",")
}

// claudeArgs builds the fixed argv for one session process.
func claudeArgs(cfg ClaudeConfig, mcpConfigPath, cwd, resumeID string) []string {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--strict-mcp-config",
		"--mcp-config", mcpConfigPath,
		"--permission-prompt-tool", "stdio",
		"--setting-sources", cfg.SettingSources,
		"--tools", ClaudeBuiltinTools,
		"--allowedTools", archivistAllowedTools(),
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	if resumeID != "" {
		args = append(args, "--resume="+resumeID)
	}
	// Last: --add-dir is variadic, so nothing may follow its value.
	return append(args, "--add-dir", cwd)
}

// mcpConfig is the --mcp-config file: only archivist, run from this binary
// against a token file the daemon rotates. publish carries the
// publish_artifact flags when the token was granted the publish scope.
func mcpConfig(cfg ClaudeConfig, tokenFile string, publish []string) ([]byte, error) {
	env := map[string]string{}
	if cfg.BaseURL != "" {
		env["ARCHIVIST_BASE_URL"] = cfg.BaseURL
	}
	return json.Marshal(map[string]any{"mcpServers": map[string]any{"archivist": map[string]any{
		"command": cfg.Executable,
		"args":    mcpServeArgs(tokenFile, publish),
		"env":     env,
	}}})
}

// mcpServeArgs is the archivist MCP server argv (both harnesses).
func mcpServeArgs(tokenFile string, publish []string) []string {
	return append([]string{"mcp", "serve", "--token-file", tokenFile}, publish...)
}

// Stream-json frames written to Claude's stdin.

func userFrame(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
}

func allowFrame(requestID string, input json.RawMessage) map[string]any {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	return controlSuccess(requestID, map[string]any{"behavior": "allow", "updatedInput": input})
}

func denyFrame(requestID, message string) map[string]any {
	return controlSuccess(requestID, map[string]any{"behavior": "deny", "message": message})
}

func controlSuccess(requestID string, body map[string]any) map[string]any {
	return map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": requestID, "response": body}}
}

func controlError(requestID, msg string) map[string]any {
	return map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "error", "request_id": requestID, "error": msg}}
}

func interruptFrame(requestID string) map[string]any {
	return map[string]any{"type": "control_request", "request_id": requestID,
		"request": map[string]any{"subtype": "interrupt"}}
}

// claudeFrame is the routing view of one stdout line.
type claudeFrame struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	SessionID      string          `json:"session_id"`
	APIKeySource   string          `json:"apiKeySource"`
	PermissionMode string          `json:"permissionMode"`
	Tools          []string        `json:"tools"`
	MCPServers     json.RawMessage `json:"mcp_servers"`
	Plugins        json.RawMessage `json:"plugins"`
	Version        string          `json:"claude_code_version"`
	Model          string          `json:"model"`
	RequestID      string          `json:"request_id"`
	Request        json.RawMessage `json:"request"`
	IsError        *bool           `json:"is_error"`
	TerminalReason string          `json:"terminal_reason"`
}

type controlRequest struct {
	Subtype   string          `json:"subtype"`
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// initProblem returns why an init frame fails the subscription proof.
func initProblem(f claudeFrame) string {
	var problems []string
	if f.APIKeySource != "none" {
		problems = append(problems, fmt.Sprintf("apiKeySource is %q, not \"none\"", f.APIKeySource))
	}
	if f.PermissionMode != "default" {
		problems = append(problems, fmt.Sprintf("permissionMode is %q, not \"default\"", f.PermissionMode))
	}
	return strings.Join(problems, "; ")
}
