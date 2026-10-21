package connect

import (
	"crypto/sha256"
	"fmt"
	"regexp"

	"github.com/mosaicss/archivist/internal/auth"
)

// secretPatterns match credentials that may appear in harness stderr or in
// our own error text. Archivist credentials keep a fingerprint so logs can
// still be correlated; everything else becomes a fixed marker.
var secretPatterns = []struct {
	re          *regexp.Regexp
	fingerprint bool
	// repl, when set, is the replacement template (a kept label group).
	repl string
}{
	// PEM private key blocks (Story 94.5), to the END line or the end of
	// the text when it was cut.
	// PGP private key blocks too.
	{re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----[\s\S]*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|$)`)},
	{re: regexp.MustCompile(`(?i)Bearer\s+[^\s"',;]+`)},
	// Anthropic keys: the whole key the sign in request file's rule accepts
	// (Story 78.38; its base64url body is an assumption from the keys seen so
	// far) plus other printable characters, except quotes,
	// whitespace, backslash and the delimiters ( ) , < > [ ] { } that end a
	// key in text and JSON.
	{re: regexp.MustCompile(`sk-ant-[!#-&*+\x2d-;=?-Z^_a-z|~]+`)},
	{re: regexp.MustCompile(`mst_[A-Za-z0-9_.\-]+`), fingerprint: true},
	{re: regexp.MustCompile(`mc_pat_[A-Za-z0-9_\-]+`), fingerprint: true},
	{re: regexp.MustCompile(`\bak_[A-Za-z0-9_\-]+`), fingerprint: true},
	// JWTs (Codex's ChatGPT OAuth tokens) before the two-part ticket rule.
	{re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`)},
	// Relay tickets: base64url JSON claims ("{" encodes as "eyJ") plus a
	// 43 character HMAC.
	{re: regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{43}`)},
	// OpenAI API keys (Codex, Story 78.17).
	{re: regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`)},
	// Story 94.5: common token formats. GitHub tokens (classic and fine
	// grained).
	{re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`)},
	{re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`)},
	// AWS access key ids and secret access key values (the label kept).
	{re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{re: regexp.MustCompile(`(?i)(aws_secret_access_key(?:\\?["'])?\s*[:=]\s*(?:\\?["'])?)[A-Za-z0-9/+=]{16,}`), repl: "${1}[redacted]"},
	// Slack tokens.
	{re: regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{re: regexp.MustCompile(`\bxapp-[A-Za-z0-9-]{10,}`)},
	// Stripe live secret and restricted keys.
	{re: regexp.MustCompile(`\b[sr]k_live_[A-Za-z0-9]{10,}`)},
	// Google API keys and OAuth access tokens.
	{re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{re: regexp.MustCompile(`\bya29\.[0-9A-Za-z_\-]+`)},
}

// Scrub replaces every recognised credential in s.
func Scrub(s string) string {
	for _, p := range secretPatterns {
		if p.fingerprint {
			s = p.re.ReplaceAllStringFunc(s, func(m string) string {
				return "[redacted fp:" + auth.Fingerprint(m) + "]"
			})
			continue
		}
		if p.repl != "" {
			s = p.re.ReplaceAllString(s, p.repl)
			continue
		}
		s = p.re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// TicketFingerprint is the log-safe identity of a relay ticket.
func TicketFingerprint(ticket string) string {
	h := sha256.Sum256([]byte(ticket))
	return fmt.Sprintf("%x", h[:4])
}
