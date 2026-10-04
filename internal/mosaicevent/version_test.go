package mosaicevent

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

// Pinned corpus sizes: a bundle refresh that changes them must update these deliberately.
var pinnedCorpus = map[string]struct{ cases, events int }{
	Version:  {214, 719},
	Version2: {252, 758},
	Version3: {298, 811},
}

func TestCorpusPerVersion(t *testing.T) {
	for _, version := range []string{Version, Version2, Version3} {
		t.Run(version, func(t *testing.T) {
			p, err := NewVersion(version)
			if err != nil {
				t.Fatal(err)
			}
			if p.Version != version {
				t.Fatalf("parser version %q", p.Version)
			}
			report, err := p.Report()
			if err != nil {
				t.Fatal(err)
			}
			if report.SchemaVersion != version || report.BundleDigest != p.Digest {
				t.Fatalf("report header %s %s", report.SchemaVersion, report.BundleDigest)
			}
			data, err := fs.ReadFile(p.bundle, "cases.json")
			if err != nil {
				t.Fatal(err)
			}
			var corpus struct {
				Cases []Case `json:"cases"`
			}
			if err := json.Unmarshal(data, &corpus); err != nil {
				t.Fatal(err)
			}
			if len(report.Cases) != len(corpus.Cases) {
				t.Fatal("fixture count differs")
			}
			events, overflow := 0, 0
			for i, verdict := range report.Cases {
				events += verdict.EventCount
				if verdict.ID != corpus.Cases[i].ID || verdict.Accepted != corpus.Cases[i].Valid {
					t.Errorf("%s: accepted=%v want=%v", verdict.ID, verdict.Accepted, corpus.Cases[i].Valid)
				}
				if strings.HasPrefix(verdict.ID, "invalid-opaque-overflow-") {
					overflow++
					if verdict.Accepted {
						t.Errorf("%s: a non-finite number was accepted", verdict.ID)
					}
				}
			}
			want := pinnedCorpus[version]
			if len(report.Cases) != want.cases || events != want.events {
				t.Fatalf("corpus %d cases, %d events; pinned %d, %d", len(report.Cases), events, want.cases, want.events)
			}
			// The overflow cases are refused by decoding (Go JSON has no infinite float64), not by
			// the schemas, in every version.
			if overflow != 5 {
				t.Fatalf("overflow cases %d, want 5", overflow)
			}
		})
	}
}

func TestOverflowIsRefusedAtDecode(t *testing.T) {
	p, err := NewVersion(Version2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Parse([]byte(`{"type":"tool-input-available","toolCallId":"o","toolName":"s","input":{"n":[1e400]}}`), "chunk")
	if err == nil || !strings.Contains(err.Error(), "1e400") {
		t.Fatalf("overflow: %v", err)
	}
}

func TestUnsupportedVersion(t *testing.T) {
	if _, err := NewVersion("mosaic-event/4"); err == nil {
		t.Fatal("mosaic-event/4 loaded")
	}
	b, err := fs.Sub(assets, "vendor/1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FromBundleVersion(b, "mosaic-event/4"); err == nil {
		t.Fatal("mosaic-event/4 bundle accepted")
	}
}

// envelopeV is a daemon envelope of version v around payload.
func envelopeV(v, typ, payload string) []byte {
	return []byte(`{"kind":"event","schemaVersion":"` + v + `","seq":1,"ts":0,"correlationId":"r:1","origin":"daemon","type":"` +
		typ + `","payload":` + payload + `}`)
}

func TestSetDispatchesOnSchemaVersion(t *testing.T) {
	s, err := NewSet()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Versions(), ","); got != Version+","+Version2+","+Version3 {
		t.Fatalf("versions %s", got)
	}
	prompt := `{"type":"data-auth-prompt","data":{"promptId":"p","provider":"codex","message":"m","code":"ABCD-1234","expiresAt":1791000000000}}`
	promptV1 := `{"type":"data-auth-prompt","data":{"promptId":"p","provider":"codex","message":"m","url":"https://auth.openai.com/codex/device"}}`
	lost := `{"type":"data-session-status","data":{"sessionId":"7c1b6a0e-3f52-4c1d-9a8e-2b4f6d8e0a11","status":"lost"}}`
	usage := `{"type":"data-usage","data":{"inputTokens":10,"outputTokens":2,"model":"claude-opus-5-5"}}`
	usageCtx := `{"type":"data-usage","data":{"inputTokens":10,"outputTokens":2,"model":"claude-opus-5-5","contextTokens":51234,"contextWindow":1000000}}`
	controls := `{"type":"data-session-controls","data":{"mode":"auto_edits","maxMode":"full_auto","model":"claude-opus-5-5[1m]","effort":"high"}}`
	relayLost := []byte(`{"kind":"event","schemaVersion":"mosaic-event/2","seq":1,"ts":0,"correlationId":"lost:x","origin":"relay",` +
		`"type":"data-session-status","payload":` + lost + `}`)
	for _, c := range []struct {
		name    string
		raw     []byte
		kind    string
		version string // "" = refused
	}{
		{"v1 text", envelopeV(Version, "text-delta", `{"type":"text-delta","id":"a","delta":"hi"}`), "envelope", Version},
		{"v1 auth prompt", envelopeV(Version, "data-auth-prompt", promptV1), "envelope", Version},
		{"v1 auth prompt with code", envelopeV(Version, "data-auth-prompt", prompt), "envelope", ""},
		{"v2 auth prompt with code", envelopeV(Version2, "data-auth-prompt", prompt), "envelope", Version2},
		{"v2 auth prompt v1 shape", envelopeV(Version2, "data-auth-prompt", promptV1), "envelope", Version2},
		{"v1 lost", envelopeV(Version, "data-session-status", lost), "envelope", ""},
		{"v2 lost", envelopeV(Version2, "data-session-status", lost), "envelope", Version2},
		{"v2 relay lost", relayLost, "envelope", Version2},
		{"v1 relay lost", bytes.Replace(relayLost, []byte(Version2), []byte(Version), 1), "envelope", ""},
		{"v3 text", envelopeV(Version3, "text-delta", `{"type":"text-delta","id":"a","delta":"hi"}`), "envelope", Version3},
		{"v3 usage with context", envelopeV(Version3, "data-usage", usageCtx), "envelope", Version3},
		{"v3 usage without context", envelopeV(Version3, "data-usage", usage), "envelope", Version3},
		{"v2 usage with context", envelopeV(Version2, "data-usage", usageCtx), "envelope", ""},
		{"v1 usage with context", envelopeV(Version, "data-usage", usageCtx), "envelope", ""},
		{"v3 session controls", envelopeV(Version3, "data-session-controls", controls), "envelope", Version3},
		{"v3 session controls minimal", envelopeV(Version3, "data-session-controls", `{"type":"data-session-controls","data":{"mode":"read_only","maxMode":"ask"}}`), "envelope", Version3},
		{"v3 session controls bad mode", envelopeV(Version3, "data-session-controls", `{"type":"data-session-controls","data":{"mode":"root","maxMode":"ask"}}`), "envelope", ""},
		{"v2 session controls", envelopeV(Version2, "data-session-controls", controls), "envelope", ""},
		{"v3 auth prompt with code", envelopeV(Version3, "data-auth-prompt", prompt), "envelope", Version3},
		{"v4", envelopeV("mosaic-event/4", "text-delta", `{"type":"text-delta","id":"a","delta":"hi"}`), "envelope", ""},
		{"no version", []byte(`{"kind":"event","seq":1}`), "envelope", ""},
		{"v2 envelope v1 start", envelopeV(Version2, "start", `{"type":"start","messageMetadata":{"schemaVersion":"mosaic-event/1"}}`), "envelope", ""},
		{"v1 start", []byte(`{"type":"start","messageMetadata":{"schemaVersion":"mosaic-event/1"}}`), "chunk", Version},
		{"v2 start", []byte(`{"type":"start","messageMetadata":{"schemaVersion":"mosaic-event/2"}}`), "chunk", Version2},
		{"v3 start", []byte(`{"type":"start","messageMetadata":{"schemaVersion":"mosaic-event/3"}}`), "chunk", Version3},
		{"v3 envelope v1 start", envelopeV(Version3, "start", `{"type":"start","messageMetadata":{"schemaVersion":"mosaic-event/1"}}`), "envelope", ""},
		{"unversioned chunk", []byte(`{"type":"text-delta","id":"a","delta":"hi"}`), "chunk", ""},
		{"unknown kind", envelopeV(Version, "text-delta", `{"type":"text-delta","id":"a","delta":"hi"}`), "frame", ""},
	} {
		parsed, err := s.Parse(c.raw, c.kind)
		switch {
		case c.version == "" && err == nil:
			t.Errorf("%s: accepted as %s", c.name, parsed.Version)
		case c.version != "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.version != "" && (parsed.Version != c.version || !bytes.Equal(parsed.Raw, c.raw)):
			t.Errorf("%s: parsed as %s", c.name, parsed.Version)
		}
	}
	if s.Parser(Version2) == nil || s.Parser(Version3) == nil || s.Parser("mosaic-event/4") != nil {
		t.Fatal("Parser lookup")
	}
}

func cloneBundle(t *testing.T, root fs.FS) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	if err := fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		out[name] = &fstest.MapFile{Data: data}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestV2DriftAndCrossVersionBundles(t *testing.T) {
	v1, err := fs.Sub(assets, "vendor/1")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := fs.Sub(assetsV2, "vendor/2")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := VerifyBundleVersion(cloneBundle(t, v2), Version2)
	if err != nil || digest != "62333a1cf6afd948171840cc1344ee1718cf9b1c02488f1cbd4792347b43a1a5" {
		t.Fatalf("v2 digest %s, %v", digest, err)
	}
	// Each bundle loads only as its own version.
	if _, err := FromBundle(v2); err == nil {
		t.Fatal("v2 bundle accepted as v1")
	}
	if _, err := FromBundleVersion(v1, Version2); err == nil {
		t.Fatal("v1 bundle accepted as v2")
	}
	restamp := func(f fstest.MapFS) {
		f["manifest.sha256"].Data = []byte(sum(f["manifest.json"].Data) + "\n")
	}
	for name, mutate := range map[string]func(fstest.MapFS){
		"stamp.mjs changed":  func(f fstest.MapFS) { f["stamp.mjs"].Data = append(f["stamp.mjs"].Data, ' ') },
		"stamp.mjs missing":  func(f fstest.MapFS) { delete(f, "stamp.mjs") },
		"schema changed":     func(f fstest.MapFS) { f["data-auth-prompt.json"].Data = append(f["data-auth-prompt.json"].Data, ' ') },
		"fixture missing":    func(f fstest.MapFS) { delete(f, "fixtures/valid-v2-envelope-auth-prompt.json") },
		"extra file":         func(f fstest.MapFS) { f["extra.json"] = &fstest.MapFile{Data: []byte(`{}`)} },
		"stamp digest drift": func(f fstest.MapFS) { f["manifest.sha256"].Data = []byte(strings.Repeat("0", 64) + "\n") },
		"relabelled v1": func(f fstest.MapFS) {
			f["manifest.json"].Data = bytes.ReplaceAll(f["manifest.json"].Data, []byte(Version2), []byte(Version))
			restamp(f)
		},
	} {
		f := cloneBundle(t, v2)
		mutate(f)
		if _, err := FromBundleVersion(f, Version2); err == nil {
			t.Errorf("%s: drift accepted", name)
		}
	}
}

func TestV3DriftAndCrossVersionBundles(t *testing.T) {
	v2, err := fs.Sub(assetsV2, "vendor/2")
	if err != nil {
		t.Fatal(err)
	}
	v3, err := fs.Sub(assetsV3, "vendor/3")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := VerifyBundleVersion(cloneBundle(t, v3), Version3)
	if err != nil || digest != "dae6f6383fe4a8f1395feb0c8437bcf23fdfab2a8026408e849294c16fc6ff27" {
		t.Fatalf("v3 digest %s, %v", digest, err)
	}
	// Each bundle loads only as its own version.
	if _, err := FromBundleVersion(v3, Version2); err == nil {
		t.Fatal("v3 bundle accepted as v2")
	}
	if _, err := FromBundleVersion(v2, Version3); err == nil {
		t.Fatal("v2 bundle accepted as v3")
	}
	restamp := func(f fstest.MapFS) {
		f["manifest.sha256"].Data = []byte(sum(f["manifest.json"].Data) + "\n")
	}
	for name, mutate := range map[string]func(fstest.MapFS){
		"schema changed": func(f fstest.MapFS) {
			f["data-session-controls.json"].Data = append(f["data-session-controls.json"].Data, ' ')
		},
		"usage changed":  func(f fstest.MapFS) { f["data-usage.json"].Data = append(f["data-usage.json"].Data, ' ') },
		"schema missing": func(f fstest.MapFS) { delete(f, "data-session-controls.json") },
		"readme changed": func(f fstest.MapFS) { f["README.md"].Data = append(f["README.md"].Data, ' ') },
		"extra file":     func(f fstest.MapFS) { f["extra.json"] = &fstest.MapFile{Data: []byte(`{}`)} },
		"digest drift":   func(f fstest.MapFS) { f["manifest.sha256"].Data = []byte(strings.Repeat("0", 64) + "\n") },
		"relabelled v2": func(f fstest.MapFS) {
			f["manifest.json"].Data = bytes.ReplaceAll(f["manifest.json"].Data, []byte(Version3), []byte(Version2))
			restamp(f)
		},
	} {
		f := cloneBundle(t, v3)
		mutate(f)
		if _, err := FromBundleVersion(f, Version3); err == nil {
			t.Errorf("%s: drift accepted", name)
		}
	}
}
