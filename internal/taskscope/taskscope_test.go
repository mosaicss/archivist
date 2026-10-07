package taskscope

import (
	"strings"
	"testing"
)

// The Mosaic read tools (Story 78.32) are answered allow in every mode;
// publish_artifact and the account verbs are not read tools.
func TestIsReadTool(t *testing.T) {
	for name, want := range map[string]bool{
		"search": true, "companies_search": true, "companies_get": true, "read_passage": true, "read_section": true,
		"toc": true, "filings": true, "find": true, PublishTool: false, "usage": false, "auth_status": false,
		"auth_whoami": false, "doctor": false, "version": false, "": false,
	} {
		if IsReadTool(name) != want {
			t.Errorf("IsReadTool(%q) != %v", name, want)
		}
	}
	for _, name := range AutoAllowedTools() {
		if !IsReadTool(name) {
			t.Errorf("auto-allowed task tool %q is not a read tool", name)
		}
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
	want := "companies_search,filings,find,publish_artifact,read_passage,read_section,search,toc"
	if got := strings.Join(Tools(), ","); got != want {
		t.Fatalf("Tools() = %s", got)
	}
	if got := strings.Join(AutoAllowedTools(), ","); got != "companies_search,filings,find,read_passage,read_section,search,toc" {
		t.Fatalf("AutoAllowedTools() = %s", got)
	}
}
