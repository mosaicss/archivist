package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mosaicss/archivist/internal/guidance"
)

// Story 78.31: `archivist mcp serve` instructions are Mosaic's compact
// research guidance (agent-ui for an ak_ token, mosaic-ui for a task token)
// plus short exit code notes, at most 2048 characters; task mode pins tool
// output to JSON so each passage's cite_as survives.

func wantInstructions(surface string) string {
	return strings.TrimRight(guidance.Embedded(surface, guidance.FormCompact).Body, "\n") + "\n" + mcpLocalSuffix + "\n" + mcpExitNotes
}

func checkInstructionBounds(t *testing.T, got string) {
	t.Helper()
	if jsLen(got) > mcpInstructionsMax {
		t.Errorf("instructions are %d characters", jsLen(got))
	}
	if strings.Contains(got, "SEC and SEDAR") || !strings.Contains(got, "SEC, SEDAR+, KAP and expanding") {
		t.Error("instructions are not global filings wording")
	}
	if strings.ContainsAny(got, "-–—") {
		t.Error("instructions carry a hyphen or dash")
	}
}

func TestMCPInstructionsNonTaskAreAgentUICompact(t *testing.T) {
	cs := newMCPSession(t, "")
	got := cs.InitializeResult().Instructions
	if got != wantInstructions(guidance.SurfaceAgentUI) {
		t.Fatalf("instructions %q", got)
	}
	checkInstructionBounds(t, got)
	if !strings.Contains(got, "markdown link [label](url)") || strings.Contains(got, "cite_as") {
		t.Error("ak_ instructions must cite urls, not cite_as")
	}
}

func TestMCPInstructionsTaskModeAreMosaicUICompact(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "task-token")
	if err := os.WriteFile(tokenPath, []byte(testTaskToken), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := newTaskSession(t, "http://127.0.0.1:1", tokenPath)
	got := cs.InitializeResult().Instructions
	if got != wantInstructions(guidance.SurfaceMosaicUI) {
		t.Fatalf("task instructions %q", got)
	}
	checkInstructionBounds(t, got)
	if !strings.Contains(got, "cite_as") {
		t.Error("task instructions must cite cite_as")
	}
}

func TestMCPExitNotesBounds(t *testing.T) {
	if jsLen(mcpExitNotes) > mcpExitNotesMax || strings.ContainsAny(mcpExitNotes, "-–—\n") {
		t.Fatalf("exit notes %d characters: %q", jsLen(mcpExitNotes), mcpExitNotes)
	}
	for _, code := range []string{"2 ", "3 ", "4 ", "6 ", "7 "} {
		if !strings.Contains(mcpExitNotes, code) {
			t.Errorf("exit notes lack code %s", code)
		}
	}
}

func TestMCPInstructionsFromLiveAndOversize(t *testing.T) {
	live := guidance.Text{Body: "Live compact guidance.\n", Source: guidance.SourceLive}
	if got := mcpInstructionsFrom(live, guidance.SurfaceAgentUI); got != "Live compact guidance.\n"+mcpLocalSuffix+"\n"+mcpExitNotes {
		t.Fatalf("live %q", got)
	}
	// Too long with the suffix but not without it: the suffix goes first.
	room := mcpInstructionsMax - jsLen(mcpExitNotes) - 1
	tight := guidance.Text{Body: strings.Repeat("a", room-10) + "\n", Source: guidance.SourceLive}
	if got := mcpInstructionsFrom(tight, guidance.SurfaceAgentUI); got != strings.Repeat("a", room-10)+"\n"+mcpExitNotes {
		t.Fatalf("tight live text kept the suffix or lost its body: %d characters", jsLen(got))
	}
	huge := guidance.Text{Body: strings.Repeat("a", mcpInstructionsMax), Source: guidance.SourceLive}
	if got := mcpInstructionsFrom(huge, guidance.SurfaceMosaicUI); got != wantInstructions(guidance.SurfaceMosaicUI) {
		t.Fatal("an oversize live text must fall back to the embedded compact form with the suffix")
	}
}

// Story 81.4: the local suffix maps the hosted flow onto this server's tool
// names, has no hyphen or dash, and fits with both embedded forms.
func TestMCPLocalSuffix(t *testing.T) {
	const want = "This server's tools: search (latest_only for the newest filing of a form), read_passage, toc and " +
		"read_section; filing_id feeds the last two. filings lists a company's filings newest first; " +
		"find says whether one filing mentions a term."
	if mcpLocalSuffix != want {
		t.Fatalf("suffix %q", mcpLocalSuffix)
	}
	if strings.ContainsAny(mcpLocalSuffix, "-–—\n") {
		t.Fatal("suffix carries a hyphen, dash or newline")
	}
	for _, surface := range []string{guidance.SurfaceAgentUI, guidance.SurfaceMosaicUI} {
		if got := embeddedMCPInstructions(surface == guidance.SurfaceMosaicUI); !strings.Contains(got, mcpLocalSuffix) || jsLen(got) > mcpInstructionsMax {
			t.Errorf("%s: %d characters, suffix present %v", surface, jsLen(got), strings.Contains(got, mcpLocalSuffix))
		}
	}
}

// The serve RunE itself in task mode: the startup fetch asks for mosaic-ui
// compact without credentials and the live text becomes the instructions.
func TestMCPServeTaskModeFetchesLiveMosaicUIGuidance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build in -short mode")
	}
	bin := buildTestBinary(t)
	const live = "Live mosaic-ui compact guidance from chat-api.\n"
	var mu sync.Mutex
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/agent-guidance" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		seen = append(seen, r.Clone(context.Background()))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": guidance.SchemaVersion,
			"surface": "mosaic-ui", "form": "compact", "digest": guidance.Digest(live), "text": live})
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "task-token")
	if err := os.WriteFile(tokenPath, []byte(testTaskToken), 0o600); err != nil {
		t.Fatal(err)
	}
	serve := exec.Command(bin, "mcp", "serve", "--token-file", tokenPath)
	serve.Env = append(os.Environ(), "HOME="+dir, "USERPROFILE="+dir, "ARCHIVIST_TOKEN=", "ARCHIVIST_BASE_URL="+srv.URL)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "task-guidance-test", Version: "0"}, nil).
		Connect(context.Background(), &mcp.CommandTransport{Command: serve}, nil)
	if err != nil {
		t.Fatalf("initialize over stdio: %v", err)
	}
	defer func() { _ = cs.Close() }()
	if got := cs.InitializeResult().Instructions; got != strings.TrimRight(live, "\n")+"\n"+mcpLocalSuffix+"\n"+mcpExitNotes {
		t.Fatalf("task mode instructions %q", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("guidance requests %d", len(seen))
	}
	if q := seen[0].URL.Query(); len(q) != 2 || q.Get("surface") != "mosaic-ui" || q.Get("form") != "compact" {
		t.Fatalf("guidance query %q", seen[0].URL.RawQuery)
	}
	if seen[0].Header.Get("Authorization") != "" {
		t.Fatal("the guidance fetch sent a credential")
	}
}

func TestTaskModePinsJSONFormat(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rec := `{"id":"11111111-2222-4333-8444-555555555555","url":"https://mosaic-finance.com/filings/f1/p/c1/t/","cite_as":"[cite:1.1]"}`
		switch {
		case strings.HasPrefix(r.URL.Path, "/research/passages/"):
			_, _ = w.Write([]byte(`{"passage":` + rec + `,"neighbours":[` + strings.Replace(rec, "1.1", "1.2", 1) + `],"truncated":false,"next_cursor":null}`))
		case strings.HasSuffix(r.URL.Path, "/sections"):
			_, _ = w.Write([]byte(`{"filing_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","section_header":"Risk Factors","chunk_count":1,"passages":[` + strings.Replace(rec, "1.1", "2.1", 1) + `],"truncated":false,"next_cursor":null}`))
		default:
			_, _ = w.Write([]byte(`{"results":[` + rec + `],"entity_resolution":null,"truncated":false,"next_cursor":null}`))
		}
	}))
	t.Cleanup(srv.Close)
	tokenPath := filepath.Join(t.TempDir(), "task-token")
	if err := os.WriteFile(tokenPath, []byte(testTaskToken), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := newTaskSession(t, srv.URL, tokenPath)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		raw, _ := tool.InputSchema.(map[string]any)
		props, ok := raw["properties"].(map[string]any)
		if !ok {
			t.Fatalf("task tool %s schema %T", tool.Name, tool.InputSchema)
		}
		if _, ok := props["format"]; ok {
			t.Errorf("task tool %s still offers format", tool.Name)
		}
	}
	text, isErr := callToolText(t, cs, "search", map[string]any{"query": "reserves"})
	if isErr || !strings.Contains(text, `"cite_as": "[cite:1.1]"`) {
		t.Fatalf("task search output lost cite_as: isErr=%v\n%s", isErr, text)
	}
	if _, isErr := callToolText(t, cs, "search", map[string]any{"query": "reserves", "format": "table"}); !isErr {
		t.Fatal("a format argument must be refused in task mode")
	}
	// The read tools keep cite_as too (passage and neighbours; section passages).
	text, isErr = callToolText(t, cs, "read_passage", map[string]any{"chunk_id": "11111111-2222-4333-8444-555555555555"})
	if isErr || !strings.Contains(text, `"cite_as": "[cite:1.1]"`) || !strings.Contains(text, `"cite_as": "[cite:1.2]"`) {
		t.Fatalf("task read_passage output lost cite_as: isErr=%v\n%s", isErr, text)
	}
	text, isErr = callToolText(t, cs, "read_section", map[string]any{
		"filing_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "section_header": "Risk Factors"})
	if isErr || !strings.Contains(text, `"cite_as": "[cite:2.1]"`) {
		t.Fatalf("task read_section output lost cite_as: isErr=%v\n%s", isErr, text)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, want := range []string{"/research/search", "/research/passages/11111111-2222-4333-8444-555555555555",
		"/research/filings/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/sections"} {
		if !slices.Contains(queries, want) {
			t.Fatalf("requests %v lack %s", queries, want)
		}
	}
}

// Story 81.4: without a task token every tool pins --format=compact: no
// format (or dry_run) argument in any schema, a passed format is refused, and
// the output is the minified projection.
func TestNonTaskPinsCompactFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"c1","filing_id":"f1","exchange":"NGS","formtype":"10-K","formdescription":"Annual report",` +
			`"chunk_index":4,"snippet":"Revenue & margin.","url":"https://mosaic-finance.com/filings/f1/p/c1/t/"}],` +
			`"entity_resolution":null,"truncated":false,"next_cursor":null}`))
	}))
	t.Cleanup(srv.Close)
	cs := newMCPSession(t, srv.URL)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		raw, _ := tool.InputSchema.(map[string]any)
		props, ok := raw["properties"].(map[string]any)
		if !ok {
			t.Fatalf("tool %s schema %T", tool.Name, tool.InputSchema)
		}
		for _, banned := range []string{"format", "dry_run"} {
			if _, ok := props[banned]; ok {
				t.Errorf("tool %s still offers %s", tool.Name, banned)
			}
		}
	}
	text, isErr := callToolText(t, cs, "search", map[string]any{"query": "margin"})
	want := `{"results":[{"id":"c1","filing_id":"f1","formtype":"10-K","snippet":"Revenue & margin.","url":"https://mosaic-finance.com/filings/f1/p/c1/t/"}]}` + "\n"
	if isErr || text != want {
		t.Fatalf("compact search output: isErr=%v\n%s", isErr, text)
	}
	for _, args := range []map[string]any{{"query": "x", "format": "json"}, {"query": "x", "dry_run": true}} {
		if text, isErr := callToolText(t, cs, "search", args); !isErr || !strings.Contains(text, "exit code 2") {
			t.Errorf("args %v: isErr=%v %s", args, isErr, text)
		}
	}
}

func TestBuildArgvPinFormat(t *testing.T) {
	for format, want := range map[string]string{
		"json":    "search --format=json --token mst_tok -- x",
		"compact": "search --format=compact --token mst_tok -- x",
	} {
		spec := toolSpec{Name: "search", Path: []string{"search"}, Positionals: []string{"query"}, FlagFor: map[string]string{}, PinFormat: format}
		argv, usage := buildArgv(spec, []byte(`{"query":"x"}`), "mst_tok")
		if usage != "" || strings.Join(argv, " ") != want {
			t.Fatalf("argv %v usage %q", argv, usage)
		}
	}
}
