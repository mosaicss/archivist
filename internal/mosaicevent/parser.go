// Package mosaicevent validates mosaic-event/1 and mosaic-event/2 before decoding the supported
// Grafana wire types. Each version has its own vendored bundle and parser; a Set dispatches on
// the input's schemaVersion.
package mosaicevent

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	aisdk "github.com/grafana/ai-sdk"
)

// Version is mosaic-event/1: New, FromBundle and VerifyBundle keep meaning this version.
const Version = "mosaic-event/1"

// Version2 adds the optional data-auth-prompt code and expiresAt, the session status lost and
// relay-origin data-session-status envelopes.
const Version2 = "mosaic-event/2"

// Explicit patterns keep installed Node dependencies and generated files out of the binary.
//
//go:embed vendor/1/*.json vendor/1/*.sha256 vendor/1/*.md vendor/1/fixtures/*.json vendor/1/fixtures/raw/* vendor/1/contract/*.ts vendor/1/contract/*.json vendor/1/contract/.npmrc
var assets embed.FS

// The v2 bundle has no contract package: schemas, corpus, fixtures, provenance and stamp.mjs.
//
//go:embed vendor/2/*.json vendor/2/*.sha256 vendor/2/*.md vendor/2/*.mjs vendor/2/fixtures/*.json
var assetsV2 embed.FS

// bundles maps each supported version to its embedded vendored bundle.
var bundles = map[string]struct {
	fs  embed.FS
	dir string
}{
	Version:  {assets, "vendor/1"},
	Version2: {assetsV2, "vendor/2"},
}

// Parsed retains exact input bytes: SDK serialization omits some optional v7 fields.
type Parsed struct {
	// Version is the contract version that accepted the input.
	Version    string
	Raw        json.RawMessage
	Chunk      *aisdk.UIMessageChunk
	Origin     string
	Resolution json.RawMessage
}

// Parser validates one contract version from its own bundle.
type Parser struct {
	bundle   fs.FS
	chunk    *jsonschema.Resolved
	envelope *jsonschema.Resolved
	Digest   string
	// Version is the bundle's schemaVersion.
	Version string
}

// New returns the mosaic-event/1 parser.
func New() (*Parser, error) {
	return NewVersion(Version)
}

// NewVersion returns the parser of a supported version from its embedded bundle.
func NewVersion(version string) (*Parser, error) {
	b, ok := bundles[version]
	if !ok {
		return nil, fmt.Errorf("unsupported contract version %q", version)
	}
	bundle, err := fs.Sub(b.fs, b.dir)
	if err != nil {
		return nil, err
	}
	return FromBundleVersion(bundle, version)
}

// FromBundle is also used by drift tests; schema references can only resolve from this bundle.
// It accepts only a mosaic-event/1 bundle.
func FromBundle(bundle fs.FS) (*Parser, error) {
	return FromBundleVersion(bundle, Version)
}

// FromBundleVersion verifies bundle as the given version (manifest, inventory, bytes and schema
// identifiers) and compiles its chunk and envelope schemas.
func FromBundleVersion(bundle fs.FS, version string) (*Parser, error) {
	if _, ok := bundles[version]; !ok {
		return nil, fmt.Errorf("unsupported contract version %q", version)
	}
	digest, err := VerifyBundleVersion(bundle, version)
	if err != nil {
		return nil, err
	}
	p := &Parser{bundle: bundle, Digest: digest, Version: version}
	// Schema identifiers belong to the canonical bundle, not CLI endpoint constants.
	rootBytes, err := fs.ReadFile(bundle, "chunk.json")
	if err != nil {
		return nil, err
	}
	var header struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(rootBytes, &header); err != nil {
		return nil, err
	}
	rootURI, err := url.Parse(header.ID)
	if err != nil || rootURI.Scheme != "https" || rootURI.Host == "" ||
		!strings.HasSuffix(rootURI.Path, "/"+version+"/chunk.json") {
		return nil, fmt.Errorf("invalid canonical schema identifier")
	}
	schemaBase := rootURI.ResolveReference(&url.URL{Path: "."}).String()
	load := func(uri *url.URL) (*jsonschema.Schema, error) {
		if uri.RawQuery != "" || uri.Fragment != "" || !strings.HasPrefix(uri.String(), schemaBase) {
			return nil, fmt.Errorf("nonlocal schema reference %s", uri)
		}
		name := strings.TrimPrefix(uri.String(), schemaBase)
		if path.Base(name) != name || !strings.HasSuffix(name, ".json") {
			return nil, fmt.Errorf("invalid schema reference %s", uri)
		}
		data, err := fs.ReadFile(bundle, name)
		if err != nil {
			return nil, err
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(data, &schema); err != nil {
			return nil, err
		}
		return &schema, nil
	}
	for _, target := range []struct {
		name string
		out  **jsonschema.Resolved
	}{{"chunk.json", &p.chunk}, {"envelope.json", &p.envelope}} {
		uri, err := url.Parse(schemaBase + target.name)
		if err != nil {
			return nil, err
		}
		schema, err := load(uri)
		if err != nil {
			return nil, err
		}
		*target.out, err = schema.Resolve(&jsonschema.ResolveOptions{Loader: load})
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Parser) Parse(raw []byte, kind string) (*Parsed, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	schema := p.chunk
	if kind == "envelope" {
		schema = p.envelope
	} else if kind != "chunk" {
		return nil, fmt.Errorf("unknown contract kind %q", kind)
	}
	if err := schema.Validate(value); err != nil {
		return nil, err
	}
	parsed := &Parsed{Version: p.Version, Raw: append(json.RawMessage(nil), raw...)}
	chunk := raw
	if kind == "envelope" {
		var envelope struct {
			Type          string          `json:"type"`
			Origin        string          `json:"origin"`
			Payload       json.RawMessage `json:"payload"`
			CorrelationID string          `json:"correlationId"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		parsed.Origin = envelope.Origin
		if envelope.Type == "approval_resolved" {
			var resolution struct {
				CorrelationID string `json:"correlationId"`
			}
			if err := json.Unmarshal(envelope.Payload, &resolution); err != nil {
				return nil, err
			}
			if envelope.CorrelationID != "resolved:"+resolution.CorrelationID {
				return nil, fmt.Errorf("resolution correlation mismatch")
			}
			parsed.Resolution = envelope.Payload
			return parsed, nil
		}
		chunk = envelope.Payload
	}
	var typed aisdk.UIMessageChunk
	if err := json.Unmarshal(chunk, &typed); err != nil {
		return nil, err
	}
	parsed.Chunk = &typed
	return parsed, nil
}

type Case struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Valid bool   `json:"valid"`
	Input string `json:"input"`
}

type Verdict struct {
	ID         string `json:"id"`
	Accepted   bool   `json:"accepted"`
	EventCount int    `json:"eventCount"`
}

type Report struct {
	SchemaVersion string    `json:"schemaVersion"`
	BundleDigest  string    `json:"bundleDigest"`
	Cases         []Verdict `json:"cases"`
}

func (p *Parser) Report() (*Report, error) {
	data, err := fs.ReadFile(p.bundle, "cases.json")
	if err != nil {
		return nil, err
	}
	var corpus struct {
		Version string `json:"schemaVersion"`
		Cases   []Case `json:"cases"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil {
		return nil, err
	}
	if corpus.Version != p.Version || len(corpus.Cases) == 0 {
		return nil, fmt.Errorf("invalid corpus version/count")
	}
	seen := map[string]bool{}
	valid, invalid := false, false
	for _, c := range corpus.Cases {
		if c.ID == "" || seen[c.ID] || !fs.ValidPath(c.Input) {
			return nil, fmt.Errorf("invalid/duplicate fixture %s", c.ID)
		}
		if c.Kind != "chunk" && c.Kind != "envelope" {
			return nil, fmt.Errorf("invalid corpus kind: %s", c.ID)
		}
		seen[c.ID] = true
		valid = valid || c.Valid
		invalid = invalid || !c.Valid
	}
	if !valid || !invalid {
		return nil, fmt.Errorf("missing positive/negative corpus")
	}
	report := &Report{SchemaVersion: p.Version, BundleDigest: p.Digest}
	for _, c := range corpus.Cases {
		data, err := fs.ReadFile(p.bundle, c.Input)
		if err != nil {
			return nil, err
		}
		var events []json.RawMessage
		if err := json.Unmarshal(data, &events); err != nil {
			return nil, err
		}
		if len(events) == 0 {
			return nil, fmt.Errorf("empty fixture %s", c.ID)
		}
		accepted := true
		for _, event := range events {
			if _, err := p.Parse(event, c.Kind); err != nil {
				accepted = false
			}
		}
		report.Cases = append(report.Cases, Verdict{c.ID, accepted, len(events)})
	}
	return report, nil
}

func sum(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// VerifyBundle rejects changed, missing, extra and differently versioned vendored assets of a
// mosaic-event/1 bundle.
func VerifyBundle(bundle fs.FS) (string, error) {
	return VerifyBundleVersion(bundle, Version)
}

// VerifyBundleVersion is VerifyBundle for a bundle that must declare version.
func VerifyBundleVersion(bundle fs.FS, version string) (string, error) {
	bytes, err := fs.ReadFile(bundle, "manifest.json")
	if err != nil {
		return "", err
	}
	digest := sum(bytes)
	stamp, err := fs.ReadFile(bundle, "manifest.sha256")
	if err != nil || strings.TrimSpace(string(stamp)) != digest {
		return "", fmt.Errorf("manifest digest drift")
	}
	var manifest struct {
		Version string `json:"schemaVersion"`
		Files   []struct {
			Path string `json:"path"`
			Hash string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(bytes, &manifest); err != nil {
		return "", err
	}
	if manifest.Version != version || len(manifest.Files) == 0 {
		return "", fmt.Errorf("invalid bundle version/count")
	}
	var actual []string
	err = fs.WalkDir(bundle, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("bundle symlink %s", name)
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if name != "manifest.json" && name != "manifest.sha256" {
			actual = append(actual, name)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(actual)
	if len(actual) != len(manifest.Files) {
		return "", fmt.Errorf("bundle inventory drift")
	}
	for i, file := range manifest.Files {
		if actual[i] != file.Path {
			return "", fmt.Errorf("bundle inventory drift: %s", file.Path)
		}
		data, err := fs.ReadFile(bundle, file.Path)
		if err != nil {
			return "", err
		}
		if sum(data) != file.Hash {
			return "", fmt.Errorf("bundle byte drift: %s", file.Path)
		}
	}
	return digest, nil
}
