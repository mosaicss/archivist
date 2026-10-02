package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mosaicss/archivist/internal/taskscope"
	"github.com/spf13/cobra"
)

// Obviously fake, low entropy task tokens in the strict mst_ wire shape.
var (
	testTaskToken  = "mst_00000000-0000-4000-8000-000000000001." + strings.Repeat("a", 43)
	testTaskToken2 = "mst_00000000-0000-4000-8000-000000000002." + strings.Repeat("b", 43)
)

// expectedTaskToolNames is the task mode tools/list contract: exactly the
// verbs whose every chat-api route is in the 78.15 task allowlist.
var expectedTaskToolNames = []string{"companies_search", "read_passage", "read_section", "search", "toc"}

func TestTaskModeToolSet(t *testing.T) {
	var got []string
	for _, s := range collectToolsForTest(t) {
		if taskscope.ToolAllowed(s.Name) {
			got = append(got, s.Name)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(expectedTaskToolNames, ",") {
		t.Fatalf("task tools = %v, want %v", got, expectedTaskToolNames)
	}
	if strings.Join(taskscope.Tools(), ",") != strings.Join(expectedTaskToolNames, ",") {
		t.Fatalf("taskscope.Tools() = %v", taskscope.Tools())
	}
	// companies_get falls back to GET /companies, which tasks cannot call.
	if taskscope.ToolAllowed("companies_get") {
		t.Fatal("companies_get must stay out of task mode")
	}
	if taskscope.ToolAllowed("version") || taskscope.ToolAllowed("unknown_tool") {
		t.Fatal("tools without known routes must fail closed")
	}
}

func TestTaskRouteScopeMirrorsChatAPI(t *testing.T) {
	cases := map[string]string{
		"GET /research/search":                   "search",
		"GET /research/companies":                "search",
		"GET /uploads/search":                    "search",
		"GET /research/passages/abc":             "read",
		"GET /research/filings/f1/toc":           "read",
		"GET /research/filings/f1/sections":      "read",
		"GET /uploads/u1/chunks/3":               "read",
		"GET /uploads/u1/chunks/x":               "",
		"GET /research/search/":                  "",
		"POST /research/search":                  "",
		"GET /companies":                         "",
		"GET /account/cli-tokens":                "",
		"POST /chat":                             "",
		"GET /research/filings/f1/sections/more": "",
	}
	for route, want := range cases {
		method, path, _ := strings.Cut(route, " ")
		if got := taskscope.RouteScope(method, path); got != want {
			t.Errorf("taskscope.RouteScope(%s) = %q, want %q", route, got, want)
		}
	}
}

// TestToolRoutesMatchDryRun keeps toolRoutes honest: each task tool's
// --dry-run request path must be the route recorded for it.
func TestToolRoutesMatchDryRun(t *testing.T) {
	cases := map[string][]string{
		"search":           {"search", "--dry-run", "revenue"},
		"companies_search": {"companies", "search", "--dry-run", "apple"},
		"read_passage":     {"read", "passage", "--dry-run", "11111111-2222-4333-8444-555555555555"},
		"read_section":     {"read", "section", "--dry-run", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "Risk Factors"},
		"toc":              {"toc", "--dry-run", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"},
	}
	for tool, argv := range cases {
		root := buildRootForTest("dev")
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(argv)
		if err := root.Execute(); err != nil {
			t.Fatalf("%s dry run: %v\n%s", tool, err, out.String())
		}
		line := strings.TrimSpace(out.String())
		_, rawURL, ok := strings.Cut(line, "GET ")
		if !ok {
			t.Fatalf("%s: unexpected dry run output %q", tool, line)
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if scope := taskscope.RouteScope("GET", u.Path); scope == "" {
			t.Errorf("%s requests %s, outside the task allowlist", tool, u.Path)
		}
		want := taskscope.ToolRoutes[tool][0]
		_, wantPath, _ := strings.Cut(want, " ")
		if strings.Count(wantPath, "/") != strings.Count(u.Path, "/") {
			t.Errorf("%s: recorded route %s does not match requested %s", tool, wantPath, u.Path)
		}
	}
}

// newTaskSession connects an in-memory client to a server in task mode whose
// token comes from a file re-read per call.
func newTaskSession(t *testing.T, baseURL, tokenPath string) *mcp.ClientSession {
	t.Helper()
	t.Setenv("ARCHIVIST_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_BASE_URL", baseURL)
	newRoot := func() *cobra.Command { return buildRootForTest("dev") }
	server, count := buildMCPServerWith(newRoot, "dev", fileToken(tokenPath), true)
	if count != len(expectedTaskToolNames) {
		t.Fatalf("registered %d tools in task mode", count)
	}
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "task-test", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestTaskModeTokenFileRotation(t *testing.T) {
	var mu sync.Mutex
	var bearers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"entity_resolution":null,"truncated":false,"next_cursor":null}`))
	}))
	t.Cleanup(srv.Close)
	tokenPath := filepath.Join(t.TempDir(), "task-token")
	if err := os.WriteFile(tokenPath, []byte(testTaskToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := newTaskSession(t, srv.URL, tokenPath)

	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(expectedTaskToolNames, ",") {
		t.Fatalf("task tools/list = %v", names)
	}

	callToolText(t, cs, "search", map[string]any{"query": "risk"})
	if err := os.WriteFile(tokenPath, []byte(testTaskToken2), 0o600); err != nil {
		t.Fatal(err)
	}
	callToolText(t, cs, "search", map[string]any{"query": "risk"})
	if len(bearers) != 2 || bearers[0] != "Bearer "+testTaskToken || bearers[1] != "Bearer "+testTaskToken2 {
		t.Fatalf("bearers = %q", bearers)
	}

	// A broken token file fails the call as an auth error without a request.
	if err := os.WriteFile(tokenPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, isErr := callToolText(t, cs, "search", map[string]any{"query": "risk"})
	if !isErr || !strings.Contains(text, "exit code 4") || len(bearers) != 2 {
		t.Fatalf("broken token file: isErr=%v bearers=%d text=%s", isErr, len(bearers), text)
	}
	// Tools outside the allowlist are not callable.
	res2, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "companies_get", Arguments: map[string]any{"issuer_key": "x"}})
	if err == nil && (res2 == nil || !res2.IsError) {
		t.Fatal("companies_get callable in task mode")
	}
}

func TestMCPServe_TokenFlagsExclusiveAndFileValidated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build in -short mode")
	}
	bin := buildTestBinary(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte("mst_bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "HOME="+dir, "ARCHIVIST_TOKEN=")
		c.Stdin = strings.NewReader("")
		out, _ := c.CombinedOutput()
		return c.ProcessState.ExitCode(), string(out)
	}
	if code, out := run("mcp", "serve", "--token", "ak_00000000000", "--token-file", tokenPath); code != 2 {
		t.Fatalf("both flags: exit %d %s", code, out)
	}
	if code, out := run("mcp", "serve", "--token-file", tokenPath); code != 4 || !strings.Contains(out, "task token format invalid") {
		t.Fatalf("malformed token file: exit %d %s", code, out)
	}
	if err := os.WriteFile(tokenPath, []byte(testTaskToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := run("mcp", "serve", "--token-file", tokenPath); code != 0 || !strings.Contains(out, "5 tools registered") || !strings.Contains(out, "task mode") {
		t.Fatalf("task token file: exit %d %s", code, out)
	}
}
