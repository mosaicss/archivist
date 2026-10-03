package connect

import "encoding/json"

// Hand-written Codex app-server (protocol v2, codex-cli 0.160.0) shapes for
// the subset the adapter reads. Fields the daemon only forwards stay
// json.RawMessage. The generated JSON Schema (generate-json-schema
// --experimental) is the reference these were written from; unions there
// become interface{} in generated Go, so the structs below are kept small
// and explicit instead.

// codexInitializeParams is the initialize request.
type codexInitializeParams struct {
	ClientInfo   codexClientInfo   `json:"clientInfo"`
	Capabilities codexCapabilities `json:"capabilities"`
}

type codexClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type codexCapabilities struct {
	// ExperimentalAPI enables thread/backgroundTerminals/clean.
	ExperimentalAPI    bool `json:"experimentalApi"`
	RequestAttestation bool `json:"requestAttestation"`
}

// codexInitializeResult is the initialize response.
type codexInitializeResult struct {
	UserAgent string `json:"userAgent"`
	CodexHome string `json:"codexHome"`
}

// codexAccountRead is the account/read response.
type codexAccountRead struct {
	Account *struct {
		Type     string `json:"type"`
		PlanType string `json:"planType"`
	} `json:"account"`
	RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
}

// codexModelList is the model/list response.
type codexModelList struct {
	Data []struct {
		ID        string `json:"id"`
		Model     string `json:"model"`
		IsDefault bool   `json:"isDefault"`
		Hidden    bool   `json:"hidden"`
	} `json:"data"`
	NextCursor *string `json:"nextCursor"`
}

// codexThreadParams is thread/start, or thread/resume when ThreadID is set.
// approvalPolicy travels here (never in -c or config, where "untrusted" is
// a retired value Codex refuses).
type codexThreadParams struct {
	ThreadID          string         `json:"threadId,omitempty"`
	Model             string         `json:"model"`
	Cwd               string         `json:"cwd"`
	ApprovalPolicy    string         `json:"approvalPolicy"`
	ApprovalsReviewer string         `json:"approvalsReviewer"`
	Sandbox           string         `json:"sandbox"`
	Config            map[string]any `json:"config,omitempty"`
	Ephemeral         *bool          `json:"ephemeral,omitempty"`
	// ExcludeTurns (resume) returns thread metadata without its history.
	ExcludeTurns bool `json:"excludeTurns,omitempty"`
}

// codexThreadResult is the thread/start and thread/resume response.
type codexThreadResult struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	Model              string          `json:"model"`
	ModelProvider      string          `json:"modelProvider"`
	Cwd                string          `json:"cwd"`
	InstructionSources []string        `json:"instructionSources"`
	ApprovalPolicy     json.RawMessage `json:"approvalPolicy"`
	ApprovalsReviewer  string          `json:"approvalsReviewer"`
	Sandbox            struct {
		Type          string   `json:"type"`
		WritableRoots []string `json:"writableRoots"`
		// Pointers: a missing field fails the proof instead of reading false.
		NetworkAccess       *bool `json:"networkAccess"`
		ExcludeSlashTmp     *bool `json:"excludeSlashTmp"`
		ExcludeTmpdirEnvVar *bool `json:"excludeTmpdirEnvVar"`
	} `json:"sandbox"`
	ReasoningEffort *string `json:"reasoningEffort"`
}

// codexTextInput is one turn/start input item.
type codexTextInput struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	TextElements []any  `json:"text_elements"`
}

type codexTurnStartParams struct {
	ThreadID string           `json:"threadId"`
	Input    []codexTextInput `json:"input"`
}

type codexTurnRef struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type codexThreadRef struct {
	ThreadID string `json:"threadId"`
}

// codexTurnResult is the turn/start response and the turn/started and
// turn/completed notification params.
type codexTurnResult struct {
	Turn struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Error  json.RawMessage `json:"error"`
	} `json:"turn"`
}

// codexMCPStatus is mcpServer/startupStatus/updated.
type codexMCPStatus struct {
	ThreadID *string `json:"threadId"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Error    *string `json:"error"`
}

// codexAccountUpdated is account/updated (authMode null when logged out).
type codexAccountUpdated struct {
	AuthMode *string `json:"authMode"`
}

// codexItemEnvelope is item/started and item/completed.
type codexItemEnvelope struct {
	Item     json.RawMessage `json:"item"`
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
}

// codexItemHead is the routing view of a ThreadItem.
type codexItemHead struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status string `json:"status"`
}

// codexApprovalHead is the routing view of the approval server requests.
type codexApprovalHead struct {
	ThreadID string  `json:"threadId"`
	TurnID   *string `json:"turnId"`
	ItemID   string  `json:"itemId"`
}

// codexElicitation is the routing view of mcpServer/elicitation/request.
type codexElicitation struct {
	ServerName string  `json:"serverName"`
	TurnID     *string `json:"turnId"`
	Mode       string  `json:"mode"`
	Meta       *struct {
		ApprovalKind string `json:"codex_approval_kind"`
	} `json:"_meta"`
}

// codexResolved is serverRequest/resolved.
type codexResolved struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}
