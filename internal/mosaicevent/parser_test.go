package mosaicevent

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	aisdk "github.com/grafana/ai-sdk"
)

func TestCorpus(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	report, err := p.Report()
	if err != nil {
		t.Fatal(err)
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
	for i, verdict := range report.Cases {
		if verdict.ID != corpus.Cases[i].ID || verdict.Accepted != corpus.Cases[i].Valid {
			t.Errorf("%s: accepted=%v want=%v", verdict.ID, verdict.Accepted, corpus.Cases[i].Valid)
		}
	}
}

func TestRawPreservationAndSDKTypes(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"type":"tool-approval-response","approvalId":"deny","approved":false,"reason":"decline"}`,
		`{"type":"tool-approval-request","approvalId":"a","toolCallId":"t","approvalDescriptor":{"options":["accept"]},"inputSchemaInput":{"command":"echo"},"reason":"consent"}`,
		`{"type":"data-plan","data":{"entries":[{"content":"Research","priority":"high","status":"in_progress","_meta":{"vendor":true}}]}}`,
		`{"type":"start","messageMetadata":{"schemaVersion":"mosaic-event/1","other":{"retained":true}}}`,
	} {
		parsed, err := p.Parse([]byte(raw), "chunk")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(parsed.Raw, []byte(raw)) || parsed.Chunk == nil {
			t.Fatal("raw/type lost")
		}
		if parsed.Chunk.Type == aisdk.ChunkToolApprovalResponse {
			if parsed.Chunk.Approved {
				t.Fatal("denial became approval")
			}
			wire, err := json.Marshal(parsed.Chunk)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(wire, []byte(`"approved":false`)) {
				t.Fatal("SDK omitted denial")
			}
		}
		if parsed.Chunk.Type == aisdk.ChunkData && parsed.Chunk.DataName != "plan" {
			t.Fatal("SDK data name lost")
		}
	}
}

func TestCorruptedNegativeKindFailsReportSetup(t *testing.T) {
	original, err := fs.Sub(assets, "vendor/1")
	if err != nil {
		t.Fatal(err)
	}
	clone := fstest.MapFS{}
	if err := fs.WalkDir(original, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(original, name)
		if err != nil {
			return err
		}
		clone[name] = &fstest.MapFile{Data: data}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var corpus struct {
		Version string `json:"schemaVersion"`
		Cases   []Case `json:"cases"`
	}
	if err := json.Unmarshal(clone["cases.json"].Data, &corpus); err != nil {
		t.Fatal(err)
	}
	for i := range corpus.Cases {
		if !corpus.Cases[i].Valid {
			corpus.Cases[i].Kind = "chnuk"
			break
		}
	}
	clone["cases.json"].Data, err = json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"schemaVersion"`
		Files   []struct {
			Path string `json:"path"`
			Hash string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(clone["manifest.json"].Data, &manifest); err != nil {
		t.Fatal(err)
	}
	for i := range manifest.Files {
		if manifest.Files[i].Path == "cases.json" {
			manifest.Files[i].Hash = sum(clone["cases.json"].Data)
		}
	}
	clone["manifest.json"].Data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clone["manifest.sha256"].Data = []byte(sum(clone["manifest.json"].Data) + "\n")
	p, err := FromBundle(clone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Report(); err == nil || !strings.Contains(err.Error(), "invalid corpus kind") {
		t.Fatalf("corrupted corpus became a verdict: %v", err)
	}
}

func TestDriftRejectsChangedMissingExtraAndVersion(t *testing.T) {
	original, err := fs.Sub(assets, "vendor/1")
	if err != nil {
		t.Fatal(err)
	}
	config, err := fs.ReadFile(original, "contract/.npmrc")
	if err != nil || string(config) != "min-release-age=3\n" {
		t.Fatalf("native embedded cooldown config: %q, %v", config, err)
	}
	clone := func() fstest.MapFS {
		out := fstest.MapFS{}
		if err := fs.WalkDir(original, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := fs.ReadFile(original, name)
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
	if _, err := VerifyBundle(clone()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(fstest.MapFS){
		func(f fstest.MapFS) { f["contract/.npmrc"].Data = []byte("min-release-age=0\n") },
		func(f fstest.MapFS) { delete(f, "contract/.npmrc") },
		func(f fstest.MapFS) { f["chunk.json"].Data = append(f["chunk.json"].Data, ' ') },
		func(f fstest.MapFS) { delete(f, "chunk.json") },
		func(f fstest.MapFS) { f["extra.json"] = &fstest.MapFile{Data: []byte(`{}`)} },
		func(f fstest.MapFS) {
			f["manifest.json"].Data = bytes.ReplaceAll(f["manifest.json"].Data, []byte(Version), []byte("mosaic-event/2"))
			f["manifest.sha256"].Data = []byte(sum(f["manifest.json"].Data) + "\n")
		},
	} {
		if _, err := FromBundle(func() fstest.MapFS { f := clone(); mutate(f); return f }()); err == nil {
			t.Fatal("drift unexpectedly accepted")
		}
	}
}
