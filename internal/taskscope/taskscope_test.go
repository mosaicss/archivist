package taskscope

import (
	"strings"
	"testing"
)

// The Mosaic read tools (Story 78.32) only search and read; publish_artifact
// and the account verbs are not read tools.
func TestIsReadTool(t *testing.T) {
	for name, want := range map[string]bool{
		"search": true, "companies_search": true, "companies_get": true, "read_passage": true, "read_section": true,
		"toc": true, "filings": true, "find": true, "screen": true, PublishTool: false, "usage": false, "auth_status": false,
		"auth_whoami": false, "doctor": false, "version": false, "": false,
	} {
		if IsReadTool(name) != want {
			t.Errorf("IsReadTool(%q) != %v", name, want)
		}
	}
}

// Story 94.4: every Mosaic tool, publish_artifact included, runs without
// asking; the account verbs and unknown names are not Mosaic tools.
func TestMosaicToolsNeverAsk(t *testing.T) {
	for name, want := range map[string]bool{
		"search": true, "companies_search": true, "companies_get": true, "read_passage": true, "read_section": true,
		"toc": true, "filings": true, "find": true, "screen": true, PublishTool: true, "usage": false, "auth_status": false,
		"auth_whoami": false, "doctor": false, "version": false, "": false, "Bash": false,
	} {
		if IsMosaicTool(name) != want {
			t.Errorf("IsMosaicTool(%q) != %v", name, want)
		}
	}
	for _, name := range AutoAllowedTools() {
		if !IsMosaicTool(name) {
			t.Errorf("auto-allowed task tool %q is not a Mosaic tool", name)
		}
	}
	if strings.Join(AutoAllowedTools(), ",") != strings.Join(Tools(), ",") {
		t.Errorf("AutoAllowedTools() = %v, want every task tool %v", AutoAllowedTools(), Tools())
	}
}

// Story 81.4: the mirror of chat-api middleware/task-scope.ts gains the
// filings list (search scope) and the find route (read scope); the hidden
// admin verbs leave ToolRoutes, so the task tools are exactly these.
func TestTaskToolsMirrorChatAPI(t *testing.T) {
	if got := RouteScope("GET", "/research/filings"); got != "search" {
		t.Errorf("/research/filings scope %q", got)
	}
	if got := RouteScope("GET", "/research/filings/f1/find"); got != "read" {
		t.Errorf("/research/filings/:id/find scope %q", got)
	}
	// v0.2.35: the company screen is a search scope route, so screen is a
	// task tool that never asks.
	if got := RouteScope("GET", "/research/screen"); got != "search" || !IsReadTool("screen") || !IsMosaicTool("screen") {
		t.Errorf("/research/screen scope %q", got)
	}
	want := "companies_search,filings,find,publish_artifact,read_passage,read_section,screen,search,toc"
	if got := strings.Join(Tools(), ","); got != want {
		t.Fatalf("Tools() = %s", got)
	}
	if got := strings.Join(AutoAllowedTools(), ","); got != want {
		t.Fatalf("AutoAllowedTools() = %s", got)
	}
}
