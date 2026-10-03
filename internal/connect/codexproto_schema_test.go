package connect

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The Codex structs in codexproto.go are hand-written. This test checks
// them against a vendored subset of the pinned schema: every field the
// adapter sends or reads and every enum value it sends or matches exists.
//
// Regenerate (after `codex app-server generate-json-schema --experimental --out DIR`):
//
//	go test ./internal/connect/ -run TestCodexProtoMatchesSchema -codex-schema-src=DIR
var codexSchemaSrc = flag.String("codex-schema-src", "", "regenerate testdata/codex-schema.json from this generate-json-schema output")

const codexSchemaFile = "testdata/codex-schema.json"

// codexSchemaDefs are the definitions the adapter touches; the three
// method unions are vendored as their method name lists only.
var codexSchemaDefs = []string{
	"InitializeParams", "InitializeResponse", "ClientInfo", "InitializeCapabilities",
	"GetAccountParams", "GetAccountResponse", "Account", "AuthMode", "AccountUpdatedNotification",
	"ModelListParams", "ModelListResponse", "Model",
	"ThreadStartParams", "ThreadStartResponse", "ThreadResumeParams", "ThreadResumeResponse", "Thread",
	"SandboxPolicy", "SandboxMode", "AskForApproval", "ApprovalsReviewer",
	"TurnStartParams", "TurnStartResponse", "Turn", "TurnStatus", "UserInput", "TurnInterruptParams",
	"ThreadBackgroundTerminalsCleanParams", "TurnStartedNotification", "TurnCompletedNotification",
	"ItemStartedNotification", "ItemCompletedNotification", "ThreadItem", "AgentMessageDeltaNotification",
	"ReasoningSummaryTextDeltaNotification", "TurnPlanUpdatedNotification", "TurnPlanStep", "TurnPlanStepStatus",
	"ThreadTokenUsageUpdatedNotification", "ThreadTokenUsage", "TokenUsageBreakdown",
	"McpServerStatusUpdatedNotification", "McpServerStartupState", "ServerRequestResolvedNotification",
	"ErrorNotification", "FileUpdateChange", "PatchChangeKind", "CommandExecutionStatus", "PatchApplyStatus",
	"McpToolCallStatus", "CommandExecutionRequestApprovalParams", "CommandExecutionRequestApprovalResponse",
	"CommandExecutionApprovalDecision", "CommandExecutionApprovalKind", "FileChangeRequestApprovalParams",
	"FileChangeRequestApprovalResponse", "FileChangeApprovalDecision", "McpServerElicitationRequestParams",
	"McpServerElicitationRequestResponse", "McpServerElicitationAction", "PermissionsRequestApprovalResponse",
	"PermissionGrantScope", "ToolRequestUserInputResponse",
}

var codexMethodUnions = []string{"ClientRequest", "ClientNotification", "ServerRequest", "ServerNotification"}

func regenerateCodexSchema(t *testing.T, dir string) {
	load := func(name string) map[string]any {
		var v map[string]any
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		defs, _ := v["definitions"].(map[string]any)
		return defs
	}
	v2, all := load("codex_app_server_protocol.v2.schemas.json"), load("codex_app_server_protocol.schemas.json")
	out := map[string]any{}
	for _, n := range codexSchemaDefs {
		d, ok := v2[n]
		if !ok {
			d, ok = all[n]
		}
		if !ok {
			t.Fatalf("definition %s not in %s", n, dir)
		}
		out[n] = d
	}
	methods := map[string]any{}
	for _, n := range codexMethodUnions {
		d, ok := all[n]
		if !ok {
			d = v2[n]
		}
		var names []string
		for _, v := range variants(d) {
			props, _ := v["properties"].(map[string]any)
			m, _ := props["method"].(map[string]any)
			names = append(names, enumOf(m)...)
		}
		sort.Strings(names)
		methods[n] = names
	}
	b, _ := json.MarshalIndent(map[string]any{"source": "codex-cli 0.160.0 generate-json-schema --experimental",
		"definitions": out, "methods": methods}, "", " ")
	if err := os.WriteFile(codexSchemaFile, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// variants returns d and every nested oneOf/anyOf/allOf member.
func variants(d any) []map[string]any {
	m, ok := d.(map[string]any)
	if !ok {
		return nil
	}
	out := []map[string]any{m}
	for _, k := range []string{"oneOf", "anyOf", "allOf"} {
		list, _ := m[k].([]any)
		for _, x := range list {
			out = append(out, variants(x)...)
		}
	}
	return out
}

func enumOf(m map[string]any) []string {
	var out []string
	list, _ := m["enum"].([]any)
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestCodexProtoMatchesSchema(t *testing.T) {
	if *codexSchemaSrc != "" {
		regenerateCodexSchema(t, *codexSchemaSrc)
	}
	b, err := os.ReadFile(codexSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions map[string]any      `json:"definitions"`
		Methods     map[string][]string `json:"methods"`
	}
	if err := json.Unmarshal(b, &schema); err != nil {
		t.Fatal(err)
	}
	def := func(name string) any {
		d, ok := schema.Definitions[name]
		if !ok {
			t.Fatalf("definition %s not vendored", name)
		}
		return d
	}
	// fields of a definition (any variant), or of the variant whose
	// discriminator property "type" is tag.
	fieldsOf := func(name, tag string) map[string]bool {
		set := map[string]bool{}
		for _, v := range variants(def(name)) {
			props, _ := v["properties"].(map[string]any)
			if tag != "" {
				tp, _ := props["type"].(map[string]any)
				found := false
				for _, e := range enumOf(tp) {
					found = found || e == tag
				}
				if !found {
					continue
				}
			}
			for k := range props {
				set[k] = true
			}
		}
		return set
	}
	enumsOf := func(name, prop string) map[string]bool {
		set := map[string]bool{}
		for _, v := range variants(def(name)) {
			if prop == "" {
				for _, e := range enumOf(v) {
					set[e] = true
				}
				continue
			}
			props, _ := v["properties"].(map[string]any)
			p, _ := props[prop].(map[string]any)
			for _, e := range enumOf(p) {
				set[e] = true
			}
		}
		return set
	}
	fields := map[string][]string{
		// sent
		"InitializeParams":                        {"clientInfo", "capabilities"},
		"ClientInfo":                              {"name", "title", "version"},
		"InitializeCapabilities":                  {"experimentalApi", "requestAttestation"},
		"GetAccountParams":                        {"refreshToken"},
		"ModelListParams":                         {"cursor"},
		"ThreadStartParams":                       {"model", "cwd", "approvalPolicy", "approvalsReviewer", "sandbox", "config", "ephemeral"},
		"ThreadResumeParams":                      {"threadId", "model", "cwd", "approvalPolicy", "approvalsReviewer", "sandbox", "config", "excludeTurns"},
		"TurnStartParams":                         {"threadId", "input"},
		"TurnInterruptParams":                     {"threadId", "turnId"},
		"ThreadBackgroundTerminalsCleanParams":    {"threadId"},
		"CommandExecutionRequestApprovalResponse": {"decision"},
		"FileChangeRequestApprovalResponse":       {"decision"},
		"McpServerElicitationRequestResponse":     {"action", "content", "_meta"},
		"PermissionsRequestApprovalResponse":      {"permissions", "scope"},
		"ToolRequestUserInputResponse":            {"answers"},
		// read
		"InitializeResponse":                    {"codexHome"},
		"GetAccountResponse":                    {"account", "requiresOpenaiAuth"},
		"ModelListResponse":                     {"data", "nextCursor"},
		"Model":                                 {"id", "model", "isDefault", "hidden"},
		"ThreadStartResponse":                   {"thread", "model", "modelProvider", "cwd", "instructionSources", "approvalPolicy", "approvalsReviewer", "sandbox", "reasoningEffort"},
		"ThreadResumeResponse":                  {"thread", "model", "modelProvider", "cwd", "instructionSources", "approvalPolicy", "approvalsReviewer", "sandbox", "reasoningEffort"},
		"Thread":                                {"id"},
		"TurnStartResponse":                     {"turn"},
		"Turn":                                  {"id", "status", "error"},
		"TurnStartedNotification":               {"threadId", "turn"},
		"TurnCompletedNotification":             {"threadId", "turn"},
		"ItemStartedNotification":               {"item", "threadId", "turnId"},
		"ItemCompletedNotification":             {"item", "threadId", "turnId"},
		"AgentMessageDeltaNotification":         {"itemId", "delta"},
		"ReasoningSummaryTextDeltaNotification": {"itemId", "delta"},
		"TurnPlanUpdatedNotification":           {"plan"},
		"TurnPlanStep":                          {"step", "status"},
		"ThreadTokenUsageUpdatedNotification":   {"tokenUsage"},
		"ThreadTokenUsage":                      {"total"},
		"TokenUsageBreakdown":                   {"inputTokens", "outputTokens", "cachedInputTokens", "reasoningOutputTokens"},
		"McpServerStatusUpdatedNotification":    {"threadId", "name", "status", "error"},
		"AccountUpdatedNotification":            {"authMode"},
		"ServerRequestResolvedNotification":     {"threadId", "requestId"},
		"ErrorNotification":                     {"willRetry"},
		"FileUpdateChange":                      {"path", "diff", "kind"},
		"CommandExecutionRequestApprovalParams": {"kind", "threadId", "turnId", "itemId", "command", "cwd", "reason", "availableDecisions"},
		"FileChangeRequestApprovalParams":       {"threadId", "turnId", "itemId", "reason", "grantRoot"},
		"McpServerElicitationRequestParams":     {"serverName", "turnId", "mode", "_meta"},
	}
	for name, want := range fields {
		have := fieldsOf(name, "")
		for _, f := range want {
			if !have[f] {
				t.Errorf("%s has no field %q", name, f)
			}
		}
	}
	tagged := map[[2]string][]string{
		{"Account", "chatgpt"}:              {"type", "planType"},
		{"UserInput", "text"}:               {"type", "text", "text_elements"},
		{"SandboxPolicy", "workspaceWrite"}: {"type", "writableRoots", "networkAccess", "excludeSlashTmp", "excludeTmpdirEnvVar"},
		{"ThreadItem", "userMessage"}:       {"id"},
		{"ThreadItem", "agentMessage"}:      {"id", "text"},
		{"ThreadItem", "reasoning"}:         {"id"},
		{"ThreadItem", "commandExecution"}:  {"id", "command", "cwd", "status", "aggregatedOutput", "exitCode", "durationMs"},
		{"ThreadItem", "mcpToolCall"}:       {"id", "server", "tool", "arguments", "status", "result", "error"},
		{"ThreadItem", "fileChange"}:        {"id", "changes", "status"},
		{"PatchChangeKind", "add"}:          {"type"},
		{"PatchChangeKind", "update"}:       {"type"},
		{"PatchChangeKind", "delete"}:       {"type"},
	}
	for k, want := range tagged {
		have := fieldsOf(k[0], k[1])
		if len(have) == 0 {
			t.Errorf("%s has no %q variant", k[0], k[1])
		}
		for _, f := range want {
			if !have[f] {
				t.Errorf("%s %q has no field %q", k[0], k[1], f)
			}
		}
	}
	enums := map[[2]string][]string{
		{"CommandExecutionApprovalDecision", ""}: {"accept", "acceptForSession", "decline", "cancel"},
		{"FileChangeApprovalDecision", ""}:       {"accept", "acceptForSession", "decline", "cancel"},
		{"McpServerElicitationAction", ""}:       {"accept", "decline", "cancel"},
		{"PermissionGrantScope", ""}:             {"turn"},
		{"SandboxMode", ""}:                      {"workspace-write"},
		{"AskForApproval", ""}:                   {"untrusted"},
		{"ApprovalsReviewer", ""}:                {"user"},
		{"TurnStatus", ""}:                       {"completed", "interrupted", "failed"},
		{"McpServerStartupState", ""}:            {"ready", "failed", "cancelled"},
		{"AuthMode", ""}:                         {"chatgpt"},
		{"TurnPlanStepStatus", ""}:               {"pending", "inProgress", "completed"},
		{"CommandExecutionStatus", ""}:           {"declined", "failed"},
		{"PatchApplyStatus", ""}:                 {"declined", "failed"},
		{"McpToolCallStatus", ""}:                {"failed"},
		{"CommandExecutionApprovalKind", ""}:     {"command", "writeStdin"},
		{"SandboxPolicy", "type"}:                {"workspaceWrite"},
		{"Account", "type"}:                      {"chatgpt"},
		{"ThreadItem", "type"}:                   {"userMessage", "agentMessage", "reasoning", "commandExecution", "mcpToolCall", "fileChange"},
		{"PatchChangeKind", "type"}:              {"add", "update", "delete"},
		{"UserInput", "type"}:                    {"text"},
	}
	for k, want := range enums {
		have := enumsOf(k[0], k[1])
		for _, e := range want {
			if !have[e] {
				t.Errorf("%s %s has no value %q", k[0], k[1], e)
			}
		}
	}
	methods := map[string][]string{
		"ClientRequest": {"initialize", "account/read", "model/list", "thread/start", "thread/resume", "turn/start",
			"turn/interrupt", "thread/backgroundTerminals/clean"},
		"ClientNotification": {"initialized"},
		"ServerRequest": {"item/commandExecution/requestApproval", "item/fileChange/requestApproval",
			"mcpServer/elicitation/request", "item/tool/requestUserInput", "item/permissions/requestApproval"},
		"ServerNotification": {"turn/started", "turn/completed", "item/started", "item/completed", "item/agentMessage/delta",
			"item/reasoning/summaryTextDelta", "turn/plan/updated", "thread/tokenUsage/updated",
			"mcpServer/startupStatus/updated", "account/updated", "serverRequest/resolved", "error"},
	}
	for union, want := range methods {
		have := map[string]bool{}
		for _, m := range schema.Methods[union] {
			have[m] = true
		}
		for _, m := range want {
			if !have[m] {
				t.Errorf("%s has no method %q", union, m)
			}
		}
	}
}
