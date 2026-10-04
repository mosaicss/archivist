package guidance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var pairs = [][2]string{
	{SurfaceAgentUI, FormFull}, {SurfaceAgentUI, FormCompact},
	{SurfaceMosaicUI, FormFull}, {SurfaceMosaicUI, FormCompact},
}

// The embedded bytes must match vendor-source.json: a drifted or hand-edited
// copy fails here, and the copy is refreshed only by re-vendoring the snapshot.
func TestEmbeddedMatchesVendorSource(t *testing.T) {
	v, err := LoadVendorSource()
	if err != nil {
		t.Fatal(err)
	}
	if v.SchemaVersion != SchemaVersion || v.SnapshotPath != "reference/schemas/agent-guidance/1" ||
		v.VendoredPath != "internal/guidance/vendor/1" {
		t.Fatalf("vendor source %+v", v)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(v.SourceCommit) {
		t.Fatalf("sourceCommit %q is not a full SHA", v.SourceCommit)
	}
	if len(v.Files) != len(pairs) {
		t.Fatalf("files %d", len(v.Files))
	}
	entries, err := assets.ReadDir("vendor/1")
	if err != nil || len(entries) != len(pairs) {
		t.Fatalf("embedded files %v %v", entries, err)
	}
	digests := map[string]string{}
	for _, f := range v.Files {
		digests[f.Path] = f.SHA256
	}
	for _, p := range pairs {
		name := p[0] + "." + p[1] + ".md"
		b, err := assets.ReadFile("vendor/1/" + name)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != digests[name] {
			t.Errorf("%s: embedded digest %x, vendor-source %s", name, sum, digests[name])
		}
		e := Embedded(p[0], p[1])
		if e.Body != string(b) || e.Digest != "sha256:"+digests[name] || e.Source != SourceEmbedded {
			t.Errorf("Embedded(%s) = %+v", name, e)
		}
	}
}

func TestEmbeddedContent(t *testing.T) {
	for _, p := range pairs {
		body := Embedded(p[0], p[1]).Body
		if !strings.Contains(body, "SEC, SEDAR+, KAP and expanding") || strings.Contains(body, "SEC and SEDAR") {
			t.Errorf("%v: global filings wording", p)
		}
		if !strings.Contains(body, "Never output source_url") {
			t.Errorf("%v: no upstream URL rule", p)
		}
		if p[1] == FormCompact && (len(body) > 2048-200-1 || strings.ContainsAny(body, "-–—")) {
			t.Errorf("%v: compact length %d or a hyphen", p, len(body))
		}
	}
	if !strings.Contains(Embedded(SurfaceMosaicUI, FormFull).Body, "cite_as") ||
		strings.Contains(Embedded(SurfaceAgentUI, FormFull).Body, "cite_as") {
		t.Error("citation clause per surface")
	}
}

func TestEmbeddedPanicsOnUnknownPair(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	Embedded("x", FormFull)
}

func liveBody(surface, form, text string) map[string]any {
	return map[string]any{"schemaVersion": SchemaVersion, "surface": surface, "form": form, "digest": Digest(text), "text": text}
}

func serveJSON(t *testing.T, status int, body any, seen *http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(context.Background())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch b := body.(type) {
		case []byte:
			_, _ = w.Write(b)
		default:
			_ = json.NewEncoder(w).Encode(b)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchLive(t *testing.T) {
	text := "live guidance text\n"
	var seen http.Request
	srv := serveJSON(t, 200, liveBody(SurfaceMosaicUI, FormFull, text), &seen)
	got := Fetch(context.Background(), srv.URL+"/", SurfaceMosaicUI, FormFull)
	if got.Source != SourceLive || got.Body != text || got.Digest != Digest(text) || got.Reason != "" {
		t.Fatalf("got %+v", got)
	}
	if seen.Method != http.MethodGet || seen.URL.Path != "/agent-guidance" ||
		seen.URL.Query().Get("surface") != SurfaceMosaicUI || seen.URL.Query().Get("form") != FormFull {
		t.Fatalf("request %s %s", seen.Method, seen.URL)
	}
	if seen.Header.Get("Authorization") != "" || seen.Header.Get("Cookie") != "" {
		t.Fatal("credentials sent")
	}
}

func TestFetchFallsBackToEmbedded(t *testing.T) {
	text := "live guidance text\n"
	good := func() map[string]any { return liveBody(SurfaceAgentUI, FormCompact, text) }
	with := func(k string, v any) map[string]any { b := good(); b[k] = v; return b }
	without := func(k string) map[string]any { b := good(); delete(b, k); return b }
	big := strings.Repeat("a", MaxTextBytes+1)
	cases := map[string]struct {
		status int
		body   any
		reason string
	}{
		"non-200":           {503, good(), "status 503"},
		"404":               {404, map[string]any{"error": "x"}, "status 404"},
		"redirect":          {302, good(), "status 302"},
		"wrong schema":      {200, with("schemaVersion", "mosaic-agent-guidance/2"), "wrong schemaVersion"},
		"missing schema":    {200, without("schemaVersion"), "wrong schemaVersion"},
		"wrong surface":     {200, with("surface", SurfaceMosaicUI), "wrong surface"},
		"wrong form":        {200, with("form", FormFull), "wrong form"},
		"digest mismatch":   {200, with("digest", Digest("other")), "digest mismatch"},
		"missing digest":    {200, without("digest"), "digest mismatch"},
		"empty text":        {200, liveBody(SurfaceAgentUI, FormCompact, ""), "empty text"},
		"null text":         {200, with("text", nil), "empty text"},
		"text over 64 KiB":  {200, liveBody(SurfaceAgentUI, FormCompact, big), "text too large"},
		"body too large":    {200, []byte(`{"pad":"` + strings.Repeat("a", maxBodyBytes) + `"}`), "response too large"},
		"invalid UTF-8":     {200, []byte("{\"schemaVersion\":\"" + SchemaVersion + "\",\"surface\":\"agent-ui\",\"form\":\"compact\",\"digest\":\"x\",\"text\":\"\xff\xfe\"}"), "text is not UTF-8"},
		"invalid JSON":      {200, []byte("not json"), "invalid JSON"},
		"text not a string": {200, with("text", 42), "invalid JSON"},
	}
	want := Embedded(SurfaceAgentUI, FormCompact)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serveJSON(t, c.status, c.body, nil)
			got := Fetch(context.Background(), srv.URL, SurfaceAgentUI, FormCompact)
			if got.Source != SourceEmbedded || got.Body != want.Body || got.Digest != want.Digest {
				t.Fatalf("got source %s digest %s", got.Source, got.Digest)
			}
			if got.Reason != c.reason {
				t.Fatalf("reason %q, want %q", got.Reason, c.reason)
			}
		})
	}
}

func TestFetchTimeoutAndTransportFailures(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 200 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	start := time.Now()
	got := Fetch(context.Background(), srv.URL, SurfaceMosaicUI, FormFull)
	if got.Source != SourceEmbedded || got.Reason != "timeout" || hits.Load() != 1 {
		t.Fatalf("got %+v hits %d", got, hits.Load())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %s", elapsed)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	if got := Fetch(context.Background(), closed.URL, SurfaceMosaicUI, FormFull); got.Source != SourceEmbedded || got.Reason != "request failed" {
		t.Fatalf("closed server %+v", got)
	}
	for _, base := range []string{"", "ftp://example.invalid", "not a url", "http://"} {
		if got := Fetch(context.Background(), base, SurfaceMosaicUI, FormFull); got.Source != SourceEmbedded || got.Reason != "invalid chat-api base URL" {
			t.Errorf("base %q: %+v", base, got)
		}
	}
}

func TestFetchTimeoutConstant(t *testing.T) {
	if FetchTimeout != 3*time.Second || MaxTextBytes != 64*1024 {
		t.Fatalf("bounds %s %d", FetchTimeout, MaxTextBytes)
	}
}

// skill/SKILL.md carries the agent-ui full guidance between markers, byte for
// byte, and the description says global filings (Story 78.31).
func TestSkillCarriesAgentGuidance(t *testing.T) {
	b, err := os.ReadFile("../../skill/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	skill := string(b)
	const begin, end = "<!-- mosaic-agent-guidance:agent-ui:full:begin -->\n", "<!-- mosaic-agent-guidance:agent-ui:full:end -->"
	i, j := strings.Index(skill, begin), strings.Index(skill, end)
	if i < 0 || j < i || strings.Count(skill, begin) != 1 || strings.Count(skill, end) != 1 {
		t.Fatal("guidance markers missing or repeated")
	}
	if got := skill[i+len(begin) : j]; got != Embedded(SurfaceAgentUI, FormFull).Body {
		t.Fatal("SKILL.md guidance block differs from the embedded agent-ui full text")
	}
	head, _, _ := strings.Cut(skill, "\n---\n")
	if strings.Contains(skill, "SEC and SEDAR") || !strings.Contains(head, "global public company filings (SEC, SEDAR+, KAP and expanding)") {
		t.Fatal("skill wording is not global filings")
	}
	for _, upstream := range []string{"sec.gov/", "sedarplus.ca", "kap.org.tr", "quotemedia.com", "money.tmx.com"} {
		if strings.Contains(skill, upstream) {
			t.Errorf("skill links upstream %s", upstream)
		}
	}
}
