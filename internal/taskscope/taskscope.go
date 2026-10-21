// Package taskscope holds the task token tool allowlist (Story 78.16): which
// archivist MCP tools a session task token (mst_) may use. It is derived from
// chat-api's 78.15 HTTP allowlist, so the scope comes from `archivist mcp
// serve` itself rather than from harness flags.
package taskscope

import (
	"regexp"
	"sort"
	"strings"
)

// Scopes are the scopes `archivist connect` mints task tokens with. publish
// (Story 78.18) admits only POST /artifacts, which publish_artifact calls.
var Scopes = []string{"search", "read", "publish"}

// PublishTool is the one task tool that is not a CLI verb: `archivist mcp
// serve` registers it itself, only for a session that was granted the
// publish scope (Story 78.18).
const PublishTool = "publish_artifact"

var readRoute = regexp.MustCompile(`^(/research/passages/[^/]+|/research/filings/[^/]+/(toc|sections|find)|/uploads/[^/]+/chunks/[0-9]+)$`)

// RouteScope mirrors chat-api's taskRouteScope
// (chat-api/src/middleware/task-scope.ts): the scope a route needs, or ""
// when a task token may not call it.
func RouteScope(method, path string) string {
	if method == "POST" && path == "/artifacts" {
		return "publish"
	}
	if method != "GET" {
		return ""
	}
	switch path {
	case "/research/search", "/research/companies", "/research/filings", "/research/screen", "/uploads/search":
		return "search"
	}
	if readRoute.MatchString(path) {
		return "read"
	}
	return ""
}

// ToolRoutes lists every chat-api route each MCP-exposed verb can call
// (":id" stands for a path parameter). A verb missing here is never exposed
// in task mode. The admin verbs (auth status, auth whoami, doctor, usage,
// version) are hidden from MCP since Story 81.4, so they have no entry.
var ToolRoutes = map[string][]string{
	"search":           {"GET /research/search"},
	"companies_search": {"GET /research/companies"},
	"companies_get":    {"GET /research/companies", "GET /companies"},
	"read_passage":     {"GET /research/passages/:id"},
	"read_section":     {"GET /research/filings/:id/sections"},
	"toc":              {"GET /research/filings/:id/toc"},
	"filings":          {"GET /research/filings"},
	"screen":           {"GET /research/screen"},
	"find":             {"GET /research/filings/:id/find"},
	PublishTool:        {"POST /artifacts"},
}

// IsMosaicTool reports a Mosaic tool (by its MCP tool name): any task tool
// with routes, publish_artifact included. Since Story 94.4 a session
// answers every Mosaic tool allow in every permission mode, on both
// harnesses, without an approval card; the mode governs only the tools that
// touch the computer (shell, edit, write).
func IsMosaicTool(name string) bool {
	_, ok := ToolRoutes[name]
	return ok
}

// ReadTools are the Mosaic read tools (Story 78.32): they only search and
// read filings; publish_artifact is not one. filings and find joined in
// Story 81.4, screen (the company screen, search scope) in v0.2.35.
var ReadTools = []string{"search", "companies_search", "companies_get", "read_passage", "read_section", "toc", "filings", "screen", "find"}

// IsReadTool reports a Mosaic read tool (by its MCP tool name).
func IsReadTool(name string) bool { return contains(ReadTools, name) }

// ToolAllowed reports whether every route the tool calls is allowed under
// the minted scopes.
func ToolAllowed(name string) bool { return ToolAllowedFor(name, Scopes) }

// ToolAllowedFor is ToolAllowed under the scopes a token was granted.
func ToolAllowedFor(name string, granted []string) bool {
	routes, ok := ToolRoutes[name]
	if !ok || len(routes) == 0 {
		return false
	}
	for _, r := range routes {
		method, path, _ := strings.Cut(r, " ")
		scope := RouteScope(method, path)
		if scope == "" || !contains(granted, scope) {
			return false
		}
	}
	return true
}

// Tools returns the sorted task mode tool names.
func Tools() []string {
	var out []string
	for name := range ToolRoutes {
		if ToolAllowed(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// AutoAllowedTools are the task mode tools a harness runs without asking:
// every one of Tools() (Story 94.4: Mosaic tools never ask).
func AutoAllowedTools() []string { return Tools() }

// Granted reports whether scope is among the granted scopes.
func Granted(granted []string, scope string) bool { return contains(granted, scope) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
