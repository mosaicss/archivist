package connect

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Filing source links in approval cards (Story 94.7 A1). A link to a
// filing source host (a regulator's, exchange's or filing vendor's own
// document URL) never reaches Mosaic output, approval cards included. The
// daemon removes such links from every string of an approval descriptor
// before it hashes the card and sends it, so the browser signs exactly the
// card it shows; the harness still gets the tool call's original input when
// it is allowed.
//
// This is a port of the workspace's stripFilingLinks and
// stripFilingLinksDeep (Story 78.33, apps/workspace filingLinks.js), with
// the same host list. A host is a filing source when it equals an entry of
// filingSourceHosts or ends with "." plus one; storage.googleapis.com only
// under the filing buckets (the path /themcp-v2-filings…, or the bucket as
// a virtual host). A markdown link or image keeps its label or alt text;
// an autolink, a reference definition and a bare scheme:// or www. address
// become "(source link removed)". Where Go's URL reading may differ from a
// browser's, the check leans toward removing: it removes at least what the
// workspace removes. In particular any link whose host has a non-ASCII
// character is removed, whatever host a browser maps it to (this only
// changes what the card shows).

// filingSourceHosts are the workspace's FILING_SOURCE_HOSTS
// (testdata/filing-source-hosts.json holds the shared copy).
var filingSourceHosts = []string{
	"sec.gov",
	"sedarplus.ca",
	"sedarplus.com",
	"sedar.com",
	"kap.org.tr",
	"quotemedia.com",
	"tmx.com",
	"tmxmoney.com",
	"tmxinfoservices.com",
	"tmxwebstore.com",
	"financialfilings.com",
	"financialreports.eu",
	"r2.cloudflarestorage.com",
}

const (
	filingBucketPrefix = "themcp-v2-filings"
	gcsHost            = "storage.googleapis.com"
	removedLinkText    = "(source link removed)"
)

// jsSpace is the JavaScript \s class (WhiteSpace and LineTerminator), for
// a regexp character class; RE2's \s is ASCII only.
const jsSpace = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// The workspace's patterns, with \s spelled as jsSpace and the case
// insensitive scheme and www. spelled out in ASCII (RE2's (?i) also folds
// non-ASCII letters such as U+017F into s; JavaScript's non-unicode /i
// does not).
var (
	// [label](url) or ![alt](url), url bare or <…>, with an optional title.
	markdownLinkRe = regexp.MustCompile(`(!?)\[([^\]\n]*)\]\([` + jsSpace + `]*(<[^>\n]*>|[^` + jsSpace + `()]*(?:\([^` + jsSpace + `()]*\)[^` + jsSpace + `()]*)*)(?:[` + jsSpace + `]+(?:"[^"\n]*"|'[^'\n]*'|\([^)\n]*\)))?[` + jsSpace + `]*\)`)
	// [label]: url "title" at the start of a line; the line end is
	// captured (JavaScript's multiline $), so the match ends where it starts.
	referenceDefinitionRe = regexp.MustCompile(`\A {0,3}\[[^\]\n]+\]:[ \t]*(<[^>\n]*>|[^` + jsSpace + `]+)(?:[ \t]+(?:"[^"\n]*"|'[^'\n]*'|\([^)\n]*\)))?[ \t]*(\z|[\n\r\x{2028}\x{2029}])`)
	// <https://…> or <www.…>.
	autolinkRe = regexp.MustCompile(`<((?:[Hh][Tt][Tt][Pp][Ss]?://|[Ww][Ww][Ww]\.)[^` + jsSpace + `<>]*)>`)
	// A scheme as the workspace's hostsAndPaths tests for one.
	schemeSlashesRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
)

// stripFilingLinksDeep returns v (a value decoded from JSON: strings,
// []any, map[string]any, numbers, booleans, nil) with stripFilingLinks
// applied to every string; v itself is not changed.
func stripFilingLinksDeep(v any) any {
	switch x := v.(type) {
	case string:
		return stripFilingLinks(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = stripFilingLinksDeep(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = stripFilingLinksDeep(e)
		}
		return out
	}
	return v
}

// stripFilingDescriptor is an approval descriptor with its filing source
// links removed: the descriptor as JSON carries it (numbers kept as
// written), every string through stripFilingLinks. A descriptor JSON
// cannot carry is returned as it is (no card digest can match it).
func stripFilingDescriptor(descriptor any) any {
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return descriptor
	}
	var v any
	if decodeNumbers(raw, &v) != nil {
		return descriptor
	}
	return stripFilingLinksDeep(v)
}

// stripFilingLinks is text with every filing source link neutralized, in
// the workspace's order: markdown links and images, reference
// definitions, autolinks, then bare addresses.
func stripFilingLinks(text string) string {
	if text == "" {
		return text
	}
	text = replaceMarkdownLinks(text)
	text = replaceReferenceDefinitions(text)
	text = autolinkRe.ReplaceAllStringFunc(text, func(whole string) string {
		if isFilingSourceURL(whole[1 : len(whole)-1]) {
			return removedLinkText
		}
		return whole
	})
	return replaceBareURLs(text)
}

func replaceMarkdownLinks(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range markdownLinkRe.FindAllStringSubmatchIndex(text, -1) {
		if !isFilingSourceURL(unwrapAngle(text[m[6]:m[7]])) {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString(text[m[4]:m[5]]) // the label or alt text
		last = m[1]
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// replaceReferenceDefinitions replaces the reference definitions that point
// at a filing source. A definition starts a line: at the start of text or
// after a JavaScript line terminator (\n, \r, U+2028, U+2029).
func replaceReferenceDefinitions(text string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); i++ {
		if i < last || (text[i] != ' ' && text[i] != '[') || !lineStart(text, i) {
			continue
		}
		m := referenceDefinitionRe.FindStringSubmatchIndex(text[i:])
		if m == nil {
			continue
		}
		end := i + m[4] // the line end is not part of the definition
		if isFilingSourceURL(unwrapAngle(text[i+m[2] : i+m[3]])) {
			b.WriteString(text[last:i])
			b.WriteString(removedLinkText)
			last = end
		}
		i = end - 1
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// lineStart reports whether i starts a line of text in JavaScript's
// multiline sense.
func lineStart(text string, i int) bool {
	if i == 0 {
		return true
	}
	switch text[i-1] {
	case '\n', '\r':
		return true
	}
	return strings.HasSuffix(text[:i], " ") || strings.HasSuffix(text[:i], " ")
}

// replaceBareURLs replaces the bare addresses (what GFM autolinks) that
// point at a filing source: a scheme URL not after an ASCII letter, or a
// www. address not after an ASCII letter or digit, up to white space,
// quotes or angle brackets, less trailing punctuation. These "not after"
// checks are the workspace pattern's lookbehinds, done by hand.
func replaceBareURLs(text string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(text); {
		n := bareURLAt(text, i)
		if n == 0 {
			i++
			continue
		}
		match := text[i : i+n]
		url := strings.TrimRight(match, `.,;:!?*_~'")]`)
		if isFilingSourceURL(url) {
			b.WriteString(text[last:i])
			b.WriteString(removedLinkText)
			b.WriteString(match[len(url):])
			last = i + n
		}
		i += n
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// bareURLAt is the length of the bare address that starts at i (0 when
// none does).
func bareURLAt(text string, i int) int {
	var before byte
	if i > 0 {
		before = text[i-1]
	}
	rest := text[i:]
	start := 0
	switch {
	case !isASCIILetter(before) && hasPrefixFold(rest, "https://"):
		start = len("https://")
	case !isASCIILetter(before) && hasPrefixFold(rest, "http://"):
		start = len("http://")
	case !isASCIILetter(before) && !isASCIIDigit(before) && hasPrefixFold(rest, "www."):
		start = len("www.")
	default:
		return 0
	}
	n := start
	for n < len(rest) {
		r, size := utf8.DecodeRuneInString(rest[n:])
		if isJSSpace(r) || strings.ContainsRune("<>\"'`", r) {
			break
		}
		n += size
	}
	if n == start {
		return 0
	}
	return n
}

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }
func isASCIIDigit(c byte) bool  { return '0' <= c && c <= '9' }

// hasPrefixFold is strings.HasPrefix with ASCII letters compared without
// case (prefix is lower case ASCII).
func hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != prefix[i] {
			return false
		}
	}
	return true
}

// isJSSpace reports JavaScript white space or a line terminator (\s).
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return 0x2000 <= r && r <= 0x200a
}

func unwrapAngle(s string) string {
	if len(s) >= 2 && s[0] == '<' && s[len(s)-1] == '>' {
		return s[1 : len(s)-1]
	}
	return s
}

// ─── reading a link ─────────────────────────────────────────────────────────

// placeholderBase is the base an href is resolved against, as a browser
// resolves a relative link (the workspace's PLACEHOLDER_BASE).
var placeholderBase = &url.URL{Scheme: "https", Host: "placeholder.invalid", Path: "/"}

// specialSchemes are the WHATWG special schemes, whose URLs always have a
// host and treat "\" as "/".
var specialSchemes = map[string]bool{"http": true, "https": true, "ws": true, "wss": true, "ftp": true, "file": true}

// isFilingSourceURL reports whether raw (a URL, with or without a scheme)
// points at a filing source. raw is read three ways, as the workspace's
// hostsAndPaths does: with "https://" added when it has no "scheme://", as
// an absolute URL on its own, and resolved as a browser resolves an href.
// When a reading fails, or Go may read a host differently from a browser
// (see linkReading), the host names raw contains decide, as the workspace
// does when no reading parses.
func isFilingSourceURL(raw string) bool {
	text := strings.TrimFunc(raw, isJSSpace)
	if text == "" {
		return false
	}
	withScheme := text
	if !schemeSlashesRe.MatchString(text) {
		withScheme = "https://" + strings.TrimPrefix(text, "//")
	}
	if nonASCIIHost(text) || nonASCIIHost(withScheme) {
		return true
	}
	loose := false
	for _, r := range []linkReading{readLink(withScheme, false), readLink(text, false), readLink(text, true)} {
		if r.state == readingOK && isFilingHost(r.host, r.path) {
			return true
		}
		if r.state == readingUnsure {
			loose = true
		}
	}
	return loose && namesFilingHost(raw)
}

// nonASCIIHost reports whether the host of URL s, as written (after a
// browser's input cleanup, the scheme and the slashes before the host,
// before the path, query or fragment, without user info), has a non-ASCII
// character or the percent escape of one. A browser maps such a host to
// ASCII by Unicode rules Go does not repeat here, so such a link counts as
// a filing source link.
func nonASCIIHost(s string) bool {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, strings.TrimFunc(s, func(r rune) bool { return r <= ' ' }))
	if _, rest, ok := splitScheme(s); ok {
		s = rest
	}
	s = strings.TrimLeft(s, `/\`)
	if i := strings.IndexAny(s, `/\?#`); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf || s[i] == '%' && i+1 < len(s) && strings.IndexByte("89abcdefABCDEF", s[i+1]) >= 0 {
			return true
		}
	}
	return false
}

// namesFilingHost is the workspace's check for a link that does not parse:
// raw contains a filing source host name (here also after percent
// decoding and folding full width forms).
func namesFilingHost(raw string) bool {
	forms := []string{strings.ToLower(raw)}
	if u, err := url.PathUnescape(raw); err == nil {
		forms = append(forms, foldHost(u))
	}
	forms = append(forms, foldHost(raw))
	for _, s := range forms {
		for _, h := range filingSourceHosts {
			if strings.Contains(s, h) {
				return true
			}
		}
		if strings.Contains(s, gcsHost+"/"+filingBucketPrefix) {
			return true
		}
	}
	return false
}

func isFilingHost(host, path string) bool {
	for _, h := range filingSourceHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	if host == gcsHost {
		return strings.HasPrefix(strings.ToLower(path), "/"+filingBucketPrefix)
	}
	return strings.HasPrefix(host, filingBucketPrefix) && strings.HasSuffix(host, "."+gcsHost)
}

type readingState int

const (
	// readingNone: no URL, as for a browser (no scheme and no base).
	readingNone readingState = iota
	// readingOK: host and path read the way a browser reads them.
	readingOK
	// readingUnsure: Go's reading failed or may differ from a browser's.
	readingUnsure
)

type linkReading struct {
	state readingState
	host  string
	path  string
}

// readLink reads s as a browser's URL parser would (new URL(s), or
// new URL(s, base) with resolve), with net/url: the browser's input
// cleanup (outer C0 controls and spaces trimmed, tabs and newlines
// dropped), "\" read as "/" and any number of slashes before the host of a
// special scheme URL, safe percent escapes decoded, and the path's dot
// segments resolved (URL.ResolveReference). The host is lower case,
// without a port or one trailing dot, with full width forms folded.
func readLink(s string, resolve bool) linkReading {
	s = strings.TrimFunc(s, func(r rune) bool { return r <= ' ' })
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
	scheme, rest, hasScheme := splitScheme(s)
	switch {
	case hasScheme && specialSchemes[strings.ToLower(scheme)]:
		s = scheme + "://" + strings.TrimLeft(slashes(rest), "/")
	case hasScheme:
	case !resolve:
		return linkReading{state: readingNone}
	default:
		// A relative href against an https base: "//host", "\\host" and
		// more slashes all name a host.
		rest = slashes(s)
		if strings.HasPrefix(rest, "//") {
			s = "//" + strings.TrimLeft(rest, "/")
		} else {
			s = rest
		}
	}
	u, err := url.Parse(decodeSafeEscapes(s))
	if err != nil {
		return linkReading{state: readingUnsure}
	}
	u = placeholderBase.ResolveReference(u)
	host := u.Hostname()
	if u.Port() != "" {
		if p, err := strconv.Atoi(u.Port()); err != nil || p > 65535 {
			return linkReading{state: readingUnsure}
		}
	}
	state := readingOK
	if specialSchemes[u.Scheme] && host == "" && u.Scheme != "file" {
		state = readingUnsure
	}
	if !strings.HasPrefix(u.Host, "[") && unsureHost(host) {
		state = readingUnsure
	}
	host = strings.TrimSuffix(foldHost(host), ".")
	return linkReading{state: state, host: host, path: u.Path}
}

// splitScheme splits "scheme:rest" as a URL parser finds a scheme (an ASCII
// letter, then letters, digits, "+", "-" or ".", then ":").
func splitScheme(s string) (scheme, rest string, ok bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isASCIILetter(c):
		case i > 0 && (isASCIIDigit(c) || c == '+' || c == '-' || c == '.'):
		case i > 0 && c == ':':
			return s[:i], s[i+1:], true
		default:
			return "", s, false
		}
	}
	return "", s, false
}

// slashes is s with "\" read as "/" before its query or fragment, as in a
// special scheme URL.
func slashes(s string) string {
	end := strings.IndexAny(s, "?#")
	if end < 0 {
		end = len(s)
	}
	return strings.ReplaceAll(s[:end], `\`, "/") + s[end:]
}

// decodeSafeEscapes decodes the percent escapes of ASCII letters, digits
// and "-", ".", "_", "~", which a browser decodes in a host and which
// cannot change how a URL splits; net/url refuses them in a host.
func decodeSafeEscapes(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				c := byte(v)
				if isASCIILetter(c) || isASCIIDigit(c) || strings.IndexByte("-._~", c) >= 0 {
					b.WriteByte(c)
					i += 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// unsureHost reports a host Go may read differently from a browser: one a
// browser refuses (a forbidden code point, a last label that is a number
// but not an IPv4 address) or one a browser rewrites (non-ASCII, or
// already in punycode).
func unsureHost(host string) bool {
	for i := 0; i < len(host); i++ {
		c := host[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`#%/:<>?@[\]^|`, c) >= 0 {
			return true
		}
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	for _, l := range labels {
		if strings.HasPrefix(strings.ToLower(l), "xn--") {
			return true
		}
	}
	if last := labels[len(labels)-1]; last != "" && numericLabel(last) {
		for _, l := range labels {
			if !numericLabel(l) {
				return true
			}
		}
	}
	return false
}

// numericLabel reports a host label a browser reads as an IPv4 number
// (decimal, or 0x hexadecimal).
func numericLabel(l string) bool {
	if l == "" {
		return false
	}
	if len(l) >= 2 && l[0] == '0' && (l[1] == 'x' || l[1] == 'X') {
		for i := 2; i < len(l); i++ {
			if !isASCIIDigit(l[i]) && !strings.ContainsRune("abcdefABCDEF", rune(l[i])) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(l); i++ {
		if !isASCIIDigit(l[i]) {
			return false
		}
	}
	return true
}

// foldHost lower cases s and folds what a browser's host mapping folds to
// ASCII among full width forms and dots: U+FF01..U+FF5E to ASCII, the
// ideographic and full and half width full stops to ".", and drops the
// code points host mapping ignores (soft hyphen, zero width and variation
// selectors).
func foldHost(s string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		switch {
		case 0xff01 <= r && r <= 0xff5e:
			return r - 0xfee0
		case r == 0x3002 || r == 0xff0e || r == 0xff61:
			return '.'
		case r == 0xad || r == 0x34f || r == 0x2060 || r == 0xfeff || (0x200b <= r && r <= 0x200d) ||
			(0x180b <= r && r <= 0x180f) || (0xfe00 <= r && r <= 0xfe0f):
			return -1
		}
		return r
	}, s))
}
