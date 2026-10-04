package taskscope

import "testing"

// The Mosaic read tools (Story 78.32) are answered allow in every mode;
// publish_artifact and the account verbs are not read tools.
func TestIsReadTool(t *testing.T) {
	for name, want := range map[string]bool{
		"search": true, "companies_search": true, "companies_get": true, "read_passage": true, "read_section": true,
		"toc": true, PublishTool: false, "usage": false, "auth_status": false, "doctor": false, "": false,
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
