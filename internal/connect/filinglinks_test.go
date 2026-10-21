package connect

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"
)

// The host list is the workspace's: testdata/filing-source-hosts.json is
// byte identical to the workspace fixture (Story 94.7 A1).
func TestFilingLinksHostList(t *testing.T) {
	raw, err := os.ReadFile("testdata/filing-source-hosts.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Hosts        []string `json:"hosts"`
		BucketPrefix string   `json:"bucketPrefix"`
		GCSHost      string   `json:"gcsHost"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(filingSourceHosts, want.Hosts) || filingBucketPrefix != want.BucketPrefix || gcsHost != want.GCSHost {
		t.Fatalf("hosts %v %q %q, want %+v", filingSourceHosts, filingBucketPrefix, gcsHost, want)
	}
}

// Every shared case: the daemon's stripped descriptor equals what the
// workspace's stripFilingLinksDeep makes of it.
func TestFilingLinksSharedCases(t *testing.T) {
	raw, err := os.ReadFile("testdata/filing-link-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Input    json.RawMessage `json:"input"`
			Expected json.RawMessage `json:"expected"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no cases")
	}
	decode := func(b []byte) any {
		var v any
		d := json.NewDecoder(bytes.NewReader(b))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for i, c := range f.Cases {
		in, want := decode(c.Input), decode(c.Expected)
		if got := stripFilingLinksDeep(in); !reflect.DeepEqual(got, want) {
			gj, _ := json.Marshal(got)
			t.Errorf("case %d:\n got  %s\n want %s", i, gj, c.Expected)
		}
		if got := stripFilingDescriptor(in); !reflect.DeepEqual(got, want) {
			t.Errorf("case %d: descriptor form differs", i)
		}
		if got := stripFilingLinksDeep(want); !reflect.DeepEqual(got, want) {
			t.Errorf("case %d: stripping again changes it", i)
		}
	}
}

// Readings beyond the shared cases: the forms a browser reads as a filing
// source host, and ordinary links left alone.
func TestFilingLinksReadings(t *testing.T) {
	for in, want := range map[string]string{
		// markdown links and images keep their label or alt
		"[doc](<https://www.sec.gov/a b>)":                                   "doc",
		`[doc](https://sec.gov/a "Title")`:                                   "doc",
		"[doc](https://sec.gov/a_(b))":                                       "doc",
		"[ok](https://example.com/sec.gov)":                                  "[ok](https://example.com/sec.gov)",
		"[doc](//www.sec.gov/a)":                                             "doc",
		"[doc](https:\\\\www.sec.gov\\a)":                                    "doc",
		"[doc](https:www.sec.gov/a)":                                         "doc",
		"[doc](HTTPS://WWW.SEC.GOV./A)":                                      "doc",
		"[doc](https://user@www.sec.gov:443/a)":                              "doc",
		"[doc](https://www.sec.gov@example.com/a)":                           "[doc](https://www.sec.gov@example.com/a)",
		"[doc](https://sec%2Egov/a)":                                         "doc",
		"[doc](https://ｓｅｃ．ｇｏｖ/a)":                                           "doc",
		"[doc](https://storage.googleapis.com/x/../themcp-v2-filings-pdf/a)": "doc",
		"[doc](https://example.com:99999/sec.gov)":                           "doc",
		"[doc](www.sec.gov/a)":                                               "doc",
		"[doc](mailto:someone@example.com)":                                  "[doc](mailto:someone@example.com)",
		"[doc](#section)":                                                    "[doc](#section)",
		"![chart](https://kap.org.tr/a.png)":                                 "chart",
		// reference definitions start a line, after \n, \r or U+2028
		"intro\r[r]: https://sec.gov/a":                   "intro\r(source link removed)",
		"intro\u2028  [r]: <https://sec.gov/a> 'T'\nnext": "intro\u2028(source link removed)\nnext",
		"x [r]: https://sec.gov/a":                        "x [r]: (source link removed)",
		// autolinks and bare addresses
		"<HTTP://sec.gov/a>":                         "(source link removed)",
		"see https://sec.gov/a, and http://tmx.com.": "see (source link removed), and (source link removed).",
		"(www.tmxmoney.com/q)":                       "((source link removed))",
		"_www.sec.gov_":                              "_(source link removed)_",
		"awww.sec.gov 2www.sec.gov":                  "awww.sec.gov 2www.sec.gov",
		"\"https://sec.gov/a\"":                      "\"(source link removed)\"",
		"https://sec.gov\u00a0next":                  "(source link removed)\u00a0next",
		"https://example.com/?u=https://sec.gov/a":   "https://example.com/?u=https://sec.gov/a",
		"ftp://sec.gov/a":                            "ftp://sec.gov/a",
		"httpſ://sec.gov/a":                          "httpſ://sec.gov/a",
		"https://":                                   "https://",
		"plain text, sec.gov named only":             "plain text, sec.gov named only",
	} {
		if got := stripFilingLinks(in); got != want {
			t.Errorf("%q:\n got  %q\n want %q", in, got, want)
		}
	}
	// Non-string values pass through unchanged; the input is not changed.
	in := map[string]any{"n": json.Number("1.50"), "b": false, "z": nil, "s": []any{"https://sec.gov/x"}}
	got := stripFilingLinksDeep(in).(map[string]any)
	if got["n"] != json.Number("1.50") || got["b"] != false || got["z"] != nil || got["s"].([]any)[0] != removedLinkText ||
		in["s"].([]any)[0] != "https://sec.gov/x" {
		t.Fatalf("deep %v, input %v", got, in)
	}
}

// The card the daemon sends: links removed from every string, the card's
// other fields kept, the length rule applied to both commands.
func TestFilingLinksCard(t *testing.T) {
	request := map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "tool_use_id": "toolu_1",
		"input": map[string]any{"command": "curl https://www.sec.gov/a", "description": "see [doc](https://sec.gov/b)"}}
	card := claudeApprovalCard("r1", request)
	d := card["approvalDescriptor"].(map[string]any)
	in := d["input"].(map[string]any)
	if in["command"] != "curl (source link removed)" || in["description"] != "see doc" || card["toolCallId"] != "toolu_1" {
		t.Fatalf("card %v", card)
	}
	if request["input"].(map[string]any)["command"] != "curl https://www.sec.gov/a" {
		t.Fatal("the request was changed")
	}
	raw := claudeRawCard("r1", request)
	if a, _ := cardDigest(card["approvalDescriptor"]); a == "" {
		t.Fatal("no digest")
	} else if b, _ := cardDigest(raw["approvalDescriptor"]); a == b {
		t.Fatal("the digest does not cover the stripped descriptor")
	}
	o := newTestOutbox(t)
	long := func(cmd string) Chunk {
		return Chunk{"type": "tool-approval-request", "approvalId": "a", "toolCallId": "t",
			"approvalDescriptor": map[string]any{"command": cmd}}
	}
	pad := func(n int) string { return string(bytes.Repeat([]byte("x"), n)) }
	// The original is too long although the shown command fits.
	if msg := cardRefusal(long("curl https://sec.gov/"+pad(previewBytes)), o.Fits); msg != cardCommandTooLong {
		t.Fatalf("long original: %q", msg)
	}
	// The shown command is too long although the original fits.
	short := "http://sec.gov "
	n := previewBytes / len(short)
	cmd := string(bytes.Repeat([]byte(short), n))
	if len(cmd) > previewBytes {
		t.Fatal("test command too long")
	}
	if msg := cardRefusal(long(cmd), o.Fits); msg != cardCommandTooLong {
		t.Fatalf("long shown command: %q", msg)
	}
	if msg := cardRefusal(long("curl https://sec.gov/a"), o.Fits); msg != "" {
		t.Fatalf("short command: %q", msg)
	}
}
