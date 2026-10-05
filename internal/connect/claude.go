package connect

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mosaicss/archivist/internal/taskscope"
)

// Claude Code invocation (Story 78.16). Every flag is fixed here; the relay
// supplies only the prompt text and the session control ids (Story 78.32).
// --permission-mode is always passed, mapped from the session's mode
// (controls.go); --allow-dangerously-skip-permissions only under a
// full_auto ceiling. Never --bare, --dangerously-skip-permissions, or the
// auto and dontAsk modes.

// ClaudeBuiltinTools are the built-in tools a session may use; whether a
// call asks through the approval card is the session's mode's decision.
const ClaudeBuiltinTools = "Bash,Read,Edit,Write,Glob,Grep"

// Story 78.33: Claude Code's own web search (it runs at Anthropic, not on
// this machine) is added to --tools and pre-allowed in --allowedTools only
// when the session has web search; it is a read and never asks. WebFetch,
// which fetches pages from this machine, is never passed, and the launch
// env turns it off too (ClaudeDisableWebFetchKey).
const (
	ClaudeWebSearchTool = "WebSearch"
	ClaudeWebFetchTool  = "WebFetch"
)

// claudeFlagSettings is the --settings value of every launch (Story 78.32).
const claudeFlagSettings = `{"useAutoModeDuringPlan":false}`

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
	// PermissionMode is the session's --permission-mode and AllowBypass the
	// full_auto ceiling's --allow-dangerously-skip-permissions (Story 78.32;
	// set per session by claudeLaunchConfig, "" = default).
	PermissionMode string
	AllowBypass    bool
	// WebSearch adds Claude Code's WebSearch tool, pre-allowed (Story 78.33;
	// set per session by claudeLaunchConfig from the daemon's Config.WebSearch).
	WebSearch bool
	// Login is the Claude login a session-bound sandbox session starts on
	// (Story 78.38; outside the sandbox only a claude.ai subscription runs):
	// ClaudeLoginConsole when the sandbox home already holds a logged in
	// login other than claude.ai (a remembered Console login).
	Login ClaudeLogin
	// SignInFile is the sandbox's sign in request file (Story 78.38,
	// ARCHIVIST_SIGNIN_FILE; "" = none): while a Claude sign in waits the
	// session polls it for the user's switch to a Console login or to their
	// own API key. Ignored outside the session-bound sandbox mode.
	SignInFile string
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

// claudeTools is the --tools value: the built-in tools, plus WebSearch when
// the session has web search (Story 78.33).
func claudeTools(cfg ClaudeConfig) string {
	if cfg.WebSearch {
		return ClaudeBuiltinTools + "," + ClaudeWebSearchTool
	}
	return ClaudeBuiltinTools
}

// claudeAllowedTools is the --allowedTools value: the archivist read tools,
// plus WebSearch when the session has web search, so Claude Code never
// asks for it (verified on 2.1.285: without the entry, default mode raises
// can_use_tool for WebSearch).
func claudeAllowedTools(cfg ClaudeConfig) string {
	if cfg.WebSearch {
		return archivistAllowedTools() + "," + ClaudeWebSearchTool
	}
	return archivistAllowedTools()
}

// claudeArgs builds the fixed argv for one session process. guidancePath is
// Mosaic's research guidance (Story 78.31), appended to Claude Code's default
// system prompt; the setting sources stay empty, so it is the only addition.
func claudeArgs(cfg ClaudeConfig, mcpConfigPath, guidancePath, cwd, resumeID string) []string {
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
		"--tools", claudeTools(cfg),
		"--allowedTools", claudeAllowedTools(cfg),
	}
	mode := cfg.PermissionMode
	if mode == "" {
		mode = "default"
	}
	args = append(args, "--permission-mode", mode)
	if cfg.AllowBypass {
		args = append(args, "--allow-dangerously-skip-permissions")
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
	// Plan mode (read_only) must never let the auto mode classifier approve
	// shell commands: set on every launch, so a later switch to plan holds
	// too (verified on 2.1.280: get_settings reports it from flagSettings
	// alongside --setting-sources "").
	args = append(args, "--settings", claudeFlagSettings)
	// The file form: no argv length limit, and print mode reads it on resume too.
	args = append(args, "--append-system-prompt-file", guidancePath)
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

// setModeFrame asks Claude Code to switch the permission mode (Story 78.32).
func setModeFrame(requestID, mode string) map[string]any {
	return map[string]any{"type": "control_request", "request_id": requestID,
		"request": map[string]any{"subtype": "set_permission_mode", "mode": mode}}
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

// initToolsProblem checks the init frame's tools against the launch (Story
// 78.33): WebFetch is never allowed, and WebSearch only with web search on.
// problem fails the session closed; warning (web search on, WebSearch not
// reported) is only logged, since search being unavailable is not unsafe.
func initToolsProblem(tools []string, webSearch bool) (problem, warning string) {
	var problems []string
	if slices.Contains(tools, ClaudeWebFetchTool) {
		problems = append(problems, "it reports the WebFetch tool, which archivist connect never enables")
	}
	if !webSearch && slices.Contains(tools, ClaudeWebSearchTool) {
		problems = append(problems, "it reports the WebSearch tool, but web search is off for this session")
	}
	if webSearch && !slices.Contains(tools, ClaudeWebSearchTool) {
		warning = "web search is on, but Claude Code did not report its WebSearch tool; the session runs without web search"
	}
	return strings.Join(problems, "; "), warning
}

// initProblem returns why an init frame fails the subscription proof:
// permissionMode must be one of modes (the session's mapped mode, or one a
// pending set_permission_mode asked for).
func initProblem(f claudeFrame, modes []string) string {
	return initLoginProblem(f, modes, ClaudeLoginSubscription)
}

// initLoginProblem is initProblem for a session's login (Story 78.38): a
// claude.ai login must report apiKeySource none and API key mode the
// environment key. A Console login (sandbox only) must report a key source
// (never none: a home whose login exists only for its settings, such as an
// apiKeyHelper, leaves the -p run without a credential) and, when the
// spawn's claude auth status named one (statusSource), that same source.
func initLoginProblem(f claudeFrame, modes []string, login ClaudeLogin, statusSource ...string) string {
	var problems []string
	switch login {
	case ClaudeLoginAPIKey:
		if f.APIKeySource != ClaudeAPIKeySource {
			problems = append(problems, fmt.Sprintf("apiKeySource is %q, not %q", f.APIKeySource, ClaudeAPIKeySource))
		}
	case ClaudeLoginConsole:
		switch {
		case f.APIKeySource == "" || f.APIKeySource == "none":
			problems = append(problems, fmt.Sprintf("apiKeySource is %q, not the Console login's key", f.APIKeySource))
		case len(statusSource) > 0 && statusSource[0] != "" && f.APIKeySource != statusSource[0]:
			problems = append(problems, fmt.Sprintf("apiKeySource is %q, not %q as claude auth status reported", f.APIKeySource, statusSource[0]))
		}
	default:
		if f.APIKeySource != "none" {
			problems = append(problems, fmt.Sprintf("apiKeySource is %q, not \"none\"", f.APIKeySource))
		}
	}
	if len(modes) == 0 || !slices.Contains(modes, f.PermissionMode) {
		problems = append(problems, fmt.Sprintf("permissionMode is %q, not \"%s\"", f.PermissionMode, strings.Join(modes, "\" or \"")))
	}
	return strings.Join(problems, "; ")
}
