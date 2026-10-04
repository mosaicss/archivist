package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/guidance"
)

// Story 78.31: `archivist mcp serve` instructions are Mosaic's compact
// research guidance (agent-ui for an ak_ token, mosaic-ui for a task token)
// plus short exit code notes, at most 2048 characters; task mode pins tool
// output to JSON so each passage's cite_as survives.

func wantInstructions(surface string) string {
	return strings.TrimRight(guidance.Embedded(surface, guidance.FormCompact).Body, "\n") + "\n" + mcpExitNotes
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
	if got := mcpInstructionsFrom(live, guidance.SurfaceAgentUI); got != "Live compact guidance.\n"+mcpExitNotes {
		t.Fatalf("live %q", got)
	}
	huge := guidance.Text{Body: strings.Repeat("a", mcpInstructionsMax), Source: guidance.SourceLive}
	if got := mcpInstructionsFrom(huge, guidance.SurfaceMosaicUI); got != wantInstructions(guidance.SurfaceMosaicUI) {
		t.Fatal("an oversize live text must fall back to the embedded compact form")
	}
}

func TestTaskModePinsJSONFormat(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"c1","url":"https://mosaic-finance.com/filings/f1/p/c1/t/","cite_as":"[cite:1.1]"}],"entity_resolution":null,"truncated":false,"next_cursor":null}`))
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
	if !slices.Contains(queries, "/research/search") {
		t.Fatalf("requests %v", queries)
	}
}

func TestNonTaskKeepsFormatArgument(t *testing.T) {
	cs := newMCPSession(t, "")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "search" {
			continue
		}
		raw, _ := tool.InputSchema.(map[string]any)
		props, _ := raw["properties"].(map[string]any)
		if _, ok := props["format"]; !ok {
			t.Fatal("non task search lost its format argument")
		}
	}
}

func TestBuildArgvPinJSON(t *testing.T) {
	spec := toolSpec{Name: "search", Path: []string{"search"}, Positionals: []string{"query"}, FlagFor: map[string]string{}, PinJSON: true}
	argv, usage := buildArgv(spec, []byte(`{"query":"x"}`), "mst_tok")
	if usage != "" || strings.Join(argv, " ") != "search --format=json --token mst_tok -- x" {
		t.Fatalf("argv %v usage %q", argv, usage)
	}
}
