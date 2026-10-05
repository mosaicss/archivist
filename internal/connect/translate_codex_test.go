package connect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/mosaicevent"
)

// codexRow is a retained Codex frame: in rows are server messages, out
// rows are the daemon's replies to approval requests.
type codexRow struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params json.RawMessage `json:"params"`
	Result struct {
		Decision string `json:"decision"`
	} `json:"result"`
}

// replayCodex runs a retained raw capture through the runtime translator.
func replayCodex(t *testing.T, name string) []Chunk {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vendorDir, "fixtures/raw", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []rawRow
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatal(err)
	}
	tr := NewCodexTranslator()
	tr.Model = "codex-captured"
	approvals := map[string]string{}
	var out []Chunk
	for _, row := range rows {
		var f codexRow
		if err := json.Unmarshal(row.Frame, &f); err != nil {
			t.Fatal(err)
		}
		id := strings.Trim(string(f.ID), `"`)
		if row.Direction == "out" {
			out = append(out, tr.ApprovalResponse(approvals[id], f.Result.Decision))
			continue
		}
		chunks := tr.In(f.Method, id, f.Params)
		for _, c := range chunks {
			if c["type"] == "tool-approval-request" {
				approvals[id] = c["approvalId"].(string)
			}
		}
		out = append(out, chunks...)
	}
	if tr.Skipped != 0 {
		t.Errorf("%s: translator skipped %d retained frames", name, tr.Skipped)
	}
	return out
}

// TestCodexTranslatorMatchesGoldens is the 78.13 contract for Codex: the
// raw captures, replayed through the Go translator, equal the canonical
// goldens exactly, and every chunk is valid mosaic-event/1.
func TestCodexTranslatorMatchesGoldens(t *testing.T) {
	parser, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"07-codex-accept", "08-codex-decline", "09-codex-mcp", "10-codex-interrupt",
		"synthetic-codex-mcp-failed", "synthetic-codex-mcp-failed-result"} {
		t.Run(name, func(t *testing.T) {
			got := replayCodex(t, name)
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

// TestCodexTranslatorExtensions covers the mappings beyond the goldens and
// the never-fail rule for unknown or out-of-order frames.
func TestCodexTranslatorExtensions(t *testing.T) {
	parser, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	tr := NewCodexTranslator()
	var got []Chunk
	in := func(method, id, params string) {
		got = append(got, tr.In(method, id, json.RawMessage(params))...)
	}
	in("turn/started", "", `{"threadId":"th","turn":{"id":"t1","status":"inProgress"}}`)
	in("item/reasoning/summaryTextDelta", "", `{"itemId":"r1","delta":"think","summaryIndex":0,"threadId":"th","turnId":"t1"}`)
	in("item/reasoning/summaryTextDelta", "", `{"itemId":"r1","delta":"ing","summaryIndex":0,"threadId":"th","turnId":"t1"}`)
	in("item/completed", "", `{"item":{"type":"reasoning","id":"r1","summary":["thinking"]},"threadId":"th","turnId":"t1"}`)
	in("turn/plan/updated", "", `{"threadId":"th","turnId":"t1","explanation":null,"plan":[{"step":"a","status":"pending"},{"step":"b","status":"inProgress"},{"step":"c","status":"completed"}]}`)
	in("item/started", "", `{"item":{"type":"fileChange","id":"f1","status":"inProgress","changes":[{"path":"/w/a.txt","kind":{"type":"add"},"diff":"+x\n"},{"path":"/w/b.txt","kind":{"type":"update","move_path":null},"diff":"-a\n+b\n"}]},"threadId":"th","turnId":"t1"}`)
	in("item/fileChange/requestApproval", "7", `{"threadId":"th","turnId":"t1","itemId":"f1","startedAtMs":1,"reason":"write files"}`)
	in("item/completed", "", `{"item":{"type":"fileChange","id":"f1","status":"completed","changes":[]},"threadId":"th","turnId":"t1"}`)
	in("item/started", "", `{"item":{"type":"agentMessage","id":"m1","text":""},"threadId":"th","turnId":"t1"}`)
	in("item/agentMessage/delta", "", `{"itemId":"m1","delta":"hi","threadId":"th","turnId":"t1"}`)
	// Unknown and out-of-order frames are skipped, never fatal.
	in("item/agentMessage/delta", "", `{"itemId":"nope","delta":"x"}`)
	in("item/started", "", `{"item":{"type":"imageView","id":"v1"}}`)
	in("thread/status/changed", "", `{"threadId":"th","status":{"type":"active"}}`)
	in("error", "", `{"error":{"message":"retrying"},"willRetry":true,"threadId":"th","turnId":"t1"}`)
	in("turn/completed", "", `{"threadId":"th","turn":{"id":"t1","status":"failed","error":{"message":"boom","codexErrorInfo":null}}}`)

	types := []string{}
	for _, c := range got {
		types = append(types, c["type"].(string))
		raw, _ := json.Marshal(c)
		if _, err := parser.Parse(raw, "chunk"); err != nil {
			t.Errorf("%s invalid: %v", raw, err)
		}
	}
	want := "start reasoning-start reasoning-delta reasoning-delta reasoning-end data-plan tool-input-available data-patch data-patch " +
		"tool-approval-request tool-output-available text-start text-delta text-end error finish"
	if strings.Join(types, " ") != want {
		t.Fatalf("types\n got: %s\nwant: %s", strings.Join(types, " "), want)
	}
	plan := got[5]["data"].(map[string]any)["entries"].([]any)
	if plan[1].(map[string]any)["status"] != "in_progress" || plan[1].(map[string]any)["priority"] != "medium" {
		t.Fatalf("plan %v", plan)
	}
	req := got[9]
	d := req["approvalDescriptor"].(map[string]any)
	if req["approvalId"] != "t1:approval:7" || req["toolCallId"] != "f1" || d["reason"] != "write files" || len(d["changes"].([]any)) != 2 {
		t.Fatalf("file approval %v", req)
	}
	if got[14]["errorText"] != `{"message":"boom","codexErrorInfo":null}` {
		t.Fatalf("error text %v", got[14]["errorText"])
	}
	if tr.Skipped != 4 {
		t.Fatalf("skipped %d", tr.Skipped)
	}
}

// Story 78.33: a webSearch item (shapes as probed on codex-cli 0.160.0)
// becomes a web_search tool call; a completion without a start emits all
// three chunks; every chunk is a valid mosaic-event/1 chunk.
func TestCodexTranslatorWebSearch(t *testing.T) {
	parser, err := mosaicevent.New()
	if err != nil {
		t.Fatal(err)
	}
	tr := NewCodexTranslator()
	var got []Chunk
	in := func(method, params string) { got = append(got, tr.In(method, "", json.RawMessage(params))...) }
	completed := `{"item":{"type":"webSearch","id":"ws1","query":"brent crude price",` +
		`"action":{"type":"search","query":"brent crude price","queries":null},` +
		`"results":[{"type":"text_result","domain":"www.reuters.com","ref_id":"turn0search0","snippet":"Oil rose.","title":"Reuters","url":"https://www.reuters.com/x"}]},"threadId":"th","turnId":"t1"}`
	in("item/started", `{"item":{"type":"webSearch","id":"ws1","query":"","action":null,"results":null},"threadId":"th","turnId":"t1"}`)
	in("item/started", `{"item":{"type":"webSearch","id":"ws1","query":"","action":null,"results":null},"threadId":"th","turnId":"t1"}`)
	in("item/completed", completed)
	// Completed without started (and with null action and results).
	in("item/completed", `{"item":{"type":"webSearch","id":"ws2","query":"cpi","action":null,"results":null},"threadId":"th","turnId":"t1"}`)
	for i, c := range got {
		raw, _ := json.Marshal(c)
		if _, err := parser.Parse(raw, "chunk"); err != nil {
			t.Errorf("chunk %d invalid: %v (%s)", i, err, raw)
		}
	}
	norm := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	want := []string{
		`{"toolCallId":"ws1","toolName":"web_search","type":"tool-input-start"}`,
		`{"input":{"action":{"queries":null,"query":"brent crude price","type":"search"},"query":"brent crude price"},"toolCallId":"ws1","toolName":"web_search","type":"tool-input-available"}`,
		`{"output":{"action":{"queries":null,"query":"brent crude price","type":"search"},"query":"brent crude price","results":[{"domain":"www.reuters.com","ref_id":"turn0search0","snippet":"Oil rose.","title":"Reuters","type":"text_result","url":"https://www.reuters.com/x"}]},"toolCallId":"ws1","type":"tool-output-available"}`,
		`{"toolCallId":"ws2","toolName":"web_search","type":"tool-input-start"}`,
		`{"input":{"query":"cpi"},"toolCallId":"ws2","toolName":"web_search","type":"tool-input-available"}`,
		`{"output":{"query":"cpi"},"toolCallId":"ws2","type":"tool-output-available"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("chunks %d: %s", len(got), norm(got))
	}
	for i := range want {
		if norm(got[i]) != want[i] {
			t.Errorf("chunk %d\n got: %s\nwant: %s", i, norm(got[i]), want[i])
		}
	}
	if tr.Skipped != 0 || len(tr.searches) != 0 {
		t.Fatalf("skipped %d, open searches %v", tr.Skipped, tr.searches)
	}
}

func TestJSStringify(t *testing.T) {
	for in, want := range map[string]string{
		`{"b": 1, "a": [true, null, "x’\n\u0001"], "c": {"z": 1.50, "y": -0}}`: `{"b":1,"a":[true,null,"x’\n\u0001"],"c":{"z":1.5,"y":0}}`,
		`"<&>"`:                `"<&>"`,
		`1e21`:                 `1e+21`,
		`0.0000001`:            `1e-7`,
		`12345678901234567890`: `12345678901234567000`,
		`not json`:             `null`,
	} {
		if got := jsStringify(json.RawMessage(in)); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
