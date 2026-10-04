// Package connect implements `archivist connect` (Story 78.16): a foreground
// daemon that keeps an outbound socket to the Mosaic agent relay and drives
// the user's own Claude Code for each relay session, with Mosaic search and
// read tools served by `archivist mcp serve` under a session task token.
//
// The relay supplies data only (session ids, prompts, decisions). Binaries,
// arguments, flags and the child environment are decided here.
package connect

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// allowKeys are copied from the daemon environment when present.
var allowKeys = map[string]bool{
	"HOME": true, "PATH": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "TERM": true, "TMPDIR": true,
}

// CACertKeys name the CA bundle a TLS-intercepting egress (the Cloudflare
// sandbox's Outbound, Story 78.22) needs every harness to trust. They hold
// file paths, never credentials, and pass to harness children (and Codex's
// command environment) like the allowKeys.
var CACertKeys = []string{
	"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "GIT_SSL_CAINFO", "REQUESTS_CA_BUNDLE",
}

func init() {
	for _, k := range CACertKeys {
		allowKeys[k] = true
	}
}

// allowPrefixes are copied from the daemon environment when present.
var allowPrefixes = []string{"LC_", "XDG_"}

// denyPrefixes win over the allowlist and over overrides: provider keys,
// harness configuration, archivist credentials and parent-session state.
var denyPrefixes = []string{
	"ANTHROPIC_", "CLAUDE_CODE_", "CLAUDE_", "OPENAI_", "CODEX_", "ARCHIVIST_", "HERDR_",
}

// ClaudeConfigDirKey names the directory holding the owner's Claude Code
// login when it is not ~/.claude (Story 78.30). It is the one CLAUDE_ key a
// harness child may see, copied from the daemon environment only when it is
// an absolute path to an existing directory; it is never an override. The
// subscription proof (claude auth status, system/init apiKeySource) still
// runs on whatever login that directory holds.
const ClaudeConfigDirKey = "CLAUDE_CONFIG_DIR"

// claudeConfigDirAllowed reports whether v may pass as CLAUDE_CONFIG_DIR.
func claudeConfigDirAllowed(v string) bool {
	if v == "" || !filepath.IsAbs(v) || strings.ContainsAny(v, "\x00\n\r") {
		return false
	}
	info, err := os.Stat(v)
	return err == nil && info.IsDir()
}

// EnvAllowed reports whether key may reach a harness child.
func EnvAllowed(key string) bool {
	for _, p := range denyPrefixes {
		if strings.HasPrefix(key, p) {
			return false
		}
	}
	if allowKeys[key] {
		return true
	}
	for _, p := range allowPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// BuildChildEnv returns the child environment (os.Environ form, sorted by
// key) built only from allowlisted parent keys (plus an absolute, existing
// CLAUDE_CONFIG_DIR), then overrides. An override
// for a key outside the allowlist, or under a deny prefix, is an error.
func BuildChildEnv(parent []string, overrides map[string]string) ([]string, error) {
	vals := map[string]string{}
	for _, kv := range parent {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k == ClaudeConfigDirKey {
			if claudeConfigDirAllowed(v) {
				vals[k] = v
			}
			continue
		}
		if !ok || k == "" || !EnvAllowed(k) {
			continue
		}
		vals[k] = v
	}
	for k, v := range overrides {
		if !EnvAllowed(k) {
			return nil, fmt.Errorf("connect: environment override %q is not allowed", k)
		}
		vals[k] = v
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+vals[k])
	}
	return env, nil
}

// EnvKeys returns the sorted key names of env (never values), for logs.
func EnvKeys(env []string) []string {
	keys := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
