package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mosaicss/archivist/internal/taskscope"
	"github.com/spf13/cobra"
)

const testPublishSession = "11111111-2222-4333-8444-555555555555"

// fakeArtifactAPI is chat-api's POST /artifacts: it records each upload's
// multipart fields and answers 201, or status when set.
type fakeArtifactAPI struct {
	srv    *httptest.Server
	mu     sync.Mutex
	status int
	calls  int
	fields [][]string
	types  []string
	names  []string
	bodies []string
}

func newFakeArtifactAPI(t *testing.T) *fakeArtifactAPI {
	f := &fakeArtifactAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Method != "POST" || r.URL.Path != "/artifacts" || r.Header.Get("Authorization") != "Bearer "+testTaskToken {
			w.WriteHeader(404)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":"This task token does not allow this operation.","code":"TASK_SCOPE_REQUIRED"}`))
			return
		}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		var fields []string
		var name, ctype, body, sid string
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(400)
				return
			}
			b, _ := io.ReadAll(p)
			fields = append(fields, p.FormName())
			if p.FormName() == "file" {
				name, ctype, body = p.FileName(), p.Header.Get("Content-Type"), string(b)
			} else {
				sid = string(b)
			}
		}
		f.fields = append(f.fields, fields)
		f.types = append(f.types, ctype)
		f.names = append(f.names, name)
		f.bodies = append(f.bodies, body)
		sum := sha256.Sum256([]byte(body))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{"artifactId": "art-" + name, "ownerId": "user_x", "sessionId": sid,
			"name": name, "mediaType": ctype, "size": len(body), "digest": hex.EncodeToString(sum[:]), "createdAt": 1700000000000})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// newPublishSession serves task mode with publish_artifact for dir.
func newPublishSession(t *testing.T, baseURL, dir string, pub bool) (*mcp.ClientSession, int) {
	t.Helper()
	t.Setenv("ARCHIVIST_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ARCHIVIST_BASE_URL", baseURL)
	tokenPath := filepath.Join(t.TempDir(), "task-token")
	if err := os.WriteFile(tokenPath, []byte(testTaskToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg *publishConfig
	if pub {
		cfg = &publishConfig{SessionID: testPublishSession, Dir: dir}
	}
	newRoot := func() *cobra.Command { return buildRootForTest("dev") }
	server, count := buildMCPServerTask(newRoot, "dev", fileToken(tokenPath), true, cfg)
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "publish-test", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, count
}

func toolNames(t *testing.T, cs *mcp.ClientSession) ([]string, map[string]*mcp.Tool) {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	sort.Strings(names)
	return names, byName
}

func TestPublishToolOnlyWithSession(t *testing.T) {
	api := newFakeArtifactAPI(t)
	cs, count := newPublishSession(t, api.srv.URL, t.TempDir(), false)
	if names, _ := toolNames(t, cs); count != 5 || strings.Join(names, ",") != strings.Join(expectedTaskToolNames, ",") {
		t.Fatalf("without a session: %d tools %v", count, names)
	}
	cs, count = newPublishSession(t, api.srv.URL, t.TempDir(), true)
	names, byName := toolNames(t, cs)
	if count != 6 || strings.Join(names, ",") != "companies_search,publish_artifact,read_passage,read_section,search,toc" {
		t.Fatalf("with a session: %d tools %v", count, names)
	}
	tool := byName["publish_artifact"]
	if tool.Annotations == nil || tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || !*tool.Annotations.OpenWorldHint {
		t.Fatalf("publish_artifact annotations %+v", tool.Annotations)
	}
	schema, _ := json.Marshal(tool.InputSchema)
	if !strings.Contains(string(schema), `"required":["path"]`) || !strings.Contains(string(schema), `"additionalProperties":false`) {
		t.Fatalf("schema %s", schema)
	}
}

func TestPublishArtifactHappyPath(t *testing.T) {
	api := newFakeArtifactAPI(t)
	dir := t.TempDir()
	files := map[string]struct{ body, mt string }{
		"r.pdf":       {"%PDF-1.4 x", "application/pdf"},
		"n.txt":       {"text", "text/plain"},
		"m.md":        {"# md", "text/markdown"},
		"c.csv":       {"a,b\n", "text/csv"},
		"sub/j.json":  {`{"ok":true}`, "application/json"},
		"spaced n.md": {"x", "text/markdown"},
	}
	for name, f := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cs, _ := newPublishSession(t, api.srv.URL, dir, true)
	for name, f := range files {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "publish_artifact", Arguments: map[string]any{"path": name}})
		if err != nil || res.IsError {
			t.Fatalf("%s: err %v result %+v", name, err, res)
		}
		var got publishResult
		raw, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		var fromText publishResult
		if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &fromText); err != nil || fromText != got {
			t.Fatalf("text content %v differs from structured %+v", err, got)
		}
		base := filepath.Base(name)
		sum := sha256.Sum256([]byte(f.body))
		if got.ArtifactID != "art-"+base || got.Name != base || got.MediaType != f.mt || got.SessionID != testPublishSession ||
			got.Digest != hex.EncodeToString(sum[:]) || got.Size != int64(len(f.body)) ||
			got.URL != api.srv.URL+"/artifacts/"+strings.ReplaceAll("art-"+base, " ", "%20") {
			t.Fatalf("%s: result %+v", name, got)
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.calls != len(files) {
		t.Fatalf("uploads %d", api.calls)
	}
	for i, fields := range api.fields {
		if strings.Join(fields, ",") != "sessionId,file" {
			t.Fatalf("upload %d fields %v", i, fields)
		}
		if files[api.names[i]].mt != api.types[i] && files["sub/"+api.names[i]].mt != api.types[i] {
			t.Fatalf("upload %d: %s sent as %s", i, api.names[i], api.types[i])
		}
	}
}

func TestPublishArtifactRefusalsUploadNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink cases need unix")
	}
	api := newFakeArtifactAPI(t)
	parent := t.TempDir()
	dir := filepath.Join(parent, "cwd")
	_ = os.MkdirAll(filepath.Join(dir, "real"), 0o700)
	write := func(p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(parent, "outside.txt"), "secret")
	write(filepath.Join(dir, "ok.txt"), "ok")
	write(filepath.Join(dir, "real", "in.txt"), "in")
	write(filepath.Join(dir, "x.exe"), "MZ")
	write(filepath.Join(dir, "x.pdf"), "not a pdf")
	write(filepath.Join(dir, "x.json"), "{bad")
	big := strings.Repeat("a", 10*1024*1024+1)
	write(filepath.Join(dir, "big.txt"), big)
	_ = os.Symlink(filepath.Join(dir, "ok.txt"), filepath.Join(dir, "link.txt"))
	_ = os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "linkdir"))
	cs, _ := newPublishSession(t, api.srv.URL, dir, true)
	cases := map[string]any{
		"outside":     filepath.Join(parent, "outside.txt"),
		"dotdot":      "../outside.txt",
		"symlink":     "link.txt",
		"symlinkdir":  "linkdir/in.txt",
		"too large":   "big.txt",
		"bad ext":     "x.exe",
		"not pdf":     "x.pdf",
		"bad json":    "x.json",
		"missing":     "nope.txt",
		"not string":  42,
		"empty":       "",
		"extra field": nil,
	}
	for what, arg := range cases {
		args := map[string]any{"path": arg}
		if what == "extra field" {
			args = map[string]any{"path": "ok.txt", "name": "x"}
		}
		text, isErr := callToolText(t, cs, "publish_artifact", args)
		if !isErr || !strings.Contains(text, "exit code 2") {
			t.Errorf("%s: isErr=%v text=%s", what, isErr, text)
		}
	}
	api.mu.Lock()
	calls := api.calls
	api.mu.Unlock()
	if calls != 0 {
		t.Fatalf("%d uploads for refused files", calls)
	}
}

// A token without the publish scope: chat-api refuses (403) and the tool
// fails as an auth error with nothing stored.
func TestPublishArtifactWithoutScope(t *testing.T) {
	api := newFakeArtifactAPI(t)
	api.status = 403
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, _ := newPublishSession(t, api.srv.URL, dir, true)
	text, isErr := callToolText(t, cs, "publish_artifact", map[string]any{"path": "a.txt"})
	if !isErr || !strings.Contains(text, "exit code 4") || !strings.Contains(text, "TASK_SCOPE_REQUIRED") {
		t.Fatalf("isErr=%v text=%s", isErr, text)
	}
	if taskscope.ToolAllowedFor(taskscope.PublishTool, []string{"search", "read"}) {
		t.Fatal("publish_artifact allowed without the publish scope")
	}
}

func TestMCPServePublishFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build in -short mode")
	}
	bin := buildTestBinary(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "tok")
	if err := os.WriteFile(tokenPath, []byte(testTaskToken), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (int, string) {
		c := exec.Command(bin, args...)
		c.Env = append(os.Environ(), "HOME="+dir, "ARCHIVIST_TOKEN=")
		c.Stdin = strings.NewReader("")
		out, _ := c.CombinedOutput()
		return c.ProcessState.ExitCode(), string(out)
	}
	if code, out := run("mcp", "serve", "--token-file", tokenPath, "--publish-session", testPublishSession, "--publish-dir", dir); code != 0 || !strings.Contains(out, "6 tools registered") {
		t.Fatalf("publish flags: exit %d %s", code, out)
	}
	for _, args := range [][]string{
		{"--token-file", tokenPath, "--publish-session", testPublishSession},
		{"--token-file", tokenPath, "--publish-dir", dir},
		{"--token-file", tokenPath, "--publish-session", "not-a-uuid", "--publish-dir", dir},
		{"--token-file", tokenPath, "--publish-session", testPublishSession, "--publish-dir", "relative"},
		{"--token", "ak_00000000000", "--publish-session", testPublishSession, "--publish-dir", dir},
	} {
		if code, out := run(append([]string{"mcp", "serve"}, args...)...); code != 2 {
			t.Errorf("%v: exit %d %s", args, code, out)
		}
	}
}
