package connect

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// The Codex structs in codexproto.go are hand-written. This test checks
// them against a vendored subset of the pinned schema: every field the
// adapter sends or reads and every enum value it sends or matches exists.
//
// Regenerate (after `codex app-server generate-json-schema --experimental --out DIR`):
//
//	go test ./internal/connect/ -run TestCodexProtoMatchesSchema -codex-schema-src=DIR -codex-schema-version=X.Y.Z
var (
	codexSchemaSrc     = flag.String("codex-schema-src", "", "regenerate testdata/codex-schema.json from this generate-json-schema output")
	codexSchemaVersion = flag.String("codex-schema-version", "", "codex-cli version that produced -codex-schema-src (required with it)")
)

const codexSchemaFile = "testdata/codex-schema.json"

// codexSchemaDefs are the definitions the adapter touches; the three
// method unions are vendored as their method name lists only.
var codexSchemaDefs = []string{
	"InitializeParams", "InitializeResponse", "ClientInfo", "InitializeCapabilities",
	"GetAccountParams", "GetAccountResponse", "Account", "AuthMode", "AccountUpdatedNotification",
	"ModelListParams", "ModelListResponse", "Model", "ReasoningEffortOption", "ReasoningEffort",
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
	"LoginAccountParams", "LoginAccountResponse", "AccountLoginCompletedNotification",
	"CancelLoginAccountParams", "CancelLoginAccountResponse", "CancelLoginAccountStatus",
}

var codexMethodUnions = []string{"ClientRequest", "ClientNotification", "ServerRequest", "ServerNotification"}

func regenerateCodexSchema(t *testing.T, dir, version string) {
	if version == "" {
		t.Fatal("-codex-schema-version is required with -codex-schema-src")
	}
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
	b, _ := json.MarshalIndent(map[string]any{"source": "codex-cli " + version + " generate-json-schema --experimental",
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

// jsonFields maps each json-tagged field of struct type t to its struct
// type (pointer and slice element types unwrapped) or nil.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out[name] = ft
		} else {
			out[name] = nil
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
		regenerateCodexSchema(t, *codexSchemaSrc, *codexSchemaVersion)
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
	// Fields the adapter sends or reads through map literals and raw maps
	// (the codexproto structs are checked by reflection below).
	fields := map[string][]string{
		"GetAccountParams":                        {"refreshToken"},
		"ModelListParams":                         {"cursor"},
		"CommandExecutionRequestApprovalResponse": {"decision"},
		"FileChangeRequestApprovalResponse":       {"decision"},
		"McpServerElicitationRequestResponse":     {"action", "content", "_meta"},
		"PermissionsRequestApprovalResponse":      {"permissions", "scope"},
		"ToolRequestUserInputResponse":            {"answers"},
		"AgentMessageDeltaNotification":           {"itemId", "delta"},
		"ReasoningSummaryTextDeltaNotification":   {"itemId", "delta"},
		"TurnPlanUpdatedNotification":             {"plan"},
		"TurnPlanStep":                            {"step", "status"},
		"ThreadTokenUsageUpdatedNotification":     {"tokenUsage"},
		"ThreadTokenUsage":                        {"total", "last", "modelContextWindow"},
		"TokenUsageBreakdown":                     {"inputTokens", "outputTokens", "cachedInputTokens", "reasoningOutputTokens", "totalTokens"},
		"ErrorNotification":                       {"willRetry"},
		"FileUpdateChange":                        {"path", "diff", "kind"},
		"CommandExecutionRequestApprovalParams":   {"command", "cwd", "reason", "availableDecisions"},
		"FileChangeRequestApprovalParams":         {"reason", "grantRoot"},
	}
	// Every json-tagged field of each codexproto struct (nested structs
	// included) must exist in the schema definition it maps to.
	type structMap struct {
		v      any
		defs   []string
		nested map[string]string   // field -> definition of its struct ("" = open JSON, unchecked)
		only   map[string][]string // definition -> the only fields sent to it (shared structs)
	}
	threadFields := []string{"model", "cwd", "approvalPolicy", "approvalsReviewer", "sandbox", "config", "developerInstructions"}
	for _, m := range []structMap{
		{v: codexInitializeParams{}, defs: []string{"InitializeParams"},
			nested: map[string]string{"clientInfo": "ClientInfo", "capabilities": "InitializeCapabilities"}},
		{v: codexInitializeResult{}, defs: []string{"InitializeResponse"}},
		{v: codexAccountRead{}, defs: []string{"GetAccountResponse"}, nested: map[string]string{"account": "Account"}},
		{v: codexModelList{}, defs: []string{"ModelListResponse"}, nested: map[string]string{"data": "Model"}},
		{v: codexModel{}, defs: []string{"Model"}, nested: map[string]string{"supportedReasoningEfforts": "ReasoningEffortOption"}},
		{v: codexReasoningEffortOption{}, defs: []string{"ReasoningEffortOption"}},
		{v: codexThreadParams{}, defs: []string{"ThreadStartParams", "ThreadResumeParams"}, only: map[string][]string{
			"ThreadStartParams":  append([]string{"ephemeral"}, threadFields...),
			"ThreadResumeParams": append([]string{"threadId", "excludeTurns"}, threadFields...)}},
		{v: codexThreadResult{}, defs: []string{"ThreadStartResponse", "ThreadResumeResponse"},
			nested: map[string]string{"thread": "Thread", "sandbox": "SandboxPolicy"}},
		{v: codexTurnStartParams{}, defs: []string{"TurnStartParams"}, nested: map[string]string{"input": "UserInput"}},
		{v: codexTurnRef{}, defs: []string{"TurnInterruptParams"}},
		{v: codexThreadRef{}, defs: []string{"ThreadBackgroundTerminalsCleanParams"}},
		{v: codexTurnResult{}, defs: []string{"TurnStartResponse", "TurnStartedNotification", "TurnCompletedNotification"},
			nested: map[string]string{"turn": "Turn"}},
		{v: codexMCPStatus{}, defs: []string{"McpServerStatusUpdatedNotification"}},
		{v: codexAccountUpdated{}, defs: []string{"AccountUpdatedNotification"}},
		{v: codexItemEnvelope{}, defs: []string{"ItemStartedNotification", "ItemCompletedNotification"}},
		{v: codexItemHead{}, defs: []string{"ThreadItem"}},
		{v: codexApprovalHead{}, defs: []string{"CommandExecutionRequestApprovalParams", "FileChangeRequestApprovalParams"}},
		{v: codexElicitation{}, defs: []string{"McpServerElicitationRequestParams"}, nested: map[string]string{"_meta": ""}},
		{v: codexResolved{}, defs: []string{"ServerRequestResolvedNotification"}},
		{v: codexLoginStartParams{}, defs: []string{"LoginAccountParams"}},
		{v: codexLoginStartResult{}, defs: []string{"LoginAccountResponse"}},
		{v: codexLoginCompleted{}, defs: []string{"AccountLoginCompletedNotification"}},
		{v: codexLoginCancelParams{}, defs: []string{"CancelLoginAccountParams"}},
		{v: codexLoginCancelResult{}, defs: []string{"CancelLoginAccountResponse"}},
	} {
		tags := jsonFields(reflect.TypeOf(m.v))
		if m.only != nil {
			for name := range tags {
				listed := false
				for _, fs := range m.only {
					listed = listed || slices.Contains(fs, name)
				}
				if !listed {
					t.Errorf("%T field %q is sent to no definition", m.v, name)
				}
			}
		}
		for _, d := range m.defs {
			have := fieldsOf(d, "")
			for name, sub := range tags {
				if only, ok := m.only[d]; ok && !slices.Contains(only, name) {
					continue
				}
				if !have[name] {
					t.Errorf("%T field %q is not in %s", m.v, name, d)
				}
				def, ok := m.nested[name]
				if sub == nil || (ok && def == "") {
					continue
				}
				if !ok {
					t.Errorf("%T field %q is a struct with no schema mapping", m.v, name)
					continue
				}
				subHave := fieldsOf(def, "")
				for f := range jsonFields(sub) {
					if !subHave[f] {
						t.Errorf("%T field %q.%q is not in %s", m.v, name, f, def)
					}
				}
			}
		}
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
		// Story 78.32: the read_only and full_auto sandboxes (turn/start).
		{"SandboxPolicy", "readOnly"}:         {"type", "networkAccess"},
		{"SandboxPolicy", "dangerFullAccess"}: {"type"},
		{"ThreadItem", "userMessage"}:         {"id"},
		{"ThreadItem", "agentMessage"}:        {"id", "text"},
		{"ThreadItem", "reasoning"}:           {"id"},
		{"ThreadItem", "commandExecution"}:    {"id", "command", "cwd", "status", "aggregatedOutput", "exitCode", "durationMs"},
		{"ThreadItem", "mcpToolCall"}:         {"id", "server", "tool", "arguments", "status", "result", "error"},
		{"ThreadItem", "fileChange"}:          {"id", "changes", "status"},
		{"PatchChangeKind", "add"}:            {"type"},
		{"PatchChangeKind", "update"}:         {"type"},
		{"PatchChangeKind", "delete"}:         {"type"},
		// Posture-1 sign-in (Story 78.22): the device code variants.
		{"LoginAccountParams", "chatgptDeviceCode"}:   {"type"},
		{"LoginAccountResponse", "chatgptDeviceCode"}: {"type", "loginId", "userCode", "verificationUrl"},
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
		{"SandboxMode", ""}:                      {"read-only", "workspace-write", "danger-full-access"},
		{"AskForApproval", ""}:                   {"untrusted", "on-request", "never"},
		{"ApprovalsReviewer", ""}:                {"user"},
		{"TurnStatus", ""}:                       {"completed", "interrupted", "failed"},
		{"McpServerStartupState", ""}:            {"ready", "failed", "cancelled"},
		{"AuthMode", ""}:                         {"chatgpt"},
		{"TurnPlanStepStatus", ""}:               {"pending", "inProgress", "completed"},
		{"CommandExecutionStatus", ""}:           {"declined", "failed"},
		{"PatchApplyStatus", ""}:                 {"declined", "failed"},
		{"McpToolCallStatus", ""}:                {"failed"},
		{"CommandExecutionApprovalKind", ""}:     {"command", "writeStdin"},
		{"SandboxPolicy", "type"}:                {"readOnly", "workspaceWrite", "dangerFullAccess"},
		{"Account", "type"}:                      {"chatgpt"},
		{"ThreadItem", "type"}:                   {"userMessage", "agentMessage", "reasoning", "commandExecution", "mcpToolCall", "fileChange"},
		{"PatchChangeKind", "type"}:              {"add", "update", "delete"},
		{"UserInput", "type"}:                    {"text"},
		{"LoginAccountParams", "type"}:           {"chatgptDeviceCode"},
		{"LoginAccountResponse", "type"}:         {"chatgptDeviceCode"},
		{"CancelLoginAccountStatus", ""}:         {"canceled", "notFound"},
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
			"turn/interrupt", "thread/backgroundTerminals/clean", "account/login/start", "account/login/cancel"},
		"ClientNotification": {"initialized"},
		"ServerRequest": {"item/commandExecution/requestApproval", "item/fileChange/requestApproval",
			"mcpServer/elicitation/request", "item/tool/requestUserInput", "item/permissions/requestApproval"},
		"ServerNotification": {"turn/started", "turn/completed", "item/started", "item/completed", "item/agentMessage/delta",
			"item/reasoning/summaryTextDelta", "turn/plan/updated", "thread/tokenUsage/updated",
			"mcpServer/startupStatus/updated", "account/updated", "serverRequest/resolved", "error",
			"account/login/completed"},
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
