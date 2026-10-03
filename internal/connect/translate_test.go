package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mosaicss/archivist/internal/mosaicevent"
)

const vendorDir = "../mosaicevent/vendor/1"

type rawRow struct {
	SourceIndex int             `json:"sourceIndex"`
	Direction   string          `json:"direction"`
	Frame       json.RawMessage `json:"frame"`
}

// replayClaude runs a retained raw capture through the runtime translator.
func replayClaude(t *testing.T, name string) []Chunk {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vendorDir, "fixtures/raw", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []rawRow
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	tr := NewTranslator()
	tr.FallbackModel = "claude-captured"
	var out []Chunk
	for _, row := range rows {
		if row.Direction == "out" {
			var frame map[string]any
			if err := json.Unmarshal(row.Frame, &frame); err != nil {
				t.Fatal(err)
			}
			out = append(out, tr.Out(frame)...)
			continue
		}
		out = append(out, tr.In(row.Frame)...)
	}
	if tr.Skipped != 0 {
		t.Errorf("%s: translator skipped %d retained frames", name, tr.Skipped)
	}
	return out
}

// TestTranslatorMatchesGoldens is the 78.13 contract: the Claude raw
// captures, replayed through the Go translator, equal the canonical goldens.
func TestTranslatorMatchesGoldens(t *testing.T) {
	parser, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"02-claude-allow", "03-claude-deny", "04-claude-interrupt", "06-claude-mcp"} {
		t.Run(name, func(t *testing.T) {
			got := replayClaude(t, name)
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := os.ReadFile(filepath.Join(vendorDir, "fixtures", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var gotV, wantV []any
			if err := json.Unmarshal(gotJSON, &gotV); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(wantJSON, &wantV); err != nil {
				t.Fatal(err)
			}
			if len(gotV) != len(wantV) {
				t.Fatalf("chunk count %d, golden %d\n got: %s", len(gotV), len(wantV), gotJSON)
			}
			for i := range wantV {
				if !reflect.DeepEqual(gotV[i], wantV[i]) {
					g, _ := json.Marshal(gotV[i])
					w, _ := json.Marshal(wantV[i])
					t.Errorf("chunk %d differs\n got: %s\nwant: %s", i, g, w)
				}
			}
			for i, c := range got {
				raw, _ := json.Marshal(c)
				if _, err := parser.Parse(raw, "chunk"); err != nil {
					t.Errorf("chunk %d invalid: %v", i, err)
				}
			}
		})
	}
}

func TestTranslatorSkipsUnknownFrames(t *testing.T) {
	tr := NewTranslator()
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"rate_limit_event"}`,
		`{"type":"system","subtype":"hook_started"}`,
		`not json`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"x"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"x"}}}`,
		`{"type":"control_request","request_id":"r","request":{"subtype":"hook_callback"}}`,
	} {
		if out := tr.In([]byte(line)); len(out) != 0 {
			t.Errorf("%s produced %v", line, out)
		}
	}
	// A result without usage details still produces a valid usage chunk.
	out := tr.In([]byte(`{"type":"result","is_error":false,"usage":{"input_tokens":3,"output_tokens":4}}`))
	if len(out) != 2 || out[0]["type"] != "data-usage" || out[1]["type"] != "finish" {
		t.Fatalf("result chunks %v", out)
	}
	data := out[0]["data"].(map[string]any)
	if data["model"] != "claude" || data["inputTokens"] != int64(3) {
		t.Fatalf("usage %v", data)
	}
}
