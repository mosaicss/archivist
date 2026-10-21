package connect

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Commands from the browser are signed (Story 94.7). Each Mosaic workspace
// browser holds its own ECDSA P-256 key; the commands that let an agent do
// something (start_session, user_message, set_mode, an approval_response
// that allows, trust_browser) carry a `sig` the browser made, and a user
// daemon runs them only when the key is one this computer trusts
// (browsers.go) and the signed payload names this very command. Stopping,
// interrupting and denying need no signature: they only reduce what runs.
// The session-bound sandbox mode never checks signatures.
//
//	sig = {"v":1, "key":b64u(SPKI DER), "payload":b64u(UTF-8 JSON), "sig":b64u(r||s)}
//
// The signature is ECDSA P-256 with SHA-256 over the decoded payload bytes
// (WebCrypto's {name:"ECDSA", hash:"SHA-256"}, IEEE P1363 r||s form).

// Envelope limits (base64url characters).
const (
	maxSigKeyChars     = 200
	maxSigPayloadChars = 2048
	sigChars           = 86 // 64 bytes
	digestChars        = 43 // 32 bytes
	// maxPromptPrefix bounds the context blocks the workspace may put in
	// front of the signed text (prompt binding); a filter block may list
	// many filing types.
	maxPromptPrefix = 16384
	// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER.
	maxSafeInteger = 1<<53 - 1
)

var b64uRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// b64u is base64url without padding.
func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// sha256b64u is b64u(SHA-256(b)): the text and card digests and a browser's
// fingerprint.
func sha256b64u(b []byte) string {
	sum := sha256.Sum256(b)
	return b64u(sum[:])
}

// BrowserFingerprint is a browser key's fingerprint, b64u(SHA-256(SPKI
// DER)); it is also the browser code a user copies.
func BrowserFingerprint(spki []byte) string { return sha256b64u(spki) }

// ValidBrowserCode reports a well formed browser fingerprint: 43 base64url
// characters that decode to 32 bytes.
func ValidBrowserCode(s string) bool {
	return validDigest(s)
}

func validDigest(s string) bool {
	if len(s) != digestChars || !b64uRe.MatchString(s) {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

// sigEnvelope is a decoded `sig` field.
type sigEnvelope struct {
	key     []byte
	payload []byte
	sig     []byte
}

// signedPayload is a strictly decoded signed payload. has records which
// optional fields were present.
type signedPayload struct {
	Kind       string
	Counter    int64
	Iat        int64
	Agent      string
	TextSha256 string
	SessionID  string
	Mode       string
	ApprovalID string
	Decision   string
	Scope      string
	CardSha256 string
	Browser    string
	has        map[string]bool
}

// payloadShapes are the fields of each payload kind beyond v, kind,
// counter and iat.
var payloadShapes = map[string]shape{
	"turn":              {required: []string{"agent", "textSha256"}, optional: []string{"sessionId", "mode"}},
	"user_message":      {required: []string{"sessionId", "textSha256"}},
	"set_mode":          {required: []string{"sessionId", "mode"}},
	"approval_response": {required: []string{"sessionId", "approvalId", "decision", "scope", "cardSha256"}},
	"trust_browser":     {required: []string{"browser"}},
}

// approvalScopes are the scopes a signed approval may name ("" = none).
var approvalScopes = map[string]bool{"": true, "allow_once": true, "allow_always": true, "reject_once": true, "reject_always": true}

// signFailure is why a signed command was not run. unknown means the
// command did not come from a browser this computer trusts (no signature,
// an unreadable one, or a key not on the list); otherwise a trusted browser
// signed something that does not match this command. store means the
// browser records could not be read.
type signFailure struct {
	reason  string
	unknown bool
	store   bool
}

func (f *signFailure) Error() string { return f.reason }

// User facing refusal messages (the session's error chunk).
const (
	signUnknownMessage  = "This request was not run because it did not come from a browser this computer knows. In Mosaic, open Connect your computer to add this browser."
	signMismatchMessage = "This request was not run because it did not match what this browser sent. Try again."
	signStoreMessage    = "This request was not run because archivist connect could not read its list of browsers. Restart archivist connect and try again."
)

func (f *signFailure) message() string {
	switch {
	case f.store:
		return signStoreMessage
	case f.unknown:
		return signUnknownMessage
	}
	return signMismatchMessage
}

// Tool results for an allow that was not run: the tool call is denied
// instead, since the relay has already recorded the answer.
const (
	denyUnknownMessage  = "Denied: this approval did not come from a browser this computer knows."
	denyMismatchMessage = "Denied: this approval did not match what this browser sent."
	denyStoreMessage    = "Denied: archivist connect could not read its list of browsers to check this approval."
)

// denyMessage is the tool result of an allow refused for f.
func (f *signFailure) denyMessage() string {
	switch {
	case f.store:
		return denyStoreMessage
	case f.unknown:
		return denyUnknownMessage
	}
	return denyMismatchMessage
}

func unknownBrowser(format string, args ...any) *signFailure {
	return &signFailure{reason: fmt.Sprintf(format, args...), unknown: true}
}

func mismatch(format string, args ...any) *signFailure {
	return &signFailure{reason: fmt.Sprintf(format, args...)}
}

// decodeSigEnvelope checks the envelope shape: exactly {v, key, payload,
// sig}, v the number 1, the three strings base64url within their limits.
func decodeSigEnvelope(raw json.RawMessage) (*sigEnvelope, error) {
	fields, err := strictObject(raw)
	if err != nil {
		return nil, fmt.Errorf("signature is not a JSON object: %w", err)
	}
	if reason := exactFields(fields, shape{required: []string{"v", "key", "payload", "sig"}}); reason != "" {
		return nil, errors.New("signature " + reason)
	}
	if string(fields["v"]) != "1" {
		return nil, errors.New("signature version is not 1")
	}
	var key, payload, sig string
	for _, f := range []struct {
		name string
		dst  *string
		max  int
	}{{"key", &key, maxSigKeyChars}, {"payload", &payload, maxSigPayloadChars}, {"sig", &sig, sigChars}} {
		if json.Unmarshal(fields[f.name], f.dst) != nil || *f.dst == "" || len(*f.dst) > f.max || !b64uRe.MatchString(*f.dst) {
			return nil, errors.New("signature field " + f.name + " is not base64url within its limit")
		}
	}
	if len(sig) != sigChars {
		return nil, errors.New("signature field sig is not 64 bytes")
	}
	env := &sigEnvelope{}
	for _, f := range []struct {
		s   string
		dst *[]byte
	}{{key, &env.key}, {payload, &env.payload}, {sig, &env.sig}} {
		b, err := base64.RawURLEncoding.Strict().DecodeString(f.s)
		if err != nil {
			return nil, errors.New("signature is not base64url")
		}
		*f.dst = b
	}
	if len(env.sig) != 64 {
		return nil, errors.New("signature field sig is not 64 bytes")
	}
	return env, nil
}

// verifySignature checks that key is an ECDSA P-256 SubjectPublicKeyInfo
// and sig a valid P1363 signature by it over payload.
func verifySignature(key, payload, sig []byte) error {
	pub, err := x509.ParsePKIXPublicKey(key)
	if err != nil {
		return fmt.Errorf("key is not a public key: %w", err)
	}
	ec, ok := pub.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return errors.New("key is not an ECDSA P-256 key")
	}
	if len(sig) != 64 {
		return errors.New("signature is not 64 bytes")
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	digest := sha256.Sum256(payload)
	if !ecdsa.Verify(ec, digest[:], r, s) {
		return errors.New("signature does not verify")
	}
	return nil
}

// decodeSignedPayload strictly decodes a signed payload: a UTF-8 JSON
// object with exactly the fields of its kind, no duplicates and no nulls.
func decodeSignedPayload(b []byte) (*signedPayload, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("payload is not UTF-8")
	}
	fields, err := strictObject(b)
	if err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}
	var kind string
	if json.Unmarshal(fields["kind"], &kind) != nil {
		return nil, errors.New("payload kind is missing")
	}
	sh, ok := payloadShapes[kind]
	if !ok {
		return nil, fmt.Errorf("payload kind %q is unknown", kind)
	}
	full := shape{required: append([]string{"v", "kind", "counter", "iat"}, sh.required...), optional: sh.optional}
	if reason := exactFields(fields, full); reason != "" {
		return nil, errors.New("payload " + reason)
	}
	if string(fields["v"]) != "1" {
		return nil, errors.New("payload version is not 1")
	}
	p := &signedPayload{Kind: kind, has: map[string]bool{}}
	var ok1, ok2 bool
	if p.Counter, ok1 = safeInteger(fields["counter"]); !ok1 || p.Counter < 1 {
		return nil, errors.New("payload counter is not a positive safe integer")
	}
	if p.Iat, ok2 = safeInteger(fields["iat"]); !ok2 {
		return nil, errors.New("payload iat is not a safe integer")
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{"agent", &p.Agent}, {"textSha256", &p.TextSha256}, {"sessionId", &p.SessionID}, {"mode", &p.Mode},
		{"approvalId", &p.ApprovalID}, {"decision", &p.Decision}, {"scope", &p.Scope}, {"cardSha256", &p.CardSha256},
		{"browser", &p.Browser},
	} {
		raw, present := fields[f.name]
		if !present {
			continue
		}
		if json.Unmarshal(raw, f.dst) != nil {
			return nil, errors.New("payload " + f.name + " is not a string")
		}
		p.has[f.name] = true
	}
	switch {
	case p.has["agent"] && p.Agent != "claude" && p.Agent != "codex":
		return nil, errors.New("payload agent is not claude or codex")
	case p.has["textSha256"] && !validDigest(p.TextSha256):
		return nil, errors.New("payload textSha256 is not a digest")
	case p.has["cardSha256"] && !validDigest(p.CardSha256):
		return nil, errors.New("payload cardSha256 is not a digest")
	case p.has["browser"] && !validDigest(p.Browser):
		return nil, errors.New("payload browser is not a browser code")
	case p.has["sessionId"] && !uuidRe.MatchString(p.SessionID):
		return nil, errors.New("payload sessionId is not a session UUID")
	case p.has["decision"] && p.Decision != "allow" && p.Decision != "deny":
		return nil, errors.New("payload decision is not allow or deny")
	case p.has["scope"] && !approvalScopes[p.Scope]:
		return nil, errors.New("payload scope is not a scope")
	case p.has["approvalId"] && (p.ApprovalID == "" || len(p.ApprovalID) > 400):
		return nil, errors.New("payload approvalId is not an approval id")
	}
	if p.has["mode"] {
		if _, ok := ParseMode(p.Mode); !ok {
			return nil, errors.New("payload mode is not a mode")
		}
	}
	return p, nil
}

// safeInteger reads a JSON integer literal in 0..2^53-1 (no sign,
// fraction, exponent or leading zero).
func safeInteger(raw json.RawMessage) (int64, bool) {
	s := string(raw)
	if s == "" || len(s) > 16 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > maxSafeInteger {
		return 0, false
	}
	return n, true
}

// strictObject decodes one JSON object into its raw fields, refusing
// duplicate keys and trailing data.
func strictObject(b []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("duplicate field %s", key)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		fields[key] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return fields, nil
}

// ─── bindings ───────────────────────────────────────────────────────────────

// textMatches reports whether text is the signed text (textSha256), or the
// signed text after the workspace's context blocks: one or more
// "[Search filters: …]" or "[Attached filing: …]" blocks joined by a blank
// line, at most maxPromptPrefix bytes, then a blank line.
func textMatches(text, textSha256 string) bool {
	if sha256b64u([]byte(text)) == textSha256 {
		return true
	}
	for i := 0; i <= maxPromptPrefix && i+2 <= len(text); i++ {
		j := strings.Index(text[i:], "\n\n")
		if j < 0 || i+j > maxPromptPrefix {
			return false
		}
		i += j
		if contextPrefix(text[:i]) && sha256b64u([]byte(text[i+2:])) == textSha256 {
			return true
		}
	}
	return false
}

// contextPrefix reports a run of workspace context blocks joined by "\n\n".
// A block may hold single line breaks (a filter block lists several filing
// types one per line).
func contextPrefix(prefix string) bool {
	if prefix == "" {
		return false
	}
	for _, block := range strings.Split(prefix, "\n\n") {
		if !strings.HasSuffix(block, "]") ||
			(!strings.HasPrefix(block, "[Search filters: ") && !strings.HasPrefix(block, "[Attached filing: ")) {
			return false
		}
	}
	return true
}

// ─── card digest (RFC 8785) ─────────────────────────────────────────────────

// cardDigest is b64u(SHA-256(JCS(descriptor))) for an approval descriptor as
// the workspace receives it: the descriptor is encoded as the relay event
// carries it and read back, so the digest covers exactly what was sent.
func cardDigest(descriptor any) (string, error) {
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	var v any
	if err := decodeNumbers(raw, &v); err != nil {
		return "", err
	}
	canon, err := canonicalJSON(v)
	if err != nil {
		return "", err
	}
	return sha256b64u(canon), nil
}

// canonicalJSON is the RFC 8785 JSON Canonicalization Scheme form of v (a
// value decoded from JSON, numbers as json.Number or float64): object keys
// sorted by UTF-16 code units, strings escaped as JSON.stringify does and
// numbers written as ECMAScript's Number::toString.
func canonicalJSON(v any) ([]byte, error) {
	return appendCanonical(nil, v)
}

func appendCanonical(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		return strconv.AppendBool(b, x), nil
	case string:
		return appendJSString(b, x), nil
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, fmt.Errorf("number %q: %w", x, err)
		}
		return appendJSNumber(b, f)
	case float64:
		return appendJSNumber(b, x)
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = appendCanonical(b, e); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendJSString(b, k)
			b = append(b, ':')
			var err error
			if b, err = appendCanonical(b, x[k]); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	}
	return nil, fmt.Errorf("value of type %T has no JSON form", v)
}

// compareUTF16 orders strings by their UTF-16 code units (RFC 8785 3.2.3).
func compareUTF16(a, b string) int {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	return slices.Compare(ua, ub)
}

// appendJSString writes s as JSON.stringify does: only '"', '\\' and
// control characters below U+0020 are escaped (\b \f \n \r \t, else
// \u00xx in lower case hex); '<', '>', '&', U+2028 and U+2029 are not.
func appendJSString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			b = utf8.AppendRune(b, r) // invalid bytes become U+FFFD, as encoding/json does
			i += size
			continue
		}
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if c < 0x20 {
				b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
			} else {
				b = append(b, c)
			}
		}
		i++
	}
	return append(b, '"')
}

// appendJSNumber writes f as ECMAScript's Number::toString (RFC 8785
// 3.2.2.3): the shortest round trip digits, -0 as 0, plain notation for
// exponents 1e-7 < |f| < 1e21 and e+/e- notation outside it. NaN and the
// infinities have no JSON form.
func appendJSNumber(b []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, errors.New("number is not finite")
	}
	if f == 0 {
		return append(b, '0'), nil
	}
	if f < 0 {
		b = append(b, '-')
		f = -f
	}
	// d.ddddde±x: digits and the decimal exponent of the first digit.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	x, _ := strconv.Atoi(expStr)
	k, n := len(digits), x+1 // f = 0.digits × 10^n
	switch {
	case k <= n && n <= 21:
		b = append(b, digits...)
		b = append(b, strings.Repeat("0", n-k)...)
	case 0 < n && n <= 21:
		b = append(b, digits[:n]...)
		b = append(b, '.')
		b = append(b, digits[n:]...)
	case -6 < n && n <= 0:
		b = append(b, "0."...)
		b = append(b, strings.Repeat("0", -n)...)
		b = append(b, digits...)
	default:
		b = append(b, digits[0])
		if k > 1 {
			b = append(b, '.')
			b = append(b, digits[1:]...)
		}
		b = append(b, 'e')
		if n-1 >= 0 {
			b = append(b, '+')
		}
		b = strconv.AppendInt(b, int64(n-1), 10)
	}
	return b, nil
}

// ─── daemon checks ──────────────────────────────────────────────────────────

// signedCommand is a command whose signature passed: its browser, payload
// and the counter reserved for it.
type signedCommand struct {
	fp      string
	payload *signedPayload
	ticket  *counterTicket
}

// shortFP is a fingerprint prefix for log lines.
func shortFP(fp string) string {
	if len(fp) > 8 {
		return fp[:8]
	}
	return fp
}

// verifyCommand checks the signature of an allow direction command: a
// trusted browser key, a valid signature, a strictly decoded payload that
// names this command (bind returns "" when it does, else why not) and a
// fresh counter, which stays reserved until commitSigned or releaseSigned.
func (d *Daemon) verifyCommand(in *Inbound, bind func(p *signedPayload) string) (*signedCommand, *signFailure) {
	if len(in.Sig) == 0 {
		return nil, unknownBrowser("not signed")
	}
	env, err := decodeSigEnvelope(in.Sig)
	if err != nil {
		return nil, unknownBrowser("%v", err)
	}
	fp := BrowserFingerprint(env.key)
	list, err := LoadBrowsers(d.store.Dir())
	if err != nil {
		return nil, &signFailure{reason: "trusted browsers unreadable: " + err.Error(), store: true}
	}
	if !list.Trusts(fp) {
		return nil, unknownBrowser("browser fp:%s is not trusted here", shortFP(fp))
	}
	if err := verifySignature(env.key, env.payload, env.sig); err != nil {
		return nil, mismatch("browser fp:%s: %v", shortFP(fp), err)
	}
	p, err := decodeSignedPayload(env.payload)
	if err != nil {
		return nil, mismatch("browser fp:%s: %v", shortFP(fp), err)
	}
	if why := bind(p); why != "" {
		return nil, mismatch("browser fp:%s: signed %s does not match: %s", shortFP(fp), p.Kind, why)
	}
	t, f := d.counters.reserve(fp, p.Counter)
	if f != nil {
		f.reason = fmt.Sprintf("browser fp:%s: %s", shortFP(fp), f.reason)
		return nil, f
	}
	return &signedCommand{fp: fp, payload: p, ticket: t}, nil
}

// commitSigned records the counter of a command that ran.
func (d *Daemon) commitSigned(sc *signedCommand) {
	if sc == nil {
		return
	}
	if err := d.counters.commit(sc.ticket); err != nil {
		d.log.Printf("browser counter %d (fp:%s) not saved: %v; it stays used until archivist connect restarts", sc.payload.Counter, shortFP(sc.fp), err)
	}
}

// releaseSigned gives back the counter of a command that did not run.
func (d *Daemon) releaseSigned(sc *signedCommand) {
	if sc != nil {
		d.counters.release(sc.ticket)
	}
}

// bindStart: a turn for this agent and prompt. The session id is not bound
// (the session is new); the signed mode caps the start's mode (startMode).
func bindStart(in *Inbound) func(p *signedPayload) string {
	return func(p *signedPayload) string {
		switch {
		case p.Kind != "turn":
			return "kind"
		case p.Agent != in.Agent:
			return "agent"
		case !textMatches(in.Prompt, p.TextSha256):
			return "text"
		}
		return ""
	}
}

// bindMessage: a turn for this session, its agent and this text, or a
// user_message (a sign in code) for this session with exactly this text.
func bindMessage(sessionID, agent string, in *Inbound) func(p *signedPayload) string {
	return func(p *signedPayload) string {
		switch p.Kind {
		case "turn":
			switch {
			case !p.has["sessionId"] || p.SessionID != sessionID:
				return "session"
			case p.Agent != agent:
				return "agent"
			case !textMatches(in.Text, p.TextSha256):
				return "text"
			}
			return ""
		case "user_message":
			switch {
			case p.SessionID != sessionID:
				return "session"
			case sha256b64u([]byte(in.Text)) != p.TextSha256:
				return "text"
			}
			return ""
		}
		return "kind"
	}
}

// bindMode: set_mode for this session and mode.
func bindMode(sessionID string, in *Inbound) func(p *signedPayload) string {
	return func(p *signedPayload) string {
		switch {
		case p.Kind != "set_mode":
			return "kind"
		case p.SessionID != sessionID:
			return "session"
		case p.Mode != in.Mode:
			return "mode"
		}
		return ""
	}
}

// bindApproval: this answer (approval, decision, scope) to this session's
// card, whose digest the daemon computed when it sent the card.
func bindApproval(sessionID, cardSha256 string, in *Inbound) func(p *signedPayload) string {
	return func(p *signedPayload) string {
		switch {
		case p.Kind != "approval_response":
			return "kind"
		case p.SessionID != sessionID:
			return "session"
		case p.ApprovalID != in.ApprovalID:
			return "approval"
		case p.Decision != in.Decision:
			return "decision"
		case p.Scope != in.Scope:
			return "scope"
		case cardSha256 == "" || p.CardSha256 != cardSha256:
			return "card"
		}
		return ""
	}
}

// bindTrust: trust_browser for this browser.
func bindTrust(in *Inbound) func(p *signedPayload) string {
	return func(p *signedPayload) string {
		switch {
		case p.Kind != "trust_browser":
			return "kind"
		case p.Browser != in.Browser:
			return "browser"
		}
		return ""
	}
}

// ─── signing frame ──────────────────────────────────────────────────────────

// signingFrame tells the relay that this daemon checks signatures and which
// browsers it trusts: {"kind":"signing","v":1,"browsers":[fp...]}.
func signingFrame(browsers []string) []byte {
	if browsers == nil {
		browsers = []string{}
	}
	b, _ := json.Marshal(struct {
		Kind     string   `json:"kind"`
		V        int      `json:"v"`
		Browsers []string `json:"browsers"`
	}{"signing", 1, browsers})
	return b
}

// announceSigning sends the signing frame on l when l is a new socket or
// the trusted list changed since the last announcement (checked at each
// ping, and after a trust_browser). An unreadable list announces none.
func (d *Daemon) announceSigning(l *live) {
	var fps []string
	list, err := LoadBrowsers(d.store.Dir())
	state := ""
	if err != nil {
		state = "error: " + err.Error()
	} else {
		fps = list.Fingerprints()
		if len(fps) > MaxBrowsers {
			fps = fps[len(fps)-MaxBrowsers:]
		}
		state = strings.Join(fps, ",")
	}
	d.signingMu.Lock()
	if d.signingLive == l && d.signingSent == state {
		d.signingMu.Unlock()
		return
	}
	d.signingLive, d.signingSent = l, state
	d.signingMu.Unlock()
	if err != nil {
		d.log.Printf("trusted browsers unreadable, none announced: %v", err)
	}
	sent := d.signingWin.arm()
	if !l.Send(signingFrame(fps)) {
		d.signingWin.disarm(sent)
		// Not sent: forget it, so the next ping announces the list again.
		d.signingMu.Lock()
		if d.signingLive == l && d.signingSent == state {
			d.signingLive, d.signingSent = nil, ""
		}
		d.signingMu.Unlock()
		return
	}
	d.log.Printf("trusted browsers announced: %d", len(fps))
}

// trustBrowser runs a trust_browser command (user socket): a trusted
// browser adds another browser to this computer's list. A browser already
// on the list is acknowledged only.
func (d *Daemon) trustBrowser(l *live, in *Inbound) {
	ack := func() { l.Send(commandAck(UserSocket, in.CorrelationID, "")) }
	if list, err := LoadBrowsers(d.store.Dir()); err == nil && list.Trusts(in.Browser) {
		d.log.Printf("trust_browser %s: browser fp:%s is already trusted", in.CorrelationID, shortFP(in.Browser))
		ack()
		return
	}
	sc, f := d.verifyCommand(in, bindTrust(in))
	if f != nil {
		d.log.Printf("trust_browser %s not run: %s", in.CorrelationID, f.reason)
		ack()
		return
	}
	if _, err := TrustBrowser(d.store.Dir(), in.Browser, ViaBrowser); err != nil {
		// Not acknowledged: the relay redelivers it.
		d.releaseSigned(sc)
		d.log.Printf("trust_browser %s: browser list not saved, awaiting redelivery: %v", in.CorrelationID, err)
		return
	}
	d.commitSigned(sc)
	d.log.Printf("browser fp:%s trusted (added by browser fp:%s)", shortFP(in.Browser), shortFP(sc.fp))
	d.announceSigning(l)
	ack()
}

// signedPayloadOf is sc's payload (nil for none).
func signedPayloadOf(sc *signedCommand) *signedPayload {
	if sc == nil {
		return nil
	}
	return sc.payload
}
