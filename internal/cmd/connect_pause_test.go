package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/service"
)

// Story 94.6: Pause on this computer.

const pauseOwner = "user_pausetest"

// fastPause shortens the pause timings for one test.
func fastPause(t *testing.T) {
	t.Helper()
	w, p, f, m := pauseWatchEvery, pausePollEvery, pauseRetryFirst, pauseRetryMax
	pauseWatchEvery, pausePollEvery, pauseRetryFirst, pauseRetryMax = 10*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { pauseWatchEvery, pausePollEvery, pauseRetryFirst, pauseRetryMax = w, p, f, m })
}

// testTicket is a relay ticket whose claims name owner.
func testTicket(owner string) string {
	claims, _ := json.Marshal(map[string]any{"v": 1, "sub": owner, "sid": "user:" + owner})
	return base64.RawURLEncoding.EncodeToString(claims) + "." + strings.Repeat("s", 43)
}

// fakeMinter mints user relay tickets for pauseOwner.
type fakeMinter struct {
	mu    sync.Mutex
	mints int
	err   error
}

func (f *fakeMinter) MintRelayTicket(_ context.Context, sessionID string) (*client.RelayTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mints++
	if sessionID != "" {
		return nil, errors.New("a session ticket was asked for")
	}
	if f.err != nil {
		return nil, f.err
	}
	return &client.RelayTicket{Ticket: testTicket(pauseOwner), ExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli()}, nil
}

// fakePauseRelay serves the relay's pause routes (and, with ws set, the
// daemon's user socket).
type fakePauseRelay struct {
	t   *testing.T
	srv *httptest.Server
	mu  sync.Mutex
	// post and get answer the routes: a status and a "paused" value.
	post, get func(n int) (int, bool)
	posts     int
	gets      int
	deletes   int
	// onDelete runs as a DELETE arrives (uninstall ordering checks);
	// delStatus answers it (200 when 0).
	onDelete  func()
	delStatus int
	calls     []string
	// ws: the user socket (integration test).
	dials  int
	frames []map[string]any
	user   *websocket.Conn
	closed chan struct{}
}

func newFakePauseRelay(t *testing.T) *fakePauseRelay {
	f := &fakePauseRelay{t: t, closed: make(chan struct{}, 8),
		post: func(int) (int, bool) { return http.StatusOK, true },
		get:  func(int) (int, bool) { return http.StatusOK, true }}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePauseRelay) wsURL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakePauseRelay) serve(w http.ResponseWriter, r *http.Request) {
	// The pause routes take exactly role=daemon; the user socket may also
	// carry the signing flag (Story 94.7).
	query := r.URL.RawQuery
	if strings.HasSuffix(r.URL.Path, "/ws") {
		query = strings.TrimSuffix(query, "&signing=1")
	}
	if r.Header.Get("Authorization") != "Bearer "+testTicket(pauseOwner) || query != "role=daemon" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	switch r.URL.Path {
	case "/users/" + pauseOwner + "/pause":
		f.mu.Lock()
		f.calls = append(f.calls, r.Method)
		var status int
		var paused bool
		switch r.Method {
		case http.MethodPost:
			f.posts++
			status, paused = f.post(f.posts)
		case http.MethodGet:
			f.gets++
			status, paused = f.get(f.gets)
		case http.MethodDelete:
			f.deletes++
			if f.onDelete != nil {
				f.onDelete()
			}
			status = http.StatusOK
			if f.delStatus != 0 {
				status = f.delStatus
			}
		default:
			status = http.StatusMethodNotAllowed
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]bool{"paused": paused})
		}
	case "/users/" + pauseOwner + "/ws":
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.dials++
		f.user = c
		f.mu.Unlock()
		ctx := context.Background()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				f.closed <- struct{}{}
				return
			}
			if string(data) == "ping" {
				_ = c.Write(ctx, websocket.MessageText, []byte("pong"))
				continue
			}
			var x map[string]any
			if json.Unmarshal(data, &x) == nil {
				f.mu.Lock()
				f.frames = append(f.frames, x)
				f.mu.Unlock()
			}
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakePauseRelay) snapshot() (posts, gets, dials int, calls []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.gets, f.dials, append([]string(nil), f.calls...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// syncBuffer is a log sink the loop and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runs drives serve with a fake daemon run: each run records itself and
// waits for its context; the test pauses through the loop, as the daemon's
// Config.OnPause does.
type runs struct {
	mu      sync.Mutex
	n       int
	loop    *pauseLoop
	started chan int
}

func newRuns(loop *pauseLoop) *runs { return &runs{loop: loop, started: make(chan int, 16)} }

func (r *runs) run(ctx context.Context) error {
	r.mu.Lock()
	r.n++
	n := r.n
	r.mu.Unlock()
	r.started <- n
	<-ctx.Done()
	return nil
}

func (r *runs) pause() { r.loop.pause() }

func (r *runs) await(t *testing.T, n int) {
	t.Helper()
	select {
	case got := <-r.started:
		if got != n {
			t.Fatalf("run %d started, want %d", got, n)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("run %d never started", n)
	}
}

func startLoop(t *testing.T, relay *fakePauseRelay, minter *fakeMinter) (string, *runs, *syncBuffer, func() error) {
	t.Helper()
	fastPause(t)
	stateDir := filepath.Join(t.TempDir(), "connect")
	logs := &syncBuffer{}
	loop := newPauseLoop(stateDir, relay.wsURL(), minter, connect.NewLogger(logs))
	r := newRuns(loop)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.serve(ctx, r.run) }()
	stop := func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not return")
			return nil
		}
	}
	t.Cleanup(func() { cancel() })
	return stateDir, r, logs, stop
}

// Workspace Pause: the run ends, the marker is written, the pause is
// registered (POST) and polled (GET) until the relay answers not paused,
// then the marker goes and a new run starts.
func TestPauseLoopWorkspacePauseAndResume(t *testing.T) {
	relay := newFakePauseRelay(t)
	relay.get = func(n int) (int, bool) { return http.StatusOK, n < 3 }
	stateDir, r, logs, stop := startLoop(t, relay, &fakeMinter{})
	r.await(t, 1)
	r.pause()
	r.await(t, 2)
	posts, gets, _, calls := relay.snapshot()
	if posts != 1 || gets != 3 || calls[0] != http.MethodPost {
		t.Fatalf("posts %d gets %d calls %v", posts, gets, calls)
	}
	if pausedHere(stateDir) {
		t.Fatal("marker kept after the resume")
	}
	if err := stop(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	for _, want := range []string{"pause registered with the relay", "resumed from Mosaic", "resumed on this computer: connecting"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

// CLI pause (the marker) against an older relay (404): paused here only,
// never resumed by the relay, until the marker is removed (CLI resume).
func TestPauseLoopCLIPauseOldRelayStaysPausedUntilCLIResume(t *testing.T) {
	relay := newFakePauseRelay(t)
	relay.post = func(int) (int, bool) { return http.StatusNotFound, false }
	stateDir, r, logs, stop := startLoop(t, relay, &fakeMinter{})
	r.await(t, 1)
	if err := writePauseMarker(stateDir); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three registration attempts", func() bool { p, _, _, _ := relay.snapshot(); return p >= 3 })
	if _, gets, _, _ := relay.snapshot(); gets != 0 {
		t.Fatalf("an unregistered pause was read %d times", gets)
	}
	if err := removePauseMarker(stateDir); err != nil {
		t.Fatal(err)
	}
	r.await(t, 2)
	if err := stop(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if n := strings.Count(logs.String(), "the relay does not know Pause yet"); n != 1 {
		t.Fatalf("old relay line %d times:\n%s", n, logs.String())
	}
}

// "Not paused" counts only after the relay registered the pause: failed
// registrations are retried, and a registration answered "not paused" (a
// workspace Resume was waiting) resumes at once.
func TestPauseLoopRegistersBeforeReadingTheRelay(t *testing.T) {
	relay := newFakePauseRelay(t)
	relay.post = func(n int) (int, bool) {
		if n < 3 {
			return http.StatusServiceUnavailable, false
		}
		return http.StatusOK, false
	}
	relay.get = func(int) (int, bool) { return http.StatusOK, false }
	stateDir, r, _, stop := startLoop(t, relay, &fakeMinter{})
	r.await(t, 1)
	r.pause()
	r.await(t, 2)
	posts, gets, _, _ := relay.snapshot()
	if posts != 3 || gets != 0 || pausedHere(stateDir) {
		t.Fatalf("posts %d gets %d paused %v", posts, gets, pausedHere(stateDir))
	}
	_ = stop()
}

// Paused at start (a restart while paused): no run until the resume, and
// stopping while paused returns nil.
func TestPauseLoopStartsPausedAndStopsCleanly(t *testing.T) {
	fastPause(t)
	relay := newFakePauseRelay(t)
	stateDir := filepath.Join(t.TempDir(), "connect")
	if err := writePauseMarker(stateDir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.Stat(pauseMarkerPath(stateDir)); err != nil || (runtime.GOOS != "windows" && b.Mode().Perm() != 0o600) {
		t.Fatalf("marker %v %v", b, err)
	}
	loop := newPauseLoop(stateDir, relay.wsURL(), &fakeMinter{}, connect.NewLogger(&syncBuffer{}))
	ctx, cancel := context.WithCancel(context.Background())
	r := newRuns(loop)
	done := make(chan error, 1)
	go func() { done <- loop.serve(ctx, r.run) }()
	waitFor(t, "a few polls", func() bool { _, g, _, _ := relay.snapshot(); return g >= 2 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	r.mu.Lock()
	n := r.n
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d runs while paused", n)
	}
	if !pausedHere(stateDir) {
		t.Fatal("stopping removed the pause")
	}
}

// A refused key while paused ends the loop with the typed fatal error.
func TestPauseLoopRevokedKeyIsFatal(t *testing.T) {
	fastPause(t)
	relay := newFakePauseRelay(t)
	stateDir := filepath.Join(t.TempDir(), "connect")
	if err := writePauseMarker(stateDir); err != nil {
		t.Fatal(err)
	}
	minter := &fakeMinter{err: &client.APIError{Status: http.StatusUnauthorized, Code: "UNAUTHORIZED", Message: "Unauthorized"}}
	loop := newPauseLoop(stateDir, relay.wsURL(), minter, connect.NewLogger(&syncBuffer{}))
	err := loop.serve(context.Background(), func(context.Context) error {
		t.Fatal("ran while paused")
		return nil
	})
	var fatal *connect.FatalError
	if !errors.As(err, &fatal) || !fatal.Auth {
		t.Fatalf("got %v", err)
	}
}

// A marker removed while the paused run is still ending resumes at once:
// serve never returns while ctx lives (the service would stay stopped).
func TestPauseLoopResumeDuringShutdownRunsAgain(t *testing.T) {
	relay := newFakePauseRelay(t)
	fastPause(t)
	stateDir := filepath.Join(t.TempDir(), "connect")
	loop := newPauseLoop(stateDir, relay.wsURL(), &fakeMinter{}, connect.NewLogger(&syncBuffer{}))
	started := make(chan int, 4)
	n := 0
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- loop.serve(ctx, func(runCtx context.Context) error {
			n++
			started <- n
			<-runCtx.Done()
			if n == 1 {
				// The CLI resume lands while this run shuts down.
				if err := removePauseMarker(stateDir); err != nil {
					t.Error(err)
				}
			}
			return nil
		})
	}()
	<-started
	loop.pause()
	select {
	case got := <-started:
		if got != 2 {
			t.Fatalf("run %d", got)
		}
	case err := <-done:
		t.Fatalf("serve returned %v while ctx lives", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no second run")
	}
	if posts, gets, _, _ := relay.snapshot(); posts != 0 || gets != 0 {
		t.Fatalf("posts %d gets %d", posts, gets)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
}

// The marker records the registration: a daemon restarted while paused
// reads (GET) first and never registers again, so a Resume clicked while
// the computer was off is taken on the first read.
func TestPauseLoopRestartAfterRegistrationReadsFirst(t *testing.T) {
	relay := newFakePauseRelay(t)
	stateDir, r, _, stop := startLoop(t, relay, &fakeMinter{})
	r.await(t, 1)
	r.pause()
	waitFor(t, "the registration", func() bool { p, _, _, _ := relay.snapshot(); return p == 1 })
	waitFor(t, "the registered line", func() bool { return pauseRegistered(stateDir) })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	// The restart: the relay now holds a Resume (GET answers not paused).
	relay2 := newFakePauseRelay(t)
	relay2.get = func(int) (int, bool) { return http.StatusOK, false }
	loop := newPauseLoop(stateDir, relay2.wsURL(), &fakeMinter{}, connect.NewLogger(&syncBuffer{}))
	r2 := newRuns(loop)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- loop.serve(ctx, r2.run) }()
	r2.await(t, 1)
	if posts, gets, _, calls := relay2.snapshot(); posts != 0 || gets != 1 || calls[0] != http.MethodGet {
		t.Fatalf("restart: posts %d gets %d calls %v", posts, gets, calls)
	}
	if pausedHere(stateDir) {
		t.Fatal("marker kept after the resume")
	}
	cancel()
	<-done
}

// A registered line is only ever added to an existing marker: a CLI resume
// that removed it is never undone.
func TestMarkPauseRegisteredNeverRecreatesTheMarker(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "connect")
	if err := markPauseRegistered(stateDir); err != nil || pausedHere(stateDir) {
		t.Fatalf("got %v, paused %v", err, pausedHere(stateDir))
	}
	if err := writePauseMarker(stateDir); err != nil {
		t.Fatal(err)
	}
	if pauseRegistered(stateDir) {
		t.Fatal("a fresh marker reads registered")
	}
	if err := markPauseRegistered(stateDir); err != nil || !pauseRegistered(stateDir) {
		t.Fatalf("got %v, registered %v", err, pauseRegistered(stateDir))
	}
}

// Uninstall of a paused computer clears the relay pause (DELETE with a
// ticket minted from the saved key) before the key is revoked.
func TestConnectUninstallClearsTheRelayPause(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	var mu sync.Mutex
	var order []string
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+pairedKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/relay-tickets":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": testTicket(pauseOwner),
				"expiresAt": time.Now().Add(5 * time.Minute).UnixMilli(), "scope": "user"})
		case r.Method == http.MethodDelete && r.URL.Path == "/account/cli-tokens/current":
			mu.Lock()
			order = append(order, "revoke")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(chat.Close)
	t.Setenv("ARCHIVIST_BASE_URL", chat.URL)
	relay := newFakePauseRelay(t)
	relay.onDelete = func() {
		mu.Lock()
		order = append(order, "clear pause")
		mu.Unlock()
	}
	t.Setenv("ARCHIVIST_RELAY_URL", relay.wsURL())
	if err := writePauseMarker(filepath.Join(home, ".archivist", "connect")); err != nil {
		t.Fatal(err)
	}
	out, code := runRoot(t, "connect", "--uninstall")
	if code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != "clear pause,revoke" {
		t.Fatalf("order %v\n%s", order, out)
	}
	// Not paused: no relay call at all.
	order = nil
	mu.Unlock()
	saveKey(t, home, pairedKey)
	out, code = runRoot(t, "connect", "--uninstall")
	mu.Lock()
	if code != 0 || strings.Join(order, ",") != "revoke" {
		t.Fatalf("second uninstall: exit %d order %v\n%s", code, order, out)
	}
	// An older relay (no pause route, 404) never showed a pause: no note.
	relay.delStatus = http.StatusNotFound
	mu.Unlock()
	saveKey(t, home, pairedKey)
	if err := writePauseMarker(filepath.Join(home, ".archivist", "connect")); err != nil {
		t.Fatal(err)
	}
	out, code = runRoot(t, "connect", "--uninstall")
	mu.Lock()
	if code != 0 || strings.Contains(out, "may show this computer as paused") {
		t.Fatalf("old relay uninstall: exit %d\n%s", code, out)
	}
}

// A relay that cannot be reached costs one note; the uninstall goes on.
func TestConnectUninstallRelayPauseClearIsBestEffort(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	revoked := revokeServer(t) // answers /relay-tickets 404
	t.Setenv("ARCHIVIST_RELAY_URL", "ws://127.0.0.1:1")
	if err := writePauseMarker(filepath.Join(home, ".archivist", "connect")); err != nil {
		t.Fatal(err)
	}
	out, code := runRoot(t, "connect", "--uninstall")
	if code != 0 || len(revoked()) != 1 || !strings.Contains(out, "Note: Mosaic may show this computer as paused for a while") {
		t.Fatalf("exit %d revoked %v\n%s", code, revoked(), out)
	}
	if _, err := os.Stat(filepath.Join(home, ".archivist", "connect")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("connect dir kept: %v", err)
	}
}

func TestRelayHTTPURL(t *testing.T) {
	for in, want := range map[string]string{
		"wss://relay.mosaic-finance.com":  "https://relay.mosaic-finance.com",
		"wss://relay.mosaic-finance.com/": "https://relay.mosaic-finance.com",
		"ws://127.0.0.1:8787":             "http://127.0.0.1:8787",
		"https://relay.example.test":      "https://relay.example.test",
	} {
		if got := relayHTTPURL(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

// The CLI flags: --pause writes the marker, --resume removes it, both say
// so, and neither combines with another setup flag.
func TestConnectPauseResumeFlags(t *testing.T) {
	home := sandbox(t)
	stateDir := filepath.Join(home, ".archivist", "connect")
	out, code := runRoot(t, "connect", "--resume")
	if code != 0 || !strings.Contains(out, "not paused") {
		t.Fatalf("resume before pause: exit %d\n%s", code, out)
	}
	out, code = runRoot(t, "connect", "--pause")
	if code != 0 || !strings.Contains(out, "Paused on this computer.") || !pausedHere(stateDir) {
		t.Fatalf("pause: exit %d\n%s", code, out)
	}
	if out, code = runRoot(t, "connect", "--pause"); code != 0 || !strings.Contains(out, "Already paused") {
		t.Fatalf("second pause: exit %d\n%s", code, out)
	}
	stubRunner(t, func(string) service.Result { return service.Result{ExitCode: 3} })
	out, _ = runRoot(t, "connect", "--status")
	if !strings.Contains(out, "Paused:   yes") {
		t.Fatalf("status lacks the Paused line:\n%s", out)
	}
	out, code = runRoot(t, "connect", "--resume")
	if code != 0 || !strings.Contains(out, "Resumed on this computer. Run 'archivist connect' to connect.") || pausedHere(stateDir) {
		t.Fatalf("resume: exit %d\n%s", code, out)
	}
	if out, _ = runRoot(t, "connect", "--status"); strings.Contains(out, "Paused:") {
		t.Fatalf("status still paused:\n%s", out)
	}
	for _, args := range [][]string{
		{"connect", "--pause", "--resume"},
		{"connect", "--pause", "--install"},
		{"connect", "--resume", "--status"},
		{"connect", "--pause", "--check"},
		{"connect", "--ask"},
		{"connect", "--pause", "--ask"},
	} {
		if out, code := runRoot(t, args...); code != ExitUsageError {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
}

// The acceptance check against the real daemon: it declares the pause
// feature; {kind:"pause"} from the relay closes its socket, writes the
// marker and registers the pause (POST); when the relay answers "not
// paused" (GET) it connects again.
func TestConnectDaemonPausesFromTheRelayAndReconnects(t *testing.T) {
	skipShellStubsInternal(t)
	fastPause(t)
	home := sandbox(t)
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo '2.1.280 (Claude Code)';;\n" +
		"auth) echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"subscriptionType\":\"max\"}';;\n" +
		"*) exit 2;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/relay-tickets" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"ticket": testTicket(pauseOwner),
				"expiresAt": time.Now().Add(5 * time.Minute).UnixMilli(), "scope": "user"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(chat.Close)
	relay := newFakePauseRelay(t)
	resume := make(chan struct{})
	relay.get = func(int) (int, bool) {
		select {
		case <-resume:
			return http.StatusOK, false
		default:
			return http.StatusOK, true
		}
	}
	t.Setenv("ARCHIVIST_BASE_URL", chat.URL)
	t.Setenv("ARCHIVIST_RELAY_URL", relay.wsURL())
	t.Setenv("ARCHIVIST_TOKEN", "ak_00000000000000000000")

	root := NewRootCmd("v0.2.35", "abc1234", "2026-10-21")
	var out syncBuffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"connect"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	t.Cleanup(cancel)

	features := func() bool {
		relay.mu.Lock()
		defer relay.mu.Unlock()
		for _, f := range relay.frames {
			if f["kind"] == "features" {
				list, _ := f["features"].([]any)
				return len(list) == 1 && list[0] == "pause"
			}
		}
		return false
	}
	waitFor(t, "the features frame", features)
	relay.mu.Lock()
	user := relay.user
	relay.mu.Unlock()
	if err := user.Write(context.Background(), websocket.MessageText, []byte(`{"kind":"pause"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-relay.closed:
	case <-time.After(10 * time.Second):
		t.Fatalf("the daemon kept its socket:\n%s", out.String())
	}
	stateDir := filepath.Join(home, ".archivist", "connect")
	waitFor(t, "the pause registration", func() bool { p, _, _, _ := relay.snapshot(); return p == 1 })
	if !pausedHere(stateDir) {
		t.Fatal("no marker after the relay's pause")
	}
	waitFor(t, "a pause read", func() bool { _, g, _, _ := relay.snapshot(); return g >= 1 })
	if _, _, dials, _ := relay.snapshot(); dials != 1 {
		t.Fatalf("dialled %d times while paused", dials)
	}
	close(resume)
	waitFor(t, "the reconnect", func() bool { _, _, d, _ := relay.snapshot(); return d == 2 })
	if pausedHere(stateDir) {
		t.Fatal("marker kept after the resume")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("connect: %v\n%s", err, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("connect did not stop")
	}
	for _, want := range []string{"paused from Mosaic", "pause registered with the relay", "resumed from Mosaic"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, out.String())
		}
	}
}

func skipShellStubsInternal(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix only: the harness stand-in is a shell script")
	}
}
