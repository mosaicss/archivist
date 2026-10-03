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

// Scopes are the scopes `archivist connect` mints task tokens with.
var Scopes = []string{"search", "read"}

var readRoute = regexp.MustCompile(`^(/research/passages/[^/]+|/research/filings/[^/]+/(toc|sections)|/uploads/[^/]+/chunks/[0-9]+)$`)

// RouteScope mirrors chat-api's taskRouteScope
// (chat-api/src/middleware/task-scope.ts): the scope a route needs, or ""
// when a task token may not call it.
func RouteScope(method, path string) string {
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
}

// ToolAllowed reports whether every route the tool calls is allowed under
// the minted scopes.
func ToolAllowed(name string) bool {
	routes, ok := ToolRoutes[name]
	if !ok || len(routes) == 0 {
		return false
	}
	for _, r := range routes {
		method, path, _ := strings.Cut(r, " ")
		scope := RouteScope(method, path)
		if scope == "" || !contains(Scopes, scope) {
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

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
