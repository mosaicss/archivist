package cmd

// compact.go implements --format compact (Story 81.4): a minified projection
// of a research response that also fits each page to a byte budget, so one
// tool result stays well under an agent host's tool output cut (Codex
// truncates a tool output over its model's 10,000 token budget).
//
// The projection keeps the server's member order (an ordered JSON decode)
// and drops what an agent never needs: null members, truncated:false,
// chunk_index and exchange on passage records, a form code's redundant
// formdescription, and a plain resolved entity_resolution. The fit keeps the
// longest whole prefix of the response's list whose printed size fits
// compactFitBytes. A dropped tail sets truncated:true and a CLI cursor
// "c1.<base64url JSON {s, k, h}>" that wraps the server cursor of the page
// (s), the rows already shown (k) and a hash of the request without its
// cursor (h); the next call refetches that server page and skips k rows.

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

const (
	formatCompact = "compact"
	// compactFitBytes bounds a compact page's printed size (about 6k tokens).
	compactFitBytes = 24000
	// compactCursorPrefix marks a CLI cursor. Server cursors are base64url
	// and never contain a dot.
	compactCursorPrefix = "c1."
	// formCodeMaxChars is the longest formtype read as a form code (10-K, 20-F, 6-K).
	formCodeMaxChars = 12
)

// compactListKeys are the top level lists the fit pages through.
var compactListKeys = map[string]bool{"results": true, "passages": true, "sections": true, "filings": true, "matches": true}

// entityResolutionKeys are the entity_resolution members compact keeps.
var entityResolutionKeys = map[string]bool{
	"state": true, "symbol": true, "company_name": true, "warning": true,
	"suggestion": true, "candidates": true, "alternatives": true,
}

// ─── ordered JSON ────────────────────────────────────────────────────────────

type jsonMember struct {
	Key string
	Val any
}

// jsonObject is a JSON object in document order. Values are jsonObject,
// []any, string, json.Number, bool or nil.
type jsonObject []jsonMember

func (o jsonObject) get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Val, true
		}
	}
	return nil, false
}

// set replaces key's value in place, else inserts it before the member named
// before (when present), else appends it.
func (o jsonObject) set(key string, val any, before string) jsonObject {
	for i, m := range o {
		if m.Key == key {
			o[i].Val = val
			return o
		}
	}
	if before != "" {
		for i, m := range o {
			if m.Key == before {
				out := make(jsonObject, 0, len(o)+1)
				out = append(out, o[:i]...)
				out = append(out, jsonMember{key, val})
				return append(out, o[i:]...)
			}
		}
	}
	return append(o, jsonMember{key, val})
}

// decodeOrdered decodes one JSON value keeping object member order.
func decodeOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := decodeOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := jsonObject{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, errors.New("object key is not a string")
				}
				v, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				obj = append(obj, jsonMember{key, v})
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %q", t)
	default:
		return t, nil
	}
}

// encodeCompact writes v as JSON with no indentation and no HTML escaping.
func encodeCompact(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(x.String())
	case string:
		writeJSONString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeCompact(buf, e)
		}
		buf.WriteByte(']')
	case jsonObject:
		buf.WriteByte('{')
		for i, m := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, m.Key)
			buf.WriteByte(':')
			encodeCompact(buf, m.Val)
		}
		buf.WriteByte('}')
	default:
		// Not produced by decodeOrdered; encode through encoding/json.
		b, _ := json.Marshal(x)
		buf.Write(b)
	}
}

func writeJSONString(buf *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}

// ─── projection ──────────────────────────────────────────────────────────────

// projectCompact returns v without the members compact drops (see the file
// comment). It never mutates v.
func projectCompact(v any) any {
	switch x := v.(type) {
	case jsonObject:
		_, hasFiling := x.get("filing_id")
		_, hasSnippet := x.get("snippet")
		passage := hasFiling && hasSnippet
		dropFormDescription := false
		if passage {
			ft, _ := x.get("formtype")
			dropFormDescription = isFormCode(ft)
		}
		out := make(jsonObject, 0, len(x))
		for _, m := range x {
			switch {
			case m.Val == nil:
				continue
			case m.Key == "truncated" && m.Val == false:
				continue
			case passage && (m.Key == "chunk_index" || m.Key == "exchange"):
				continue
			case dropFormDescription && m.Key == "formdescription":
				continue
			case m.Key == "entity_resolution":
				if er, keep := projectEntityResolution(m.Val); keep {
					out = append(out, jsonMember{m.Key, er})
				}
				continue
			}
			out = append(out, jsonMember{m.Key, projectCompact(m.Val)})
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = projectCompact(e)
		}
		return out
	default:
		return v
	}
}

// isFormCode reports a short form code such as 10-K: at most 12 characters
// and no colon (a KAP formtype is "FR:<id>", a SEDAR one a long numeric code).
func isFormCode(v any) bool {
	s, ok := v.(string)
	return ok && s != "" && jsLen(s) <= formCodeMaxChars && !strings.Contains(s, ":")
}

// projectEntityResolution omits a plain resolved_canonical resolution and
// otherwise keeps only the members an agent acts on, minus nulls and empty
// arrays.
func projectEntityResolution(v any) (any, bool) {
	er, ok := v.(jsonObject)
	if !ok {
		return projectCompact(v), true
	}
	state, _ := er.get("state")
	warning, _ := er.get("warning")
	suggestion, _ := er.get("suggestion")
	if state == "resolved_canonical" && !nonEmptyString(warning) && !nonEmptyString(suggestion) {
		return nil, false
	}
	out := jsonObject{}
	for _, m := range er {
		if !entityResolutionKeys[m.Key] || m.Val == nil {
			continue
		}
		if arr, isArr := m.Val.([]any); isArr && len(arr) == 0 {
			continue
		}
		out = append(out, jsonMember{m.Key, projectCompact(m.Val)})
	}
	return out, true
}

func nonEmptyString(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

// ─── printed size ────────────────────────────────────────────────────────────

// printedSize is the size of text encoded as a JSON string, as a host prints
// a tool's text content inside a CallToolResult: quotes, backslashes and
// control characters are escaped, everything else is its UTF-8 bytes.
func printedSize(text string) int {
	n := 2
	for i := 0; i < len(text); i++ {
		switch b := text[i]; {
		case b == '"' || b == '\\':
			n += 2
		case b == '\n' || b == '\r' || b == '\t' || b == '\b' || b == '\f':
			n += 2
		case b < 0x20:
			n += 6
		default:
			n++
		}
	}
	return n
}

// ─── cursor ──────────────────────────────────────────────────────────────────

// pageCursor is a parsed --cursor: the server cursor to send, the rows of
// that server page already shown, and the request hash a CLI cursor carries.
type pageCursor struct {
	Server string
	Skip   int
	hash   string
}

type cliCursorJSON struct {
	S string `json:"s"`
	K int    `json:"k"`
	H string `json:"h"`
}

// requestHash identifies a request without its cursor: path and sorted query.
func requestHash(pathAndQuery string) string {
	sum := sha256.Sum256([]byte(pathAndQuery))
	return hex.EncodeToString(sum[:8])
}

func encodeCLICursor(server string, skip int, hash string) string {
	b, _ := json.Marshal(cliCursorJSON{S: server, K: skip, H: hash})
	return compactCursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// parsePageCursor reads --cursor for a request whose hash (without cursor)
// is hash. A server cursor passes through. A CLI cursor needs the compact
// format and the same request; anything else is exit 2 before any request.
func parsePageCursor(cmd *cobra.Command, raw, format, hash string) (pageCursor, error) {
	if !strings.HasPrefix(raw, compactCursorPrefix) {
		return pageCursor{Server: raw, hash: hash}, nil
	}
	if format != formatCompact {
		return pageCursor{}, usageErr(cmd, "this cursor came from compact output; rerun with --format compact")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, compactCursorPrefix))
	var c cliCursorJSON
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		err = dec.Decode(&c)
	}
	if err != nil || c.K < 1 || c.H == "" || strings.HasPrefix(c.S, compactCursorPrefix) {
		return pageCursor{}, usageErr(cmd, "the cursor is not valid; repeat the request without --cursor")
	}
	if c.H != hash {
		return pageCursor{}, usageErr(cmd, "the cursor does not belong to this request; repeat it with the same arguments, or drop the cursor")
	}
	return pageCursor{Server: c.S, Skip: c.K, hash: hash}, nil
}

// ─── output ──────────────────────────────────────────────────────────────────

// renderCompact projects root and encodes it with a trailing newline.
func renderCompact(root any) string {
	var buf bytes.Buffer
	encodeCompact(&buf, projectCompact(root))
	buf.WriteByte('\n')
	return buf.String()
}

// compactPage builds the compact text for a server body. listKey names the
// list to fit ("" for no fit, as for read passage). It returns the text and
// the next cursor it carries ("" for none).
func compactPage(body []byte, listKey string, cur pageCursor) (string, string, error) {
	root, err := decodeOrdered(bytes.TrimSpace(body))
	if err != nil {
		return "", "", err
	}
	obj, isObj := root.(jsonObject)
	if !isObj || listKey == "" || !compactListKeys[listKey] {
		return renderCompact(root), nextCursorOf(root), nil
	}
	listVal, _ := obj.get(listKey)
	rows, isArr := listVal.([]any)
	if !isArr {
		return renderCompact(obj), nextCursorOf(obj), nil
	}
	if cur.Skip > 0 {
		if cur.Skip >= len(rows) {
			rows = []any{}
		} else {
			rows = rows[cur.Skip:]
		}
	}
	whole := obj.set(listKey, rows, "")
	if text := renderCompact(whole); len(rows) <= 1 || printedSize(text) <= compactFitBytes {
		return text, nextCursorOf(whole), nil
	}
	// Longest whole prefix that fits; a first row alone over the budget is
	// returned whole and the cursor still advances (never clip silently).
	for k := len(rows) - 1; k >= 1; k-- {
		next := encodeCLICursor(cur.Server, cur.Skip+k, cur.hash)
		cut := append(jsonObject(nil), obj...)
		cut = cut.set(listKey, rows[:k], "")
		cut = cut.set("truncated", true, "next_cursor")
		cut = cut.set("next_cursor", next, "")
		text := renderCompact(cut)
		if k == 1 || printedSize(text) <= compactFitBytes {
			return text, next, nil
		}
	}
	return "", "", errors.New("unreachable")
}

func nextCursorOf(v any) string {
	obj, ok := v.(jsonObject)
	if !ok {
		return ""
	}
	s, _ := obj.get("next_cursor")
	str, _ := s.(string)
	return str
}

// emitCompact writes the compact page for body and prints the continuation
// hint. A malformed body is exit 5.
func emitCompact(cmd *cobra.Command, body []byte, listKey string, cur pageCursor) error {
	text, next, err := compactPage(body, listKey, cur)
	if err != nil {
		return reportFailure(cmd, failure{
			exitCode: ExitServerError, code: "UNREADABLE_RESPONSE",
			message: fmt.Sprintf("could not parse the response: %v", err),
		}, formatCompact)
	}
	if _, err := io.WriteString(cmd.OutOrStdout(), text); err != nil {
		return err
	}
	if next != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "More results: rerun with --cursor %s\n", next)
	}
	return nil
}

// writeCompactJSON writes a CLI built JSON value in the compact form
// (projection, no fit): used by the companies verbs.
func writeCompactJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	root, err := decodeOrdered(b)
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, renderCompact(root))
	return err
}

// hasLetterOrDigit reports whether s holds a Unicode letter or number
// (JavaScript's \p{L} and \p{N}).
func hasLetterOrDigit(s string) bool {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return true
		}
		s = s[size:]
	}
	return false
}
