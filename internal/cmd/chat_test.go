package cmd_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/cmd"
)

// executeChat runs the chat command against a fake server URL with the given args.
// Returns stdout, stderr, and error.
func executeChat(t *testing.T, serverURL string, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
	t.Setenv("ARCHIVIST_BASE_URL", serverURL)

	root := cmd.NewRootCmd("0.2.0", "abc1234", "2026-05-20")

	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(args)

	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

// --- SSE helpers ---

func sseData(v interface{}) string {
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n"
}

// --- Tests ---

func TestChatRequestBody(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat" {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			receivedBody = buf.Bytes()
			// Respond with a minimal SSE stream
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Hello from AAPL"}))
			_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "conv-123"}))
			_, _ = fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	defer srv.Close()

	_, _, _ = executeChat(t, srv.URL, "chat",
		"What did AAPL say about supply chain?",
		"--company", "aapl_us",
		"--filing-type", "10-K",
		"--date-from", "2024-01-01",
		"--date-to", "2024-12-31",
	)

	if len(receivedBody) == 0 {
		t.Fatal("no request body received")
	}

	var body map[string]interface{}
	if err := json.Unmarshal(receivedBody, &body); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", err, string(receivedBody))
	}

	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		t.Fatal("messages array missing or empty")
	}
	msg := messages[0].(map[string]interface{})
	if msg["role"] != "user" {
		t.Errorf("messages[0].role: got %v, want user", msg["role"])
	}
	if msg["id"] == "" {
		t.Error("messages[0].id: missing UUID")
	}

	sp, ok := body["structuredParams"].(map[string]interface{})
	if !ok {
		t.Fatal("structuredParams missing")
	}
	if sp["company"] != "aapl_us" {
		t.Errorf("structuredParams.company: got %v, want aapl_us", sp["company"])
	}
	if sp["dateFrom"] != "2024-01-01" {
		t.Errorf("structuredParams.dateFrom: got %v, want 2024-01-01", sp["dateFrom"])
	}
}

func TestChatDryRunOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should not be called in dry-run mode
		t.Error("unexpected HTTP call in --dry-run mode")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	stdout, _, err := executeChat(t, srv.URL, "chat",
		"What is AAPL revenue?",
		"--company", "aapl_us",
		"--dry-run",
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "endpoint") {
		t.Errorf("expected endpoint in dry-run output, got: %s", stdout)
	}
	if !strings.Contains(stdout, "/chat") {
		t.Errorf("expected /chat in dry-run output, got: %s", stdout)
	}
}

func TestChatDryRunStreamInvalid(t *testing.T) {
	_, stderr, err := executeChat(t, "http://localhost:9", "chat",
		"test question",
		"--dry-run",
		"--stream",
	)

	if err == nil {
		t.Fatal("expected error for --dry-run --stream combo")
	}
	if !strings.Contains(stderr, "cannot be combined") {
		t.Errorf("expected error message about cannot be combined, got: %s", stderr)
	}
}

func TestChatCompanyAutoResolution_ExactMatch(t *testing.T) {
	var resolveQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/companies/search":
			resolveQ = r.URL.Query().Get("q")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"issuer_key": "aapl_us", "company_name": "Apple Inc.", "exchange": "NASDAQ"},
			})
		case "/chat":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Answer"}))
			_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "c1"}))
			_, _ = fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	defer srv.Close()

	_, _, err := executeChat(t, srv.URL, "chat", "What is revenue?", "--company", "Apple")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolveQ != "Apple" {
		t.Errorf("expected companies/search?q=Apple, got q=%q", resolveQ)
	}
}

func TestChatCompanyAutoResolution_NoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/companies/search" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{})
		}
	}))
	defer srv.Close()

	_, stderr, err := executeChat(t, srv.URL, "chat", "What is revenue?", "--company", "NonExistent Corp")
	if err == nil {
		t.Fatal("expected error for no match")
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("expected not found message, got: %s", stderr)
	}
}

func TestChatCompanyAutoResolution_Ambiguous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/companies/search" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{
				{"issuer_key": "aapl_us", "company_name": "Apple Inc.", "exchange": "NASDAQ"},
				{"issuer_key": "appl_ca", "company_name": "Apple Canada", "exchange": "TSX"},
				{"issuer_key": "app_gb", "company_name": "Apple GB", "exchange": "LSE"},
			})
		}
	}))
	defer srv.Close()

	// In tests stdout is not a TTY, so auto-JSON kicks in. Ambiguous output goes to stdout as JSON.
	stdout, _, err := executeChat(t, srv.URL, "chat", "What is revenue?", "--company", "Apple")
	if err == nil {
		t.Fatal("expected error for ambiguous match")
	}
	// In non-TTY (auto-JSON) mode, ambiguous candidates go to stdout as JSON envelope
	if !strings.Contains(stdout, "AMBIGUOUS_COMPANY") {
		t.Errorf("expected AMBIGUOUS_COMPANY in stdout JSON, got stdout: %s", stdout)
	}
}

func TestChatCompanyIssuerKeyBypass(t *testing.T) {
	// Each literal form must short-circuit AutoResolve. Mirrors 37.5's table-verb
	// fix; this exercises the chat-verb call-site (chat.go:resolver.IsLiteralIssuerKey).
	literals := []string{
		"aapl_us",
		"shop_ca",
		"cik:320193",
		"uuid:3162c889-bb75-49cf-b605-3295ae6e092d",
	}
	for _, lit := range literals {
		t.Run(lit, func(t *testing.T) {
			companiesSearchCalled := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/companies/search":
					companiesSearchCalled = true
					w.WriteHeader(http.StatusInternalServerError)
				case "/chat":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Answer"}))
					_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "c1"}))
					_, _ = fmt.Fprint(w, "data: [DONE]\n")
				}
			}))
			defer srv.Close()

			_, _, err := executeChat(t, srv.URL, "chat", "What is revenue?", "--company", lit)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if companiesSearchCalled {
				t.Errorf("expected no /companies/search call for literal %q", lit)
			}
		})
	}
}

func TestChatStreamFlagSetsAcceptHeader(t *testing.T) {
	var acceptHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptHeader = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Streaming answer"}))
		_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "c-stream"}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer srv.Close()

	_, _, _ = executeChat(t, srv.URL, "chat", "test question", "--stream")

	if acceptHeader != "text/event-stream" {
		t.Errorf("Accept header: got %q, want text/event-stream", acceptHeader)
	}
}

func TestChatMarkdownOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "AAPL reported [cite:1] strong supply chain"}))
		_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "conv-abc"}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer srv.Close()

	stdout, stderr, err := executeChat(t, srv.URL, "chat", "Supply chain question?", "--format", "markdown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout, "AAPL reported") {
		t.Errorf("expected markdown in stdout, got: %s", stdout)
	}
	if !strings.Contains(stderr, "[watch:") {
		t.Errorf("expected watch URL in stderr, got: %s", stderr)
	}
	// Watch URL must NOT appear in stdout
	if strings.Contains(stdout, "[watch:") {
		t.Errorf("watch URL should be in stderr, not stdout. stdout: %s", stdout)
	}
}

func TestChatJSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "JSON answer here"}))
		_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "conv-json"}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer srv.Close()

	stdout, _, err := executeChat(t, srv.URL, "chat", "JSON question?", "--format", "json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nstdout: %s", err, stdout)
	}
	if result["markdown"] == nil {
		t.Error("expected markdown field in JSON output")
	}
	if result["conversation_id"] == nil {
		t.Error("expected conversation_id field in JSON output")
	}
	if result["watch_url"] == nil {
		t.Error("expected watch_url field in JSON output")
	}
	if result["applied_defaults"] == nil {
		t.Error("expected applied_defaults field in JSON output (even if empty)")
	}
}

func TestChatErrorAuth401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, stderr, err := executeChat(t, srv.URL, "chat", "test question")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(stderr, "authentication failed") {
		t.Errorf("expected auth error message, got: %s", stderr)
	}
}

func TestChatError402QuotaExceeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer srv.Close()

	_, stderr, err := executeChat(t, srv.URL, "chat", "test question")
	if err == nil {
		t.Fatal("expected error on 402")
	}
	if !strings.Contains(stderr, "quota exceeded") {
		t.Errorf("expected quota exceeded message, got: %s", stderr)
	}
}

func TestChatError5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, stderr, err := executeChat(t, srv.URL, "chat", "test question")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(stderr, "server error") {
		t.Errorf("expected server error message, got: %s", stderr)
	}
}

func TestChatCompactStripsFooter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Compact answer"}))
		_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "conv-compact"}))
		_, _ = fmt.Fprint(w, "data: [DONE]\n")
	}))
	defer srv.Close()

	stdout, stderr, err := executeChat(t, srv.URL, "chat", "test?", "--compact", "--format", "markdown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(stdout, "---citations---") {
		t.Error("citations should be omitted in --compact mode")
	}
	if strings.Contains(stderr, "[watch:") {
		t.Error("watch URL should be omitted in --compact mode")
	}
}

func TestChatFilingTypeCSV(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat" {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			receivedBody = buf.Bytes()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Answer"}))
			_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "c1"}))
			_, _ = fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	defer srv.Close()

	_, _, _ = executeChat(t, srv.URL, "chat", "Question?", "--filing-type", "10-K,10-Q")

	var body map[string]interface{}
	if err := json.Unmarshal(receivedBody, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	sp, ok := body["structuredParams"].(map[string]interface{})
	if !ok {
		t.Fatal("missing structuredParams")
	}
	ft, ok := sp["filingType"].([]interface{})
	if !ok {
		t.Fatalf("filingType is not array: %T %v", sp["filingType"], sp["filingType"])
	}
	if len(ft) != 2 {
		t.Errorf("expected 2 filing types, got %d: %v", len(ft), ft)
	}
}

func TestChatStructuredParamsOmittedWhenEmpty(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat" {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			receivedBody = buf.Bytes()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Answer"}))
			_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "c1"}))
			_, _ = fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	defer srv.Close()

	_, _, _ = executeChat(t, srv.URL, "chat", "Question without filters?")

	var body map[string]interface{}
	if err := json.Unmarshal(receivedBody, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if _, exists := body["structuredParams"]; exists {
		t.Error("structuredParams should be omitted when no filter flags are set")
	}
}

func TestChatConversationIDIncluded(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat" {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			receivedBody = buf.Bytes()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "Follow-up answer"}))
			_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "resume-id"}))
			_, _ = fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	defer srv.Close()

	_, _, _ = executeChat(t, srv.URL, "chat", "Follow-up question?", "--conversation", "resume-id")

	var body map[string]interface{}
	if err := json.Unmarshal(receivedBody, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["conversationId"] != "resume-id" {
		t.Errorf("expected conversationId=resume-id, got %v", body["conversationId"])
	}
}

func TestChatFlagValidationNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Sandbox HOME: the credentials file rung (41.3) would otherwise resolve
	// a real ~/.archivist/credentials and break the no-token expectation.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_TOKEN", "")
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)

	root := cmd.NewRootCmd("0.2.0", "abc1234", "2026-05-20")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"chat", "test question"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error when no token available")
	}
	if !strings.Contains(stderr.String(), "no credentials found") {
		t.Errorf("expected no credentials message, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--token") {
		t.Errorf("guidance should mention 'auth login --token' persistence, got: %s", stderr.String())
	}
}

func TestChatJSONErrorOnAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	stdout, _, err := executeChat(t, srv.URL, "chat", "test?", "--format", "json")
	if err == nil {
		t.Fatal("expected error on 401")
	}

	var errEnv map[string]interface{}
	if jsonErr := json.Unmarshal([]byte(stdout), &errEnv); jsonErr != nil {
		t.Fatalf("expected JSON error envelope in stdout, got: %s (parse error: %v)", stdout, jsonErr)
	}
	if errEnv["error"] == nil {
		t.Error("expected error field in JSON error envelope")
	}
}

// ─── Story 41.8 — --model cheap-suite restriction ───────────────────────────

// A cheap-suite model passes the client guard and is sent to the server.
func TestChatModelAllowedCheapSuite(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat" {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			receivedBody = buf.Bytes()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "ok"}))
			_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "c1"}))
			_, _ = fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	defer srv.Close()

	_, _, err := executeChat(t, srv.URL, "chat", "What is revenue?", "--model", "gemini-3.1-flash-lite")
	if err != nil {
		t.Fatalf("unexpected error for an allowed model: %v", err)
	}

	var body map[string]interface{}
	if jsonErr := json.Unmarshal(receivedBody, &body); jsonErr != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", jsonErr, string(receivedBody))
	}
	if body["model"] != "gemini-3.1-flash-lite" {
		t.Errorf("model: got %v, want gemini-3.1-flash-lite", body["model"])
	}
}

// The premium model is web-chat-only and rejected client-side with usage exit 2.
func TestChatModelRejectsPremium(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call — premium model must be rejected client-side")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, stderr, err := executeChat(t, srv.URL, "chat", "q?", "--model", "gemini-3.1-pro-preview")
	if err == nil {
		t.Fatal("expected error for premium model on the CLI")
	}
	var exitErr *cmd.ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != cmd.ExitUsageError {
		t.Errorf("expected ExitUsageError (2), got %v", err)
	}
	if !strings.Contains(stderr, "not available on the CLI") {
		t.Errorf("expected rejection message, got: %s", stderr)
	}
	// The allowed list must NOT advertise the premium model.
	idx := strings.Index(stderr, "Choose one of:")
	if idx == -1 {
		t.Fatalf("expected allowed-model list in stderr, got: %s", stderr)
	}
	if strings.Contains(stderr[idx:], "pro-preview") {
		t.Errorf("allowed list must not include the premium model, got: %s", stderr)
	}
	if !strings.Contains(stderr[idx:], "gemini-3.1-flash-lite") {
		t.Errorf("allowed list should name the cheap suite, got: %s", stderr)
	}
}

// An unknown / removed model id is rejected client-side with usage exit 2.
func TestChatModelRejectsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected HTTP call — unknown model must be rejected client-side")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// "opus" (Claude, removed), "gemini-2.5-flash-lite" (dropped 2026-06-23 —
	// Gemini 2.5 rejects thinkingLevel), and "gemini-2.5-flash" (dropped in
	// mosaic Story 52.5 — web-search engine repointed to gemini-3-flash before
	// the 2026-10-16 Gemini 2.5 retirement) are all rejected client-side now.
	for _, removed := range []string{"opus", "gemini-2.5-flash-lite", "gemini-2.5-flash"} {
		_, stderr, err := executeChat(t, srv.URL, "chat", "q?", "--model", removed)
		if err == nil {
			t.Fatalf("expected error for removed model %q on the CLI", removed)
		}
		var exitErr *cmd.ExitError
		if !errors.As(err, &exitErr) || exitErr.Code != cmd.ExitUsageError {
			t.Errorf("%s: expected ExitUsageError (2), got %v", removed, err)
		}
		if !strings.Contains(stderr, "not available on the CLI") {
			t.Errorf("%s: expected rejection message, got: %s", removed, stderr)
		}
	}
}

// With no --model flag, the request omits the model field so the server applies
// the user's saved preference / default.
func TestChatNoModelOmitsModelField(t *testing.T) {
	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat" {
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			receivedBody = buf.Bytes()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, sseData(map[string]string{"type": "text-delta", "delta": "ok"}))
			_, _ = fmt.Fprint(w, sseData(map[string]interface{}{"type": "finish", "conversationId": "c1"}))
			_, _ = fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	defer srv.Close()

	_, _, err := executeChat(t, srv.URL, "chat", "What is revenue?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var body map[string]interface{}
	if jsonErr := json.Unmarshal(receivedBody, &body); jsonErr != nil {
		t.Fatalf("body is not valid JSON: %v\nbody: %s", jsonErr, string(receivedBody))
	}
	if _, ok := body["model"]; ok {
		t.Errorf("model field should be omitted when --model is not set, got: %v", body["model"])
	}
}

// --- mosaic Story 52.5: non-2xx JSON error bodies must surface, not vanish ---
//
// Pre-52.5, any 4xx the status switch did not enumerate (400 MODEL_NOT_ALLOWED
// from the authoritative server allowlist, 404, 429 from the CLI rate limiter)
// fell through to the SSE consumer, which found no "data:" frames in a JSON
// error body and returned an empty result with exit 0 — the error vanished.

func TestChatSurfacesServer4xxErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     map[string]interface{}
		wantExit int
		wantText string
	}{
		{
			name:   "400 MODEL_NOT_ALLOWED surfaces with usage exit 2",
			status: http.StatusBadRequest,
			body: map[string]interface{}{
				"error":      `Model "gemini-2.5-flash" is not available on this surface.`,
				"code":       "MODEL_NOT_ALLOWED",
				"suggestion": "Choose one of: gemini-3.1-flash-lite, gemini-3-flash.",
			},
			wantExit: cmd.ExitUsageError,
			wantText: "MODEL_NOT_ALLOWED",
		},
		{
			name:     "404 surfaces with not-found exit 3",
			status:   http.StatusNotFound,
			body:     map[string]interface{}{"error": "Conversation not found.", "code": "CONVERSATION_NOT_FOUND"},
			wantExit: cmd.ExitNotFound,
			wantText: "Conversation not found.",
		},
		{
			name:     "429 surfaces with rate-limit exit 7",
			status:   http.StatusTooManyRequests,
			body:     map[string]interface{}{"error": "Rate limit exceeded.", "code": "RATE_LIMITED"},
			wantExit: cmd.ExitRateLimit,
			wantText: "Rate limit exceeded.",
		},
		{
			name:     "unmapped 4xx surfaces with generic exit 1",
			status:   http.StatusTeapot,
			body:     map[string]interface{}{"error": "Teapot.", "code": "TEAPOT"},
			wantExit: cmd.ExitGenericError,
			wantText: "Teapot.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			defer srv.Close()

			stdout, stderr, err := executeChat(t, srv.URL, "chat", "any question")
			if err == nil {
				t.Fatalf("expected error for HTTP %d, got exit 0 (stdout: %s)", tc.status, stdout)
			}
			var exitErr *cmd.ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != tc.wantExit {
				t.Errorf("expected exit %d, got %v", tc.wantExit, err)
			}
			if !strings.Contains(stderr, tc.wantText) {
				t.Errorf("stderr must carry the server error, got: %s", stderr)
			}
		})
	}
}

// Suggestion text from the standardized error envelope rides along on stderr.
func TestChatSurfacesServerSuggestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error":      "Bad model.",
			"code":       "MODEL_NOT_ALLOWED",
			"suggestion": "Choose one of: gemini-3.1-flash-lite, gemini-3-flash.",
		})
	}))
	defer srv.Close()

	_, stderr, err := executeChat(t, srv.URL, "chat", "q?")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr, "Choose one of: gemini-3.1-flash-lite, gemini-3-flash.") {
		t.Errorf("expected suggestion on stderr, got: %s", stderr)
	}
}

// --format json emits the structured error object on stdout as well.
func TestChatSurfacesServer4xxJSONFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": "Bad model.",
			"code":  "MODEL_NOT_ALLOWED",
		})
	}))
	defer srv.Close()

	stdout, _, err := executeChat(t, srv.URL, "chat", "q?", "--format", "json")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stdout, "MODEL_NOT_ALLOWED") {
		t.Errorf("expected structured JSON error on stdout, got: %s", stdout)
	}
}
