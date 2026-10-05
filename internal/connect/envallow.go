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
	"regexp"
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
	// Story 78.34: the Windows core keys (none elsewhere), stored in the
	// form envKey compares.
	for _, k := range platformAllowKeys {
		allowKeys[envKey(k)] = true
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

// EnvAllowed reports whether key may reach a harness child. On Windows,
// where environment keys are case-insensitive, the allowlist and the deny
// prefixes compare case-insensitively (envKey); elsewhere exactly.
func EnvAllowed(key string) bool {
	k := envKey(key)
	for _, p := range denyPrefixes {
		if strings.HasPrefix(k, p) {
			return false
		}
	}
	if allowKeys[k] {
		return true
	}
	for _, p := range allowPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// BuildChildEnv returns the child environment (os.Environ form, sorted by
// key) built only from allowlisted parent keys (plus an absolute, existing
// CLAUDE_CONFIG_DIR), then overrides. An override
// for a key outside the allowlist, or under a deny prefix, is an error.
// Keys keep the parent's spelling (on Windows an override of a key the
// parent has under another case replaces that entry and keeps its name).
func BuildChildEnv(parent []string, overrides map[string]string) ([]string, error) {
	type entry struct{ name, value string }
	vals := map[string]entry{}
	for _, kv := range parent {
		k, v, ok := strings.Cut(kv, "=")
		if ok && envKey(k) == envKey(ClaudeConfigDirKey) {
			if claudeConfigDirAllowed(v) {
				vals[envKey(k)] = entry{k, v}
			}
			continue
		}
		if !ok || k == "" || !EnvAllowed(k) {
			continue
		}
		vals[envKey(k)] = entry{k, v}
	}
	for k, v := range overrides {
		if !EnvAllowed(k) {
			return nil, fmt.Errorf("connect: environment override %q is not allowed", k)
		}
		name := k
		if cur, ok := vals[envKey(k)]; ok {
			name = cur.name
		}
		vals[envKey(k)] = entry{name, v}
	}
	keys := make([]string, 0, len(vals))
	byName := make(map[string]string, len(vals))
	for _, e := range vals {
		keys = append(keys, e.name)
		byName[e.name] = e.value
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+byName[k])
	}
	return env, nil
}

// ClaudeDisableWebFetchKey turns Claude Code's WebFetch tool off (Claude
// Code 2.1.285 and later; older versions ignore it). Story 78.33: every
// Claude session launch sets it, on top of --tools leaving WebFetch out, so
// no page fetch runs on this machine whether or not web search is on.
const ClaudeDisableWebFetchKey = "CLAUDE_CODE_DISABLE_WEB_FETCH"

// ClaudeSessionEnv returns env (BuildChildEnv's result) plus the adapter
// set Claude keys, sorted by key. These are the only CLAUDE_CODE_ keys a
// Claude session sees: the deny prefixes keep the daemon's own out, and
// they are fixed here, never overrides.
func ClaudeSessionEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != ClaudeDisableWebFetchKey {
			out = append(out, kv)
		}
	}
	out = append(out, ClaudeDisableWebFetchKey+"=1")
	sort.Slice(out, func(i, j int) bool {
		ki, _, _ := strings.Cut(out[i], "=")
		kj, _, _ := strings.Cut(out[j], "=")
		return ki < kj
	})
	return out
}

// ClaudeAPIKeyEnvKey is the one ANTHROPIC_ key a harness child may see
// (Story 78.38): only in the session-bound sandbox's API key mode, set by
// the adapter from the sign in request file, never copied from the daemon
// environment and never an override. In the Mosaic sandbox the value is a
// fixed placeholder that the sandbox's egress swaps for the user's key on
// api.anthropic.com only, so the real key never enters the container.
const ClaudeAPIKeyEnvKey = "ANTHROPIC_API_KEY"

// apiKeyRe is the key shape the sign in request file may carry, the same
// rule in every Story 78.38 lane (the sandbox Worker's placeholder fits
// it). Anthropic does not document the key format: the base64url body after
// sk-ant- is an assumption from the keys seen so far, which the lab check
// confirms.
var apiKeyRe = regexp.MustCompile(`^sk-ant-[A-Za-z0-9_-]{16,500}$`)

// ValidAPIKey reports whether key may be passed as ClaudeAPIKeyEnvKey.
func ValidAPIKey(key string) bool { return apiKeyRe.MatchString(key) }

// ClaudeAPIKeyEnv returns env (a ClaudeSessionEnv or BuildChildEnv result)
// with ClaudeAPIKeyEnvKey set to key, sorted by key. It is the API key mode
// adapter set exception to the ANTHROPIC_ deny prefix. The value is never
// logged: log env with EnvKeys only. An invalid key is an error that never
// carries the value.
func ClaudeAPIKeyEnv(env []string, key string) ([]string, error) {
	if !ValidAPIKey(key) {
		return nil, fmt.Errorf("connect: the sign in API key is not an Anthropic API key")
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != ClaudeAPIKeyEnvKey {
			out = append(out, kv)
		}
	}
	out = append(out, ClaudeAPIKeyEnvKey+"="+key)
	sort.Slice(out, func(i, j int) bool {
		ki, _, _ := strings.Cut(out[i], "=")
		kj, _, _ := strings.Cut(out[j], "=")
		return ki < kj
	})
	return out, nil
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
