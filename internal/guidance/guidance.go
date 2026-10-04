// Package guidance serves Mosaic's agent research guidance (Story 78.31):
// the one set of research and answer instructions chat-api renders for every
// agent surface. agent-ui is for hosts where the agent is the UI (the CLI and
// MCP hosts: cite each passage url as a markdown link); mosaic-ui is for
// sessions where Mosaic is the UI (archivist connect, task tokens: cite each
// passage's cite_as). Each comes in a full form (Claude
// --append-system-prompt-file, Codex developerInstructions) and a compact form
// (MCP server instructions).
//
// Fetch takes the live text from chat-api's public GET /agent-guidance, so a
// guidance change ships with a chat-api deploy and no CLI release. Any failure
// falls back to the embedded copy vendored from the mosaic repository's
// reference/schemas/agent-guidance/1 snapshot; vendor-source.json pins its
// source commit and per-file digests.
package guidance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// SchemaVersion is the response contract this package accepts.
const SchemaVersion = "mosaic-agent-guidance/1"

// Surfaces and forms.
const (
	SurfaceAgentUI  = "agent-ui"
	SurfaceMosaicUI = "mosaic-ui"
	FormFull        = "full"
	FormCompact     = "compact"
)

// Where a Text came from.
const (
	SourceLive     = "live"
	SourceEmbedded = "embedded"
)

// FetchTimeout bounds the whole live fetch: session start never waits longer.
const FetchTimeout = 3 * time.Second

// fetchTimeout is FetchTimeout; tests shorten it.
var fetchTimeout = FetchTimeout

// MaxTextBytes caps the guidance text; a larger one is refused.
const MaxTextBytes = 64 << 10

// maxBodyBytes caps the response read: the text plus JSON escaping and fields.
const maxBodyBytes = 4 * MaxTextBytes

//go:embed vendor/1/*.md
var assets embed.FS

//go:embed vendor-source.json
var vendorSource []byte

// Text is one rendered guidance.
type Text struct {
	Body   string
	Digest string // sha256:<hex of Body>
	Source string // SourceLive or SourceEmbedded
	// Reason says why the live text was not used (empty when it was).
	Reason string
}

// Digest is sha256:<hex> over the UTF-8 text, as chat-api computes it.
func Digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func validPair(surface, form string) bool {
	return (surface == SurfaceAgentUI || surface == SurfaceMosaicUI) && (form == FormFull || form == FormCompact)
}

// Embedded returns the vendored text for surface and form. It panics on an
// unknown pair: callers pass the constants above.
func Embedded(surface, form string) Text {
	if !validPair(surface, form) {
		panic(fmt.Sprintf("guidance: unknown surface/form %q/%q", surface, form))
	}
	b, err := assets.ReadFile("vendor/1/" + surface + "." + form + ".md")
	if err != nil {
		panic("guidance: embedded file missing: " + err.Error())
	}
	return Text{Body: string(b), Digest: Digest(string(b)), Source: SourceEmbedded}
}

// VendorSource is vendor-source.json.
type VendorSource struct {
	SchemaVersion string `json:"schemaVersion"`
	Repository    string `json:"repository"`
	SourceCommit  string `json:"sourceCommit"`
	SnapshotPath  string `json:"snapshotPath"`
	VendoredPath  string `json:"vendoredPath"`
	CapturedAt    string `json:"capturedAt"`
	Files         []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// LoadVendorSource parses the embedded vendor-source.json.
func LoadVendorSource() (VendorSource, error) {
	var v VendorSource
	dec := json.NewDecoder(bytes.NewReader(vendorSource))
	dec.DisallowUnknownFields()
	err := dec.Decode(&v)
	return v, err
}

type response struct {
	SchemaVersion *string `json:"schemaVersion"`
	Surface       *string `json:"surface"`
	Form          *string `json:"form"`
	Digest        *string `json:"digest"`
	Text          *string `json:"text"`
}

// httpClient refuses redirects: a moved endpoint falls back like any other
// failure instead of following a Location somewhere else.
var httpClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Fetch returns the live guidance from baseURL (the chat-api base URL), or
// the embedded copy on any failure: timeout, transport error, non-200, a
// wrong schemaVersion, surface or form, a digest mismatch, an empty, too
// large or non UTF-8 text. It never sends credentials and never waits longer
// than FetchTimeout.
func Fetch(ctx context.Context, baseURL, surface, form string) Text {
	live, err := fetchLive(ctx, baseURL, surface, form)
	if err != nil {
		t := Embedded(surface, form)
		t.Reason = err.Error()
		return t
	}
	return live
}

func fetchLive(ctx context.Context, baseURL, surface, form string) (Text, error) {
	if !validPair(surface, form) {
		return Text{}, fmt.Errorf("unknown surface/form %q/%q", surface, form)
	}
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return Text{}, errors.New("invalid chat-api base URL")
	}
	u := base.JoinPath("agent-guidance")
	u.RawQuery = url.Values{"surface": {surface}, "form": {form}}.Encode()
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Text{}, err
	}
	// Unauthenticated on purpose: the route is public, and no bearer means a
	// task token can never reach it.
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Text{}, errors.New("timeout")
		}
		return Text{}, errors.New("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Text{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return Text{}, errors.New("read failed")
	}
	if len(body) > maxBodyBytes {
		return Text{}, errors.New("response too large")
	}
	// encoding/json would replace invalid UTF-8 with U+FFFD; refuse it instead.
	if !utf8.Valid(body) {
		return Text{}, errors.New("text is not UTF-8")
	}
	var r response
	if err := json.Unmarshal(body, &r); err != nil {
		return Text{}, errors.New("invalid JSON")
	}
	switch {
	case r.SchemaVersion == nil || *r.SchemaVersion != SchemaVersion:
		return Text{}, errors.New("wrong schemaVersion")
	case r.Surface == nil || *r.Surface != surface:
		return Text{}, errors.New("wrong surface")
	case r.Form == nil || *r.Form != form:
		return Text{}, errors.New("wrong form")
	case r.Text == nil || *r.Text == "":
		return Text{}, errors.New("empty text")
	case len(*r.Text) > MaxTextBytes:
		return Text{}, errors.New("text too large")
	case !utf8.ValidString(*r.Text):
		return Text{}, errors.New("text is not UTF-8")
	case r.Digest == nil || *r.Digest != Digest(*r.Text):
		return Text{}, errors.New("digest mismatch")
	}
	return Text{Body: *r.Text, Digest: *r.Digest, Source: SourceLive}, nil
}
