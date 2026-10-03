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
}{
	{regexp.MustCompile(`(?i)Bearer\s+[^\s"',;]+`), false},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]+`), false},
	{regexp.MustCompile(`mst_[A-Za-z0-9_.\-]+`), true},
	{regexp.MustCompile(`mc_pat_[A-Za-z0-9_\-]+`), true},
	{regexp.MustCompile(`\bak_[A-Za-z0-9_\-]+`), true},
	// Relay tickets: base64url JSON claims ("{" encodes as "eyJ") plus a
	// 43 character HMAC.
	{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{43}`), false},
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
		s = p.re.ReplaceAllString(s, "[redacted]")
	}
	return s
}

// TicketFingerprint is the log-safe identity of a relay ticket.
func TicketFingerprint(ticket string) string {
	h := sha256.Sum256([]byte(ticket))
	return fmt.Sprintf("%x", h[:4])
}
