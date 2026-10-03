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

var readRoute = regexp.MustCompile(`^(/research/passages/[^/]+|/research/filings/[^/]+/(toc|sections)|/uploads/[^/]+/chunks/[0-9]+)$`)

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
	case "/research/search", "/research/companies", "/uploads/search":
		return "search"
	}
	if readRoute.MatchString(path) {
		return "read"
	}
	return ""
}

// ToolRoutes lists every chat-api route each MCP-exposed verb can call
// (":id" stands for a path parameter). A verb missing here is never exposed
// in task mode.
var ToolRoutes = map[string][]string{
	"search":           {"GET /research/search"},
	"companies_search": {"GET /research/companies"},
	"companies_get":    {"GET /research/companies", "GET /companies"},
	"read_passage":     {"GET /research/passages/:id"},
	"read_section":     {"GET /research/filings/:id/sections"},
	"toc":              {"GET /research/filings/:id/toc"},
	"auth_status":      {"GET /account/cli-tokens"},
	"auth_whoami":      {"GET /account/cli-tokens"},
	"usage":            {"GET /account/usage"},
	"doctor":           {"GET /health", "GET /account/cli-tokens"},
	PublishTool:        {"POST /artifacts"},
}

// ApprovalRequired reports a task tool that must never be pre-allowed: every
// call goes through the harness's permission prompt (the approval card).
func ApprovalRequired(name string) bool { return name == PublishTool }

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

// AutoAllowedTools are the task mode tools a harness may run without asking:
// Tools() without the approval-required ones.
func AutoAllowedTools() []string {
	var out []string
	for _, name := range Tools() {
		if !ApprovalRequired(name) {
			out = append(out, name)
		}
	}
	return out
}

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
