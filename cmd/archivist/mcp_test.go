package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/spf13/cobra"
)

// expectedToolNames is the exact tools/list contract (Story 39.7 AC2). Any
// drift — a new verb, a hidden verb leaking, a rename — must fail loudly.
var expectedToolNames = []string{
	"auth_status",
	"auth_whoami",
	"companies_get",
	"companies_search",
	"doctor",
	"read_passage",
	"read_section",
	"search",
	"toc",
	"usage",
	"version",
}

// buildRootForTest mirrors main() setup for use in tests.
func buildRootForTest(version string) *cobra.Command {
	return cmd.NewRootCmd(version, "test", "test")
}

func collectToolsForTest(t *testing.T) []toolSpec {
	t.Helper()
	return collectTools(buildRootForTest("dev"))
}

// TestCollectTools_ExactSet asserts the 11-tool set as an exact match (AC2),
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

// TestCollectTools_TitlesAndReadOnly asserts every tool carries a non-empty
// title from mcp:title and ReadOnly from mcp:read-only "true".
func TestCollectTools_TitlesAndReadOnly(t *testing.T) {
	for _, s := range collectToolsForTest(t) {
		if strings.TrimSpace(s.Title) == "" {
			t.Errorf("tool %s: empty title", s.Name)
		}
		if !s.ReadOnly {
			t.Errorf("tool %s: want ReadOnly=true", s.Name)
		}
	}
}

// TestCollectTools_SearchSchema asserts search carries a required query
// positional and its filter flags, with limit as an integer.
func TestCollectTools_SearchSchema(t *testing.T) {
	s := findSpec(t, "search")
	if len(s.Positionals) != 1 || s.Positionals[0] != "query" {
		t.Fatalf("search positionals: want [query], got %v", s.Positionals)
	}
	for _, want := range []string{"symbol", "formtype", "date_from", "date_to", "mode", "limit", "cursor", "format", "dry_run"} {
		if _, ok := s.Schema.Properties[want]; !ok {
			t.Errorf("search schema: property %q missing; props: %v", want, propNames(s))
		}
	}
	if s.Schema.Properties["limit"].Type != "integer" {
		t.Errorf("search limit: want integer, got %q", s.Schema.Properties["limit"].Type)
	}
	if s.FlagFor["date_from"] != "date-from" {
		t.Errorf("date_from must map to --date-from, got %q", s.FlagFor["date_from"])
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
		"search":           {"query"},
		"read_passage":     {"chunk_id"},
		"read_section":     {"filing_id", "section_header"},
		"toc":              {"filing_id"},
		"companies_search": {"query"},
		"companies_get":    {"issuer_key"},
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
// root_test.go walks NewRootCmd only and cannot see mcp or mcp serve (T4.6). Also asserts mcp + serve are annotated mcp:hidden so the walker
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

// TestMCPServer_ToolsList covers AC2 + AC3 over the wire: exact 11-tool set,
// hidden verbs absent, title and all three hints surface on every tool.
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
	for _, tool := range res.Tools {
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("tool %s: empty description over the wire", tool.Name)
		}
		assertToolTitleAndHints(t, tool)
	}
	if got["search"].Title != "Search filings" {
		t.Errorf("search title: got %q", got["search"].Title)
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

// TestMCPServer_SearchRoundTrip: the search tool result text equals what the
// CLI prints for the same invocation when piped (the server body
// re-indented), byte for byte, and carries the permalink.
func TestMCPServer_SearchRoundTrip(t *testing.T) {
	const permalink = "https://mosaic-finance.com/filings/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/?c=11111111-2222-4333-8444-555555555555&t=k1.mac"
	body := `{"results":[{"id":"11111111-2222-4333-8444-555555555555","filing_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",` +
		`"company_name":"Apple Inc.","symbol":"AAPL:US","exchange":"NGS","formtype":"10-K","formdescription":"Annual Report",` +
		`"datefiled":"2025-11-01","section_header":"Risk Factors","chunk_index":4,"snippet":"Supply chain risk.",` +
		`"url":"` + permalink + `","exchange_document_id":"0000320193-25-000079","exchange_document_kind":"sec_accession_number"}],` +
		`"entity_resolution":null,"truncated":false,"next_cursor":null}`
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/research/search" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cs := newMCPSession(t, srv.URL)
	toolText, isError := callToolText(t, cs, "search", map[string]any{
		"query":     "supply chain",
		"symbol":    "AAPL:US",
		"date_from": "2024-01-01",
		"limit":     5,
	})
	if isError {
		t.Fatalf("search returned IsError, text:\n%s", toolText)
	}
	for _, want := range []string{"q=supply+chain", "symbol=AAPL%3AUS", "date_from=2024-01-01", "limit=5"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("request query %q missing %q", gotQuery, want)
		}
	}

	root := buildRootForTest("dev")
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"search", "supply chain", "--symbol=AAPL:US", "--date-from=2024-01-01", "--limit=5"})
	if err := root.Execute(); err != nil {
		t.Fatalf("CLI invocation: %v\nstderr: %s", err, stderr.String())
	}
	if toolText != stdout.String() {
		t.Errorf("round-trip parity broken\nMCP tool text:\n%s\nCLI stdout:\n%s", toolText, stdout.String())
	}
	if !strings.Contains(toolText, permalink) {
		t.Errorf("tool text lost the permalink:\n%s", toolText)
	}
	if !strings.Contains(toolText, `"exchange_document_id": "0000320193-25-000079"`) ||
		strings.Contains(toolText, "source_url") {
		t.Errorf("tool text must carry the id keys unchanged and no source_url:\n%s", toolText)
	}
}

// TestMCPServer_InstructionsCiteOnlyURL: the initialize instructions name the
// permalink url as the only link to cite and the exchange document id as an
// identifier, never a link (78-drop-source-urls).
func TestMCPServer_InstructionsCiteOnlyURL(t *testing.T) {
	cs := newMCPSession(t, "")
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("no initialize result")
	}
	got := init.Instructions
	if got != mcpInstructions {
		t.Fatalf("initialize instructions differ from mcpInstructions:\n%s", got)
	}
	for _, want := range []string{
		"That url is the only link: cite only url",
		"exchange_document_id (with exchange_document_kind)",
		"an identifier to quote, not a link",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instructions missing %q:\n%s", want, got)
		}
	}
	for _, banned := range []string{"source_url", "sec.gov", "quotemedia", "sedarplus", "kap.org.tr"} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("instructions mention %q:\n%s", banned, got)
		}
	}
}

// TestMCPServer_DashPositionalIsNotAFlag: positionals follow a "--"
// terminator, so a query that starts with "-" reaches the verb as the query
// instead of being parsed as a flag.
func TestMCPServer_DashPositionalIsNotAFlag(t *testing.T) {
	var gotQ []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQ = append(gotQ, r.URL.Query().Get("q"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"c1"}],"entity_resolution":null,"truncated":false,"next_cursor":null}`))
	}))
	t.Cleanup(srv.Close)

	cs := newMCPSession(t, srv.URL)
	for _, q := range []string{"-10% revenue", "--stdin"} {
		text, isError := callToolText(t, cs, "search", map[string]any{"query": q, "limit": 3})
		if isError {
			t.Errorf("query %q: IsError:\n%s", q, text)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(gotQ, "|") != "-10% revenue|--stdin" {
		t.Errorf("queries reaching the server: %q", gotQ)
	}

	argv, usageErr := buildArgv(findSpec(t, "search"), json.RawMessage(`{"query":"-x","limit":3}`), "ak_tok")
	if usageErr != "" {
		t.Fatal(usageErr)
	}
	if got := strings.Join(argv, " "); got != "search --limit=3 --token ak_tok -- -x" {
		t.Errorf("argv: %q", got)
	}
}

// TestMCPServer_FairUseSurfacesExitCode7: a CLI_QUOTA refusal is an IsError
// result naming exit code 7 with the JSON envelope in its stdout section.
func TestMCPServer_FairUseSurfacesExitCode7(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Queries-Remaining", "0")
		w.Header().Set("Retry-After", "2592000")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"Monthly fair use limit reached.","code":"CLI_QUOTA","reset_date":"2026-11-01"}`))
	}))
	t.Cleanup(srv.Close)

	cs := newMCPSession(t, srv.URL)
	text, isError := callToolText(t, cs, "search", map[string]any{"query": "x"})
	if !isError {
		t.Fatalf("want IsError, got success:\n%s", text)
	}
	for _, want := range []string{"exit code 7 (rate limit)", "--- stdout ---", `"CLI_QUOTA"`, "2026-11-01"} {
		if !strings.Contains(text, want) {
			t.Errorf("error text missing %q:\n%s", want, text)
		}
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

// buildTestBinary builds the real binary once per test run; TestMain removes
// its temp directory afterwards.
var (
	testBinOnce sync.Once
	testBinDir  string
	testBinPath string
	testBinErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if testBinDir != "" {
		_ = os.RemoveAll(testBinDir)
	}
	os.Exit(code)
}

func buildTestBinary(t *testing.T) string {
	t.Helper()
	testBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "archivist-test-bin-")
		if err != nil {
			testBinErr = err
			return
		}
		testBinDir = dir
		testBinPath = filepath.Join(dir, "archivist-test-bin")
		build := exec.Command("go", "build", "-o", testBinPath, ".")
		build.Dir = "."
		if out, err := build.CombinedOutput(); err != nil {
			testBinErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if testBinErr != nil {
		t.Fatal(testBinErr)
	}
	return testBinPath
}

// TestMCPServe_StdioSubprocess covers AC9 + AC10: build the real binary,
// spawn it via mcp.CommandTransport, and complete initialize, tools/list and
// a tools/call search over actual stdio. Completing the handshake proves
// nothing but the SDK transport writes to process stdout (a stray print would
// corrupt JSON-RPC framing).
func TestMCPServe_StdioSubprocess(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build in -short mode")
	}
	bin := buildTestBinary(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"c1","url":"https://mosaic-finance.com/filings/f1/?c=c1&t=k.m"}],"entity_resolution":null,"truncated":false,"next_cursor":null}`))
	}))
	t.Cleanup(srv.Close)

	cmdServe := exec.Command(bin, "mcp", "serve")
	cmdServe.Env = append(os.Environ(), "ARCHIVIST_TOKEN=mc_pat_testtoken", "ARCHIVIST_BASE_URL="+srv.URL, "HOME="+t.TempDir())

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
		assertToolTitleAndHints(t, tool)
	}
	sort.Strings(names)
	want := append([]string{}, expectedToolNames...)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("stdio tools/list mismatch\nwant: %v\ngot:  %v", want, names)
	}

	text, isError := callToolText(t, cs, "search", map[string]any{"query": "risk"})
	if isError || !strings.Contains(text, "https://mosaic-finance.com/filings/f1/?c=c1") {
		t.Errorf("tools/call search over stdio: isError=%v text:\n%s", isError, text)
	}
}

// TestMCPServe_StdioRawToolsListJSON reads the raw tools/list response off
// the binary's stdout and asserts the serialized shape: every tool has a
// title and annotations {title, readOnlyHint: true, destructiveHint: false,
// openWorldHint: false} with each hint key present.
func TestMCPServe_StdioRawToolsListJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build in -short mode")
	}
	bin := buildTestBinary(t)

	proc := exec.Command(bin, "mcp", "serve")
	proc.Env = append(os.Environ(), "ARCHIVIST_TOKEN=mc_pat_testtoken", "HOME="+t.TempDir())
	stdin, err := proc.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := proc.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = proc.Wait()
	})

	msgs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}
	for _, m := range msgs {
		if _, err := io.WriteString(stdin, m+"\n"); err != nil {
			t.Fatal(err)
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<22)
	var raw json.RawMessage
	for scanner.Scan() {
		var frame struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) == nil && frame.ID == 2 {
			raw = frame.Result
			break
		}
	}
	if raw == nil {
		t.Fatalf("no tools/list response (scan err: %v)", scanner.Err())
	}

	var result struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(result.Tools) != len(expectedToolNames) {
		t.Errorf("tool count: got %d, want %d", len(result.Tools), len(expectedToolNames))
	}
	for _, tool := range result.Tools {
		name := string(tool["name"])
		var title string
		if err := json.Unmarshal(tool["title"], &title); err != nil || title == "" {
			t.Errorf("%s: missing title in raw JSON", name)
		}
		var ann map[string]json.RawMessage
		if err := json.Unmarshal(tool["annotations"], &ann); err != nil {
			t.Errorf("%s: annotations: %v", name, err)
			continue
		}
		wantRaw := map[string]string{"readOnlyHint": "true", "destructiveHint": "false", "openWorldHint": "false"}
		for k, v := range wantRaw {
			if string(ann[k]) != v {
				t.Errorf("%s: annotations.%s = %q, want %s", name, k, ann[k], v)
			}
		}
		if string(ann["title"]) != string(tool["title"]) {
			t.Errorf("%s: annotations.title %s != title %s", name, ann["title"], tool["title"])
		}
	}
}

// mockMCPCompanyResult mirrors one /research/companies result
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
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results, "truncated": false, "next_cursor": nil})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertToolTitleAndHints checks a listed tool's title and the read-only
// annotation set: title, readOnlyHint true, destructiveHint false,
// openWorldHint false, all present on the wire.
func assertToolTitleAndHints(t *testing.T, tool *mcp.Tool) {
	t.Helper()
	if strings.TrimSpace(tool.Title) == "" {
		t.Errorf("tool %s: empty title", tool.Name)
	}
	a := tool.Annotations
	if a == nil {
		t.Errorf("tool %s: no annotations", tool.Name)
		return
	}
	if a.Title != tool.Title {
		t.Errorf("tool %s: annotations.title %q != title %q", tool.Name, a.Title, tool.Title)
	}
	if !a.ReadOnlyHint {
		t.Errorf("tool %s: readOnlyHint must be true", tool.Name)
	}
	if a.DestructiveHint == nil || *a.DestructiveHint {
		t.Errorf("tool %s: destructiveHint must be present and false", tool.Name)
	}
	if a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Errorf("tool %s: openWorldHint must be present and false", tool.Name)
	}
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

func propNames(s toolSpec) []string {
	var names []string
	for k := range s.Schema.Properties {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
