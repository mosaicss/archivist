//go:build !windows

package connect

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/client"
)

// ─── shared binaries ─────────────────────────────────────────────────────────

var (
	binOnce    sync.Once
	binDir     string
	binErr     error
	fakeClaude string
	archivist  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		_ = os.RemoveAll(binDir)
	}
	os.Exit(code)
}

// testBinaries builds the fake Claude Code and the real archivist binary
// (run by the fake as `archivist mcp serve`).
func testBinaries(t *testing.T) (claudeBin, archivistBin string) {
	t.Helper()
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "connect-test-bin-")
		if binErr != nil {
			return
		}
		fakeClaude = filepath.Join(binDir, "claude")
		archivist = filepath.Join(binDir, "archivist")
		for _, b := range []struct{ out, pkg string }{
			{fakeClaude, "./testdata/fakeclaude"},
			{archivist, "../../cmd/archivist"},
		} {
			cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
			if out, err := cmd.CombinedOutput(); err != nil {
				binErr = fmt.Errorf("go build %s: %v\n%s", b.pkg, err, out)
				return
			}
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return fakeClaude, archivist
}

// ─── fake chat-api (78.15 routes) ────────────────────────────────────────────

const testOwner = "user_connecttest"

type fakeToken struct {
	id, token, session string
	revoked            bool
}

type fakeChatAPI struct {
	t       *testing.T
	srv     *httptest.Server
	key     []byte
	mu      sync.Mutex
	session map[string]*client.AgentSession
	tokens  map[string]*fakeToken
	bearers []string // Authorization seen on research routes
	mints   int
	tickets map[string]int // relay tickets minted per session id ("" = user scope)
	off     bool
	// ticketTTL shortens relay tickets (socket rotation tests); default 5 min.
	ticketTTL time.Duration
	// tokenTTL shortens task tokens (refresh tests); default: the requested TTL.
	tokenTTL time.Duration
	// failMints makes the next N task token mints answer 503.
	failMints int
	failed    int
}

func newFakeChatAPI(t *testing.T, key []byte) *fakeChatAPI {
	f := &fakeChatAPI{t: t, key: key, session: map[string]*client.AgentSession{}, tokens: map[string]*fakeToken{},
		tickets: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// addSession registers an active owner session (what POST /agent-sessions does).
func (f *fakeChatAPI) addSession(agent string) *client.AgentSession {
	now := time.Now().UnixMilli()
	s := &client.AgentSession{SessionID: randomUUID(), OwnerID: testOwner, Agent: agent, Title: "test session",
		Status: "active", CreatedAt: now, ExpiresAt: now + 3_600_000}
	f.mu.Lock()
	f.session[s.SessionID] = s
	f.mu.Unlock()
	return s
}

func (f *fakeChatAPI) endSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UnixMilli()
	f.session[id].Status = "ended"
	f.session[id].EndedAt = &now
}

func (f *fakeChatAPI) liveTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, tk := range f.tokens {
		if !tk.revoked {
			out = append(out, tk.id)
		}
	}
	return out
}

// tokenState returns minted count, injected failures, and live/revoked token values.
func (f *fakeChatAPI) tokenState() (mints, failed int, live, revoked []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tk := range f.tokens {
		if tk.revoked {
			revoked = append(revoked, tk.token)
		} else {
			live = append(live, tk.token)
		}
	}
	return f.mints, f.failed, live, revoked
}

func (f *fakeChatAPI) setFailMints(n int) {
	f.mu.Lock()
	f.failMints = n
	f.mu.Unlock()
}

func (f *fakeChatAPI) researchBearers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bearers...)
}

func (f *fakeChatAPI) ticket(sid string, ttl time.Duration) string {
	now := time.Now().UnixMilli()
	claims, _ := json.Marshal(map[string]any{"v": 1, "sub": testOwner, "sid": sid, "iat": now, "exp": now + ttl.Milliseconds()})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, f.key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

var researchRoute = regexp.MustCompile(`^/research/`)

func (f *fakeChatAPI) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	body, _ := io.ReadAll(r.Body)
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if researchRoute.MatchString(r.URL.Path) {
		f.bearers = append(f.bearers, auth)
		tok := strings.TrimPrefix(auth, "Bearer ")
		for _, tk := range f.tokens {
			if tk.token == tok && tk.revoked {
				reply(401, map[string]any{"error": "Invalid task token.", "code": "INVALID_TASK_TOKEN"})
				return
			}
		}
		reply(200, map[string]any{"results": []any{map[string]any{"id": "c1", "url": "https://example.test/filings/f1"}},
			"entity_resolution": nil, "truncated": false, "next_cursor": nil})
		return
	}
	if f.off {
		reply(404, map[string]any{"error": "The requested endpoint does not exist.", "code": "FEATURE_DISABLED"})
		return
	}
	if !strings.HasPrefix(auth, "Bearer ak_") {
		reply(403, map[string]any{"error": "A verified API key is required.", "code": "PARENT_KEY_REQUIRED"})
		return
	}
	active := func(id string) *client.AgentSession {
		s := f.session[id]
		if s == nil || s.Status != "active" || s.ExpiresAt <= time.Now().UnixMilli() {
			return nil
		}
		return s
	}
	switch {
	case r.Method == "POST" && r.URL.Path == "/relay-tickets":
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		sid, scope := "user:"+testOwner, ""
		if in["scope"] == "session" {
			if active(in["sessionId"]) == nil {
				reply(409, map[string]any{"error": "Session is not active.", "code": "SESSION_INACTIVE"})
				return
			}
			sid, scope = in["sessionId"], in["sessionId"]
		}
		ttl := f.ticketTTL
		if ttl == 0 {
			ttl = 5 * time.Minute
		}
		f.tickets[scope]++
		reply(201, map[string]any{"ticket": f.ticket(sid, ttl), "expiresAt": time.Now().Add(ttl).UnixMilli(), "scope": in["scope"]})
	case r.Method == "POST" && r.URL.Path == "/task-tokens":
		var in struct {
			SessionID string   `json:"sessionId"`
			Scopes    []string `json:"scopes"`
			TTL       int      `json:"ttlSeconds"`
		}
		_ = json.Unmarshal(body, &in)
		if active(in.SessionID) == nil || in.TTL > 900 || strings.Join(in.Scopes, ",") != "search,read" {
			reply(400, map[string]any{"error": "Invalid request fields.", "code": "BAD_REQUEST"})
			return
		}
		if f.failMints > 0 {
			f.failMints--
			f.failed++
			reply(503, map[string]any{"error": "Temporarily unavailable.", "code": "UNAVAILABLE"})
			return
		}
		f.mints++
		secret := make([]byte, 32)
		_, _ = rand.Read(secret)
		id := randomUUID()
		tok := "mst_" + id + "." + base64.RawURLEncoding.EncodeToString(secret)
		f.tokens[id] = &fakeToken{id: id, token: tok, session: in.SessionID}
		ttl := time.Duration(in.TTL) * time.Second
		if f.tokenTTL > 0 {
			ttl = f.tokenTTL
		}
		reply(201, map[string]any{"token": tok, "tokenId": id, "expiresAt": time.Now().Add(ttl).UnixMilli(), "scopes": in.Scopes})
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/task-tokens/"):
		tk := f.tokens[strings.TrimPrefix(r.URL.Path, "/task-tokens/")]
		if tk == nil {
			reply(404, map[string]any{"error": "Task token not found.", "code": "TASK_TOKEN_NOT_FOUND"})
			return
		}
		tk.revoked = true
		w.WriteHeader(204)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/agent-sessions/"):
		s := f.session[strings.TrimPrefix(r.URL.Path, "/agent-sessions/")]
		if s == nil {
			reply(404, map[string]any{"error": "Session not found.", "code": "SESSION_NOT_FOUND"})
			return
		}
		reply(200, s)
	default:
		reply(404, map[string]any{"error": "not found", "code": "NOT_FOUND"})
	}
}

// ─── fake Claude configuration and records ──────────────────────────────────

func writeFakeClaudeConfig(t *testing.T, home string, cfg map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".fakeclaude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeRun struct {
	Args    []string `json:"args"`
	EnvKeys []string `json:"envKeys"`
	Cwd     string   `json:"cwd"`
	PID     int      `json:"pid"`
}

func fakeRuns(t *testing.T, home string) []fakeRun {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(home, ".fakeclaude", "runs"))
	var out []fakeRun
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(home, ".fakeclaude", "runs", e.Name()))
		if err != nil {
			continue
		}
		var r fakeRun
		if json.Unmarshal(b, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// processAlive reports whether pid exists (signal 0).
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

func randomHexKey() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}
