package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// expectedToolNames is the exact tools/list contract (Story 39.7 AC2). Any
// drift — a new verb, a hidden verb leaking, a rename — must fail loudly.
var expectedToolNames = []string{
	"auth_status",
	"auth_whoami",
	"chat",
	"companies_get",
	"companies_search",
	"doctor",
	"explain_cascade",
	"explain_defaults",
	"table",
	"table_list",
	"table_rerun",
	"table_run",
	"table_watch",
	"usage",
	"version",
}

func collectToolsForTest(t *testing.T) []toolSpec {
	t.Helper()
	return collectTools(buildRootForTest("dev"))
}

// TestCollectTools_ExactSet asserts the 15-tool set as an exact match (AC2),
// which doubles as the hidden-verb assertion (AC3): auth_login, auth_logout,
// update, and any mcp* self-entry would break set equality.
func TestCollectTools_ExactSet(t *testing.T) {
	specs := collectToolsForTest(t)
	var got []string
	for _, s := range specs {
		got = append(got, s.Name)
	}
	sort.Strings(got)
	want := append([]string{}, expectedToolNames...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("tool count: want %d, got %d\nwant: %v\ngot:  %v", len(want), len(got), want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool set mismatch at %d: want %q, got %q\nwant: %v\ngot:  %v",
				i, want[i], got[i], want, got)
		}
	}
}

// TestCollectTools_NamingContract asserts every tool name matches Claude
// Desktop's ^[a-zA-Z0-9_-]{1,64}$ validation (AC4). Dotted names are rejected
// by Claude Desktop even though MCP SEP-986 permits them.
func TestCollectTools_NamingContract(t *testing.T) {
	re := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	for _, s := range collectToolsForTest(t) {
		if !re.MatchString(s.Name) {
			t.Errorf("tool name %q fails Claude Desktop validation %s", s.Name, re)
		}
	}
}

// TestCollectTools_DenylistAbsent asserts token/stdin/stream/quiet/no-color
// appear in NO tool schema (AC8), under both flag and property spelling.
func TestCollectTools_DenylistAbsent(t *testing.T) {
	denied := []string{"token", "stdin", "stream", "quiet", "no-color", "no_color"}
	for _, s := range collectToolsForTest(t) {
		for prop := range s.Schema.Properties {
			for _, d := range denied {
				if prop == d {
					t.Errorf("tool %s: denylisted property %q present in schema", s.Name, prop)
				}
			}
		}
	}
}

// TestCollectTools_ChatSchema asserts chat carries a required positional
// question plus the flag-derived properties, with attach_filing as
// array-of-string (AC2).
func TestCollectTools_ChatSchema(t *testing.T) {
	s := findSpec(t, "chat")
	if len(s.Positionals) != 1 || s.Positionals[0] != "question" {
		t.Fatalf("chat positionals: want [question], got %v", s.Positionals)
	}
	if !contains(s.Schema.Required, "question") {
		t.Errorf("chat schema: question not required: %v", s.Schema.Required)
	}
	q, ok := s.Schema.Properties["question"]
	if !ok || q.Type != "string" {
		t.Errorf("chat schema: question must be a string property, got %+v", q)
	}
	af, ok := s.Schema.Properties["attach_filing"]
	if !ok {
		t.Fatalf("chat schema: attach_filing missing; props: %v", propNames(s))
	}
	if af.Type != "array" || af.Items == nil || af.Items.Type != "string" {
		t.Errorf("chat schema: attach_filing must be array-of-string, got %+v", af)
	}
	for _, want := range []string{"company", "filing_type", "date_from", "date_to", "model", "conversation", "format", "compact", "dry_run"} {
		if _, ok := s.Schema.Properties[want]; !ok {
			t.Errorf("chat schema: property %q missing; props: %v", want, propNames(s))
		}
	}
}

// TestCollectTools_TableRunSpecYAML asserts the table_run positional is
// replaced by a required spec_yaml string wired to stdin dispatch (AC7).
func TestCollectTools_TableRunSpecYAML(t *testing.T) {
	s := findSpec(t, "table_run")
	if s.StdinProp != "spec_yaml" {
		t.Errorf("table_run StdinProp: want spec_yaml, got %q", s.StdinProp)
	}
	if len(s.Positionals) != 0 {
		t.Errorf("table_run positionals must be empty (spec_yaml replaces them), got %v", s.Positionals)
	}
	if !contains(s.Schema.Required, "spec_yaml") {
		t.Errorf("table_run schema: spec_yaml not required: %v", s.Schema.Required)
	}
	sy, ok := s.Schema.Properties["spec_yaml"]
	if !ok || sy.Type != "string" {
		t.Errorf("table_run schema: spec_yaml must be a string property, got %+v", sy)
	}
	for _, want := range []string{"async", "watch", "format", "compact", "dry_run"} {
		if _, ok := s.Schema.Properties[want]; !ok {
			t.Errorf("table_run schema: property %q missing; props: %v", want, propNames(s))
		}
	}
}

// TestCollectTools_ReadOnlyHint asserts mcp:read-only "true" maps to
// ReadOnly=true and the literal "false" string does NOT read as true (AC2).
func TestCollectTools_ReadOnlyHint(t *testing.T) {
	if s := findSpec(t, "companies_search"); !s.ReadOnly {
		t.Error("companies_search: want ReadOnly=true")
	}
	if s := findSpec(t, "table"); s.ReadOnly {
		t.Error("table: annotation is the literal string \"false\" — must not read as true")
	}
	if s := findSpec(t, "table_run"); s.ReadOnly {
		t.Error("table_run: want ReadOnly=false")
	}
}

// TestCollectTools_SchemaMarshalsAdditionalPropertiesFalse asserts the wire
// shape carries additionalProperties: false (AC2) and typed properties.
func TestCollectTools_SchemaMarshalsAdditionalPropertiesFalse(t *testing.T) {
	s := findSpec(t, "companies_search")
	raw, err := json.Marshal(s.Schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	if ap, ok := m["additionalProperties"]; !ok || ap != false {
		t.Errorf("additionalProperties: want false, got %v (schema: %s)", ap, raw)
	}
	props, _ := m["properties"].(map[string]interface{})
	limit, _ := props["limit"].(map[string]interface{})
	if limit["type"] != "integer" {
		t.Errorf("companies_search limit: want integer (pflag int), got %v", limit["type"])
	}
}

// TestCollectTools_Descriptions asserts every tool description is non-empty
// and carries the typed exit codes line (AC2, T3.4).
func TestCollectTools_Descriptions(t *testing.T) {
	for _, s := range collectToolsForTest(t) {
		if strings.TrimSpace(s.Description) == "" {
			t.Errorf("tool %s: empty description", s.Name)
		}
		if !strings.Contains(s.Description, "Exit codes: ") {
			t.Errorf("tool %s: description missing 'Exit codes: ' line", s.Name)
		}
	}
}

// TestCollectTools_PositionalSanitization covers the Use-string placeholder
// parse for every positional-bearing verb (T3.3).
func TestCollectTools_PositionalSanitization(t *testing.T) {
	cases := map[string][]string{
		"table_rerun":      {"session_id"},
		"table_watch":      {"session_id"},
		"companies_search": {"query"},
		"companies_get":    {"issuer_key"},
		"chat":             {"question"},
	}
	for name, want := range cases {
		s := findSpec(t, name)
		if len(s.Positionals) != len(want) {
			t.Errorf("%s positionals: want %v, got %v", name, want, s.Positionals)
			continue
		}
		for i := range want {
			if s.Positionals[i] != want[i] {
				t.Errorf("%s positionals[%d]: want %q, got %q", name, i, want[i], s.Positionals[i])
			}
		}
	}
}

// TestPackageMainCommandsHaveAnnotations mirrors internal/cmd's
// TestAllCommandsHaveAnnotations for commands registered in package main —
// root_test.go walks NewRootCmd only and cannot see table, mcp, or mcp serve
// (T4.6). Also asserts mcp + serve are annotated mcp:hidden so the walker
// never maps a self-entry (AC3).
func TestPackageMainCommandsHaveAnnotations(t *testing.T) {
	newRoot := func() *cobra.Command { return buildRootForTest("dev") }
	root := newRoot()
	mcpCmd := newMCPCmd(newRoot, "dev")
	root.AddCommand(mcpCmd)

	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Name() == "help" || c.Name() == "completion" {
			return
		}
		if _, ok := c.Annotations["pp:typed-exit-codes"]; !ok {
			t.Errorf("command %q missing pp:typed-exit-codes annotation", c.CommandPath())
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(root)

	if mcpCmd.Annotations["mcp:hidden"] != "true" {
		t.Error("mcp command must be annotated mcp:hidden")
	}
	for _, child := range mcpCmd.Commands() {
		if child.Annotations["mcp:hidden"] != "true" {
			t.Errorf("mcp subcommand %q must be annotated mcp:hidden", child.Name())
		}
	}
}

// TestCollectTools_SkipsMCPSubtree proves the walker maps no mcp* self-entry
// even when collectTools runs over a root that HAS mcp registered (AC3
// belt-and-braces — production dispatch roots exclude mcp by construction).
func TestCollectTools_SkipsMCPSubtree(t *testing.T) {
	newRoot := func() *cobra.Command { return buildRootForTest("dev") }
	root := newRoot()
	root.AddCommand(newMCPCmd(newRoot, "dev"))
	for _, s := range collectTools(root) {
		if strings.HasPrefix(s.Name, "mcp") {
			t.Errorf("walker mapped MCP self-entry %q", s.Name)
		}
	}
}

// ─── T5: in-memory client↔server integration ─────────────────────────────────

// newMCPSession spins up the real MCP server over in-memory transports and
// returns a connected client session (T5.1 harness). Server connects FIRST —
// SDK contract: the client initializes the session during connection.
func newMCPSession(t *testing.T, baseURL string) *mcp.ClientSession {
	t.Helper()
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_testtoken")
	if baseURL != "" {
		t.Setenv("ARCHIVIST_BASE_URL", baseURL)
	}
	newRoot := func() *cobra.Command { return buildRootForTest("dev") }
	server, _ := buildMCPServer(newRoot, "dev", "")

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callToolText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, res.IsError
}

// TestMCPServer_ToolsList covers AC2 + AC3 over the wire: exact 15-tool set,
// hidden verbs absent, ReadOnlyHint surfaces correctly.
func TestMCPServer_ToolsList(t *testing.T) {
	cs := newMCPSession(t, "")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	got := map[string]*mcp.Tool{}
	var names []string
	for _, tool := range res.Tools {
		got[tool.Name] = tool
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := append([]string{}, expectedToolNames...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("tools/list mismatch\nwant: %v\ngot:  %v", want, names)
	}
	for _, hidden := range []string{"auth_login", "auth_logout", "update", "mcp", "mcp_serve"} {
		if _, ok := got[hidden]; ok {
			t.Errorf("hidden verb %q leaked into tools/list", hidden)
		}
	}
	if tool := got["companies_search"]; tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Error("companies_search: want ReadOnlyHint=true over the wire")
	}
	if tool := got["table"]; tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
		t.Error("table: ReadOnlyHint must be false/absent over the wire")
	}
	for _, tool := range res.Tools {
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("tool %s: empty description over the wire", tool.Name)
		}
	}
}

// TestMCPServer_CompaniesSearchRoundTrip covers AC5: the tool result text
// equals the JSON envelope the CLI emits for the same invocation when piped
// (non-TTY auto-JSON), byte for byte.
func TestMCPServer_CompaniesSearchRoundTrip(t *testing.T) {
	results := []mockMCPCompanyResult{
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 4127, IssuerKey: strPtr("aapl_us")},
	}
	srv := serveMCPCompanies(t, results)

	cs := newMCPSession(t, srv.URL)
	toolText, isError := callToolText(t, cs, "companies_search", map[string]any{
		"query": "Apple",
		"limit": 7,
	})
	if isError {
		t.Fatalf("companies_search returned IsError, text:\n%s", toolText)
	}

	// Same invocation through the CLI path: buffered stdout is non-TTY, so
	// format auto-resolves to JSON exactly like the dispatch buffer does.
	root := buildRootForTest("dev")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"companies", "search", "Apple", "--limit=7"})
	if err := root.Execute(); err != nil {
		t.Fatalf("CLI invocation: %v\nstderr: %s", err, stderr.String())
	}

	if toolText != stdout.String() {
		t.Errorf("round-trip parity broken\nMCP tool text:\n%s\nCLI stdout:\n%s", toolText, stdout.String())
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(toolText), &parsed); err != nil {
		t.Fatalf("tool text is not the CLI JSON envelope: %v\n%s", err, toolText)
	}
	if len(parsed) != 1 || parsed[0]["issuer_key"] != "aapl_us" {
		t.Errorf("unexpected envelope content: %v", parsed)
	}
}

// TestMCPServer_ServerErrorSurfacesExitCode5 covers AC6's 5xx leg: backend
// 500 → IsError result whose text names exit code 5 (server error) and
// carries captured stderr.
func TestMCPServer_ServerErrorSurfacesExitCode5(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cs := newMCPSession(t, srv.URL)
	text, isError := callToolText(t, cs, "companies_search", map[string]any{"query": "Apple"})
	if !isError {
		t.Fatalf("want IsError=true for backend 500, got success:\n%s", text)
	}
	if !strings.Contains(text, "exit code 5 (server error)") {
		t.Errorf("error text missing 'exit code 5 (server error)':\n%s", text)
	}
	if !strings.Contains(text, "--- stderr ---") {
		t.Errorf("error text missing stderr section:\n%s", text)
	}
}

// TestMCPServer_AmbiguousCompanyPreservesEnvelope covers AC6's exit-6 leg:
// the AMBIGUOUS_COMPANY JSON the verb prints to stdout must survive into the
// error text's stdout section.
func TestMCPServer_AmbiguousCompanyPreservesEnvelope(t *testing.T) {
	results := []mockMCPCompanyResult{
		{CompanyName: "Apple Canada Ltd.", Symbol: "APL:TSX", Exchange: "TSX", FilingCount: 5, IssuerKey: strPtr("apple_ca")},
		{CompanyName: "Apple Inc.", Symbol: "AAPL:US", Exchange: "NGS", FilingCount: 8, IssuerKey: strPtr("aapl_us")},
	}
	srv := serveMCPCompanies(t, results)

	cs := newMCPSession(t, srv.URL)
	text, isError := callToolText(t, cs, "chat", map[string]any{
		"question": "What was revenue?",
		"company":  "Apple",
	})
	if !isError {
		t.Fatalf("want IsError=true for ambiguous company, got success:\n%s", text)
	}
	if !strings.Contains(text, "exit code 6 (ambiguous match)") {
		t.Errorf("error text missing 'exit code 6 (ambiguous match)':\n%s", text)
	}
	if !strings.Contains(text, "AMBIGUOUS_COMPANY") {
		t.Errorf("error text lost the AMBIGUOUS_COMPANY stdout JSON:\n%s", text)
	}
	if !strings.Contains(text, "--- stdout ---") {
		t.Errorf("error text missing stdout section:\n%s", text)
	}
}

// TestMCPServer_TableRunSpecYAMLDryRun covers AC7's dispatch leg: spec_yaml
// feeds the verb's stdin via SetIn + --stdin, dry_run echoes the wire payload
// — proving stdin injection + spec parse with zero network.
func TestMCPServer_TableRunSpecYAMLDryRun(t *testing.T) {
	const specYAML = `top_n: 3
rows:
  - company: aapl_us
    filing-type: 10-K
    date-from: 2025-08-01
columns:
  - name: Revenue
    source: filings
    mode: rrf
    query: total net sales revenue annual
`
	cs := newMCPSession(t, "")
	text, isError := callToolText(t, cs, "table_run", map[string]any{
		"spec_yaml": specYAML,
		"dry_run":   true,
	})
	if isError {
		t.Fatalf("table_run dry_run returned IsError:\n%s", text)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("dry-run payload is not JSON: %v\n%s", err, text)
	}
	if _, ok := payload["rows"]; !ok {
		t.Error("dry-run payload missing 'rows'")
	}
	if _, ok := payload["columns"]; !ok {
		t.Error("dry-run payload missing 'columns'")
	}
}

// TestMCPServer_MissingRequiredArgIsUsageError: untyped AddTool does not
// schema-validate, so dispatch must reject missing positionals itself.
func TestMCPServer_MissingRequiredArgIsUsageError(t *testing.T) {
	cs := newMCPSession(t, "")
	text, isError := callToolText(t, cs, "companies_search", map[string]any{})
	if !isError {
		t.Fatalf("want IsError for missing required arg, got success:\n%s", text)
	}
	if !strings.Contains(text, "exit code 2 (usage error)") {
		t.Errorf("error text missing 'exit code 2 (usage error)':\n%s", text)
	}
	if !strings.Contains(text, `missing required argument "query"`) {
		t.Errorf("error text missing argument diagnostic:\n%s", text)
	}
}

// ─── T6: stdio subprocess truth test ─────────────────────────────────────────

// TestMCPServe_StdioSubprocess covers AC9 + AC10: build the real binary,
// spawn it via mcp.CommandTransport, and complete initialize + tools/list
// over actual stdio. Completing the handshake proves nothing but the SDK
// transport writes to process stdout (a stray print would corrupt JSON-RPC
// framing). Network-free: tools/list only. No build tag; budget ~10s.
func TestMCPServe_StdioSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build in -short mode")
	}
	bin := filepath.Join(t.TempDir(), "archivist-test-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	cmdServe := exec.Command(bin, "mcp", "serve")
	cmdServe.Env = append(os.Environ(), "ARCHIVIST_TOKEN=mc_pat_testtoken")

	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-stdio-test", Version: "0.0.0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: cmdServe}, nil)
	if err != nil {
		t.Fatalf("initialize over stdio failed: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list over stdio: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	want := append([]string{}, expectedToolNames...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("stdio tools/list mismatch\nwant: %v\ngot:  %v", want, names)
	}
}

// mockMCPCompanyResult mirrors the /companies/search response shape
// (pattern: internal/cmd/companies_test.go).
type mockMCPCompanyResult struct {
	CompanyName    string  `json:"company_name"`
	Symbol         string  `json:"symbol"`
	Exchange       string  `json:"exchange"`
	FilingCount    int     `json:"filing_count"`
	EarliestFiling *string `json:"earliest_filing"`
	LatestFiling   *string `json:"latest_filing"`
	IssuerKey      *string `json:"issuer_key"`
}

func serveMCPCompanies(t *testing.T, results []mockMCPCompanyResult) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(results)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func strPtr(s string) *string { return &s }

func findSpec(t *testing.T, name string) toolSpec {
	t.Helper()
	for _, s := range collectToolsForTest(t) {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("tool %q not found", name)
	return toolSpec{}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func propNames(s toolSpec) []string {
	var names []string
	for k := range s.Schema.Properties {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
