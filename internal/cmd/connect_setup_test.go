package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/service"
)

const pairedKey = "ak_pairedsecretvalue0123456789"

func runRoot(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := NewRootCmd("v0.2.26", "abc1234", "2026-10-04")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	var exitErr *ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exitErr):
		return out.String(), exitErr.Code
	default:
		return out.String() + err.Error(), ExitUsageError // main maps cobra errors to 2
	}
}

// sandbox points HOME at a temp dir and clears credentials from the env.
func sandbox(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	setHome(t, home)
	t.Setenv("ARCHIVIST_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	// On Windows --service would otherwise supervise a copy of the test
	// binary; these tests run the daemon itself (Story 78.34).
	t.Setenv("ARCHIVIST_SERVICE_SUPERVISED", "1")
	return home
}

// stubRunner replaces the service manager runner for one test.
func stubRunner(t *testing.T, answer func(line string) service.Result) *[]string {
	t.Helper()
	var calls []string
	prev := serviceRunner
	serviceRunner = func(_ context.Context, name string, args ...string) (service.Result, error) {
		line := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, line)
		return answer(line), nil
	}
	t.Cleanup(func() { serviceRunner = prev })
	return &calls
}

func TestConnectSetupFlagsAreExclusive(t *testing.T) {
	sandbox(t)
	calls := stubRunner(t, func(string) service.Result { return service.Result{} })
	for _, args := range [][]string{
		{"connect", "--pair", "ABCDE-FGHJK", "--install"},
		{"connect", "--install", "--status"},
		{"connect", "--uninstall", "--status"},
		{"connect", "--status", "--check"},
		{"connect", "--install", "--session", "0f8fad5b-d9cb-469f-a165-70867728950e"},
		{"connect", "--pair", "ABCDE-FGHJK", "--agent", "claude"},
		{"connect", "--service", "--check"},
		{"connect", "--service", "--install"},
		{"connect", "--pair", "ABCDE-FGHJK", "--claude-model", "opus"},
		{"connect", "--install", "--codex-effort", "high"},
		{"connect", "stray"},
		{"connect", "--check", "stray"},
		// Story 78.32: --max-permission only with --install or --service, a
		// valid mode with --install (--service logs a bad one and exits 0);
		// --mode, --model and --effort never with a setup flag.
		{"connect", "--status", "--max-permission", "ask"},
		{"connect", "--uninstall", "--max-permission", "ask"},
		{"connect", "--pair", "ABCDE-FGHJK", "--max-permission", "ask"},
		{"connect", "--install", "--max-permission", "root"},
		{"connect", "--install", "--mode", "ask"},
		{"connect", "--install", "--model", "opus"},
		{"connect", "--install", "--effort", "high"},
		{"connect", "--status", "--mode", "ask"},
		{"connect", "--service", "--mode", "read_only"},
		// Story 78.33: --web-search only where connect runs (not yet for background connect).
		{"connect", "--install", "--web-search"},
		{"connect", "--install", "--web-search=false"},
		{"connect", "--service", "--web-search"},
		{"connect", "--status", "--web-search"},
		{"connect", "--uninstall", "--web-search"},
		{"connect", "--pair", "ABCDE-FGHJK", "--web-search"},
	} {
		if out, code := runRoot(t, args...); code != ExitUsageError {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("a refused combination ran %v", *calls)
	}
}

// pairServer answers POST /agent-pairing/redeem with status and body and
// records what it saw.
func pairServer(t *testing.T, status int, body string, seen *map[string]string, hits *atomic.Int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/agent-pairing/redeem" || r.Method != http.MethodPost {
			w.WriteHeader(500)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("redeem sent Authorization")
		}
		b, _ := io.ReadAll(r.Body)
		if seen != nil {
			_ = json.Unmarshal(b, seen)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
}

func TestConnectPairSavesKeyAndNeverPrintsIt(t *testing.T) {
	home := sandbox(t)
	var hits atomic.Int32
	seen := map[string]string{}
	pairServer(t, 200, `{"key":"`+pairedKey+`","key_id":"ak_kid","name":"archivist box 2026-10-04 1200Z ab12","tier":"pro"}`, &seen, &hits)
	t.Setenv("ARCHIVIST_TOKEN", "ak_envshadow0000000000")
	out, code := runRoot(t, "connect", "--pair", "abcde", "fghjk")
	if code != 0 || hits.Load() != 1 {
		t.Fatalf("exit %d hits %d\n%s", code, hits.Load(), out)
	}
	if seen["code"] != "ABCDEFGHJK" || seen["device_name"] == "" {
		t.Fatalf("request %v", seen)
	}
	for _, want := range []string{"Paired as archivist box 2026-10-04 1200Z ab12.", "ak_paireds...789", "ARCHIVIST_TOKEN is set"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, pairedKey) {
		t.Fatalf("output leaks the key:\n%s", out)
	}
	path := filepath.Join(home, ".archivist", "credentials")
	b, err := os.ReadFile(path)
	if err != nil || string(b) != pairedKey+"\n" {
		t.Fatalf("credentials %q %v", b, err)
	}
	if info, _ := os.Stat(path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
}

func TestConnectPairRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		exit   int
		says   string
	}{
		{"invalid", 400, `{"error":"This pairing code is not valid.","code":"PAIRING_CODE_INVALID"}`, ExitAuthError, "Get a new code in Mosaic"},
		{"pro", 403, `{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED","account_url":"https://mosaic-finance.com/en/pricing/"}`, ExitAuthError, "Pro account"},
		{"feature off", 404, `{"error":"The requested endpoint does not exist.","code":"FEATURE_DISABLED"}`, ExitGenericError, "FEATURE_DISABLED"},
		{"too big", 413, `{"error":"Request body too large.","code":"PAYLOAD_TOO_LARGE"}`, ExitGenericError, "too large"},
		{"flood", 429, `{"error":"Too many requests.","code":"RATE_LIMITED"}`, ExitRateLimit, "RATE_LIMITED"},
		{"clerk", 502, `{"error":"Failed to create CLI token.","code":"CLERK_ERROR"}`, ExitServerError, "CLERK_ERROR"},
		{"unavailable", 503, `{"error":"Unavailable","code":"SERVICE_UNAVAILABLE"}`, ExitServerError, "Unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := sandbox(t)
			var hits atomic.Int32
			pairServer(t, tc.status, tc.body, nil, &hits)
			out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
			if code != tc.exit || !strings.Contains(out, tc.says) || hits.Load() != 1 {
				t.Fatalf("exit %d hits %d\n%s", code, hits.Load(), out)
			}
			if _, err := os.Stat(filepath.Join(home, ".archivist", "credentials")); err == nil {
				t.Fatal("a refused pairing wrote credentials")
			}
		})
	}
	t.Run("malformed code is never sent", func(t *testing.T) {
		sandbox(t)
		var hits atomic.Int32
		pairServer(t, 200, `{}`, nil, &hits)
		for _, code := range []string{"ABCDE", "ABCDE-FGHJU", ""} {
			out, exit := runRoot(t, "connect", "--pair", code)
			if exit != ExitAuthError || !strings.Contains(out, "Get a new code in Mosaic") {
				t.Errorf("%q: exit %d\n%s", code, exit, out)
			}
		}
		if hits.Load() != 0 {
			t.Fatalf("malformed codes reached the server %d times", hits.Load())
		}
	})
	t.Run("network error", func(t *testing.T) {
		sandbox(t)
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		t.Setenv("ARCHIVIST_BASE_URL", url)
		if out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK"); code != ExitServerError {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
}

func saveKey(t *testing.T, home, key string) {
	t.Helper()
	dir := filepath.Join(home, ".archivist")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials"), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConnectInstallNeedsSavedKey(t *testing.T) {
	if !connect.Supported() {
		t.Skip("no background service here")
	}
	sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "ak_onlyinenv000000000000")
	calls := stubRunner(t, func(string) service.Result { return service.Result{} })
	out, code := runRoot(t, "connect", "--install")
	if code != ExitAuthError || !strings.Contains(out, "--pair CODE") || len(*calls) != 0 {
		t.Fatalf("exit %d calls %v\n%s", code, *calls, out)
	}
}

// Install, status and uninstall against a stub service manager (Linux
// systemd here; the launchd flow is covered in internal/service).
func TestConnectInstallStatusUninstallLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd flow")
	}
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	t.Setenv("ARCHIVIST_TOKEN", "ak_mustnotreachunit00000")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/x/.claude-b")
	active := false
	calls := stubRunner(t, func(line string) service.Result {
		if strings.Contains(line, "is-active") && !active {
			return service.Result{ExitCode: 3}
		}
		return service.Result{}
	})

	out, code := runRoot(t, "connect", "--status")
	if code != ExitGenericError || !strings.Contains(out, "not installed") || !strings.Contains(out, "Key:      saved, ak_paireds...789") {
		t.Fatalf("status before install: exit %d\n%s", code, out)
	}

	out, code = runRoot(t, "connect", "--install")
	if code != 0 || !strings.Contains(out, "installed and started (systemd:") || !strings.Contains(out, "starts again at boot") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", service.UnitName)
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{" connect --service\n", `Environment="CLAUDE_CONFIG_DIR=/home/x/.claude-b"`, `Environment="HOME=` + home + `"`} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(string(unit), "ARCHIVIST_TOKEN") || strings.Contains(string(unit), pairedKey) {
		t.Fatalf("unit carries a credential:\n%s", unit)
	}
	if strings.Contains(out, pairedKey) {
		t.Fatal("install output leaks the key")
	}

	active = true
	if err := os.WriteFile(service.LogPath(home), []byte("12:00:00.000 connect: capabilities reported\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = runRoot(t, "connect", "--status")
	if code != 0 || !strings.Contains(out, "Service:  running (systemd: "+unitPath+")") || !strings.Contains(out, "Last log: 12:00:00.000 connect: capabilities reported") {
		t.Fatalf("status: exit %d\n%s", code, out)
	}

	out, code = runRoot(t, "connect", "--uninstall")
	if code != 0 || !strings.Contains(out, "Background connect removed") {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(unitPath); err == nil {
		t.Fatal("unit kept")
	}
	out, code = runRoot(t, "connect", "--uninstall")
	if code != 0 || !strings.Contains(out, "not installed") {
		t.Fatalf("second uninstall: exit %d\n%s", code, out)
	}
	for _, c := range *calls {
		if strings.Contains(c, pairedKey) {
			t.Fatalf("a manager command carried the key: %s", c)
		}
	}
}

// --service logs to ~/.archivist/connect/connect.log (0600) and maps a
// terminal outcome (here: no saved key) to exit 0.
func TestConnectServiceModeLogsAndExitsZeroWhenTerminal(t *testing.T) {
	if !connect.Supported() {
		t.Skip("daemon unsupported")
	}
	home := sandbox(t)
	out, code := runRoot(t, "connect", "--service")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, terminal output %q", code, out)
	}
	b, err := os.ReadFile(service.LogPath(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"service start v0.2.26", "needs an ak_ API key", "service stopped (exit 4)"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("log lacks %q:\n%s", want, b)
		}
	}
	if info, _ := os.Stat(service.LogPath(home)); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v", info.Mode().Perm())
	}
}

// --service uses the saved credentials file only: an injected
// ARCHIVIST_TOKEN and a --token flag are both ignored (here no key is saved,
// so the daemon stops as if neither were set).
func TestConnectServiceModeIgnoresEnvAndFlagTokens(t *testing.T) {
	if !connect.Supported() {
		t.Skip("daemon unsupported")
	}
	home := sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "ak_injectedbymanager000000")
	out, code := runRoot(t, "--token", "ak_flagtoken000000000000", "connect", "--service")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, terminal output %q", code, out)
	}
	if v, ok := os.LookupEnv("ARCHIVIST_TOKEN"); ok && v != "" {
		t.Fatalf("ARCHIVIST_TOKEN still set in the service process: %q", v)
	}
	b, err := os.ReadFile(service.LogPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "needs an ak_ API key") || !strings.Contains(string(b), "service stopped (exit 4)") {
		t.Fatalf("the service resolved a token other than the saved file:\n%s", b)
	}
	if _, err := os.Stat(service.PIDPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("service.pid left after exit: %v", err)
	}
}

// Pairing again over a different saved key says the earlier key is still
// active, masked only.
func TestConnectPairWarnsAboutEarlierKey(t *testing.T) {
	home := sandbox(t)
	const earlier = "ak_earliersecretvalue987654"
	saveKey(t, home, earlier)
	var hits atomic.Int32
	pairServer(t, 200, `{"key":"`+pairedKey+`","key_id":"ak_kid","name":"archivist box","tier":"pro"}`, nil, &hits)
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "the key saved here before (ak_earlier...654) is still active. Revoke it in Mosaic") {
		t.Fatalf("no earlier key note:\n%s", out)
	}
	if strings.Contains(out, earlier) || strings.Contains(out, pairedKey) {
		t.Fatalf("output leaks a key:\n%s", out)
	}
	// Pairing with nothing saved before, or the same key again: no note.
	home = sandbox(t)
	pairServer(t, 200, `{"key":"`+pairedKey+`","key_id":"ak_kid","name":"archivist box","tier":"pro"}`, nil, &hits)
	if out, _ := runRoot(t, "connect", "--pair", "ABCDE-FGHJK"); strings.Contains(out, "still active") {
		t.Fatalf("note without an earlier key:\n%s", out)
	}
	saveKey(t, home, pairedKey)
	if out, _ := runRoot(t, "connect", "--pair", "ABCDE-FGHJK"); strings.Contains(out, "still active") {
		t.Fatalf("note for the same key:\n%s", out)
	}
}

// connectResult's default branch (an error that is no known terminal
// outcome) is unexpected, and the service then exits non-zero (restart).
func TestConnectResultPlainErrorIsUnexpected(t *testing.T) {
	var stderr bytes.Buffer
	log := connect.NewLogger(io.Discard)
	err := connectResult(&stderr, log, &connect.Daemon{}, errors.New("relay exploded"))
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || !exitErr.unexpected || exitErr.Code != ExitGenericError {
		t.Fatalf("got %#v", err)
	}
	if serviceExit(log, err) == nil {
		t.Fatal("an unexpected daemon error must exit non-zero so the manager restarts it")
	}
	for _, terminal := range []error{nil, connect.ErrSuperseded, &connect.FatalError{Message: "x", Auth: true}} {
		if r := connectResult(&stderr, log, &connect.Daemon{}, terminal); serviceExit(log, r) != nil {
			t.Errorf("terminal %v restarted", terminal)
		}
	}
}

func TestServiceExitMapping(t *testing.T) {
	log := connect.NewLogger(io.Discard)
	if serviceExit(log, nil) != nil {
		t.Fatal("clean stop must exit 0")
	}
	for _, code := range []int{ExitGenericError, ExitUsageError, ExitNotFound, ExitAuthError} {
		if err := serviceExit(log, &ExitError{Code: code}); err != nil {
			t.Errorf("terminal exit %d restarted: %v", code, err)
		}
	}
	if err := serviceExit(log, &ExitError{Code: ExitGenericError, unexpected: true}); err == nil {
		t.Fatal("unexpected failure must exit non-zero (restart)")
	}
	if err := serviceExit(log, errors.New("boom")); err == nil {
		t.Fatal("plain error must exit non-zero")
	}
}

// --install --max-permission writes the ceiling into the service definition
// (connect --service --max-permission <mode>) and --status reports it; an
// install without it writes no flag and runs the default ceiling (Story
// 78.32). Linux systemd here; the launchd render is covered in
// internal/service.
func TestConnectInstallMaxPermissionLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd flow")
	}
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	unitPath := filepath.Join(home, ".config", "systemd", "user", service.UnitName)

	out, code := runRoot(t, "connect", "--install", "--max-permission", "full_auto")
	if code != 0 || !strings.Contains(out, "Permission ceiling: full_auto (Full auto).") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	unit, err := os.ReadFile(unitPath)
	if err != nil || !strings.Contains(string(unit), " connect --service --max-permission full_auto\n") {
		t.Fatalf("unit (%v):\n%s", err, unit)
	}
	out, code = runRoot(t, "connect", "--status")
	if code != 0 || !strings.Contains(out, "Ceiling:  full_auto (Full auto)\n") {
		t.Fatalf("status: exit %d\n%s", code, out)
	}

	// A reinstall without the flag (the install script on every update)
	// keeps the installed ceiling.
	out, code = runRoot(t, "connect", "--install")
	if code != 0 || !strings.Contains(out, "Permission ceiling: full_auto (Full auto), kept from the installed service") {
		t.Fatalf("reinstall: exit %d\n%s", code, out)
	}
	if unit, _ = os.ReadFile(unitPath); !strings.Contains(string(unit), " connect --service --max-permission full_auto\n") {
		t.Fatalf("unit:\n%s", unit)
	}
	// Removed, then installed without the flag: the default, no flag written.
	if _, code = runRoot(t, "connect", "--uninstall"); code != 0 {
		t.Fatal("uninstall")
	}
	out, code = runRoot(t, "connect", "--install")
	if code != 0 || !strings.Contains(out, "Permission ceiling: auto_edits (Auto edits), the default") {
		t.Fatalf("fresh install: exit %d\n%s", code, out)
	}
	unit, _ = os.ReadFile(unitPath)
	if !strings.Contains(string(unit), " connect --service\n") || strings.Contains(string(unit), "max-permission") {
		t.Fatalf("unit:\n%s", unit)
	}
	out, _ = runRoot(t, "connect", "--status")
	if !strings.Contains(out, "Ceiling:  auto_edits (Auto edits)\n") {
		t.Fatalf("status:\n%s", out)
	}

	// The equals form in a definition is reported and kept on reinstall.
	if err := os.WriteFile(unitPath, []byte(strings.Replace(string(unit), "connect --service", "connect --service --max-permission=read_only", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _ = runRoot(t, "connect", "--status"); !strings.Contains(out, "Ceiling:  read_only (Read only)\n") {
		t.Fatalf("status equals form:\n%s", out)
	}
	out, code = runRoot(t, "connect", "--install")
	if code != 0 || !strings.Contains(out, "Permission ceiling: read_only (Read only), kept from the installed service") {
		t.Fatalf("reinstall equals form: exit %d\n%s", code, out)
	}
	if unit, _ = os.ReadFile(unitPath); !strings.Contains(string(unit), " connect --service --max-permission read_only\n") {
		t.Fatalf("unit:\n%s", unit)
	}

	// A hand edited definition with an unknown mode is reported, not trusted.
	if err := os.WriteFile(unitPath, []byte(strings.Replace(string(unit), "connect --service", "connect --service --max-permission root", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _ = runRoot(t, "connect", "--status"); !strings.Contains(out, "Ceiling:  unreadable") {
		t.Fatalf("status:\n%s", out)
	}
}

// --service accepts --max-permission (what the installed service runs).
func TestConnectServiceAcceptsMaxPermission(t *testing.T) {
	if !connect.Supported() {
		t.Skip("daemon unsupported")
	}
	home := sandbox(t)
	out, code := runRoot(t, "connect", "--service", "--max-permission", "read_only")
	if code != 0 || out != "" {
		t.Fatalf("exit %d, terminal output %q", code, out)
	}
	b, _ := os.ReadFile(service.LogPath(home))
	if !strings.Contains(string(b), "permission ceiling read_only\n") || !strings.Contains(string(b), "service stopped (exit 4)") {
		t.Fatalf("log:\n%s", b)
	}
	// Without the flag the service logs the default ceiling.
	if _, code := runRoot(t, "connect", "--service"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if b, _ = os.ReadFile(service.LogPath(home)); !strings.Contains(string(b), "permission ceiling auto_edits\n") {
		t.Fatalf("log:\n%s", b)
	}
}

// An installed service whose --max-permission this version does not know
// (a downgrade, or a hand edit) logs why and exits 0: systemd
// Restart=on-failure and launchd KeepAlive SuccessfulExit false then leave it
// stopped instead of restarting it every 10 s. Nothing else runs.
func TestConnectServiceBadMaxPermissionStopsCleanly(t *testing.T) {
	if !connect.Supported() {
		t.Skip("daemon unsupported")
	}
	home := sandbox(t)
	for _, args := range [][]string{
		{"connect", "--service", "--max-permission", "root"},
		{"connect", "--service", "--max-permission=yolo"},
	} {
		_ = os.Remove(service.LogPath(home))
		out, code := runRoot(t, args...)
		if code != 0 || out != "" {
			t.Fatalf("%v: exit %d, terminal output %q", args, code, out)
		}
		b, _ := os.ReadFile(service.LogPath(home))
		log := string(b)
		for _, want := range []string{"service start v0.2.26", "is not a mode; use one of", "run 'archivist connect --install'", "service stopped (exit 2)"} {
			if !strings.Contains(log, want) {
				t.Errorf("%v: log lacks %q:\n%s", args, want, log)
			}
		}
		// Refused before the ceiling, the credential check and the PID file.
		for _, never := range []string{"permission ceiling", "needs an ak_ API key"} {
			if strings.Contains(log, never) {
				t.Errorf("%v: log has %q:\n%s", args, never, log)
			}
		}
		if _, err := os.Stat(service.PIDPath(home)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%v: PID file written: %v", args, err)
		}
	}
	// The plain daemon still refuses a bad ceiling with exit 2.
	if out, code := runRoot(t, "connect", "--max-permission", "root"); code != ExitUsageError || !strings.Contains(out, "is not a mode") {
		t.Fatalf("plain daemon: exit %d\n%s", code, out)
	}
}

func TestInstalledCeiling(t *testing.T) {
	for _, c := range []struct {
		args      []string
		want      connect.Mode
		found, ok bool
	}{
		{nil, connect.DefaultMaxMode, false, true},
		{[]string{"--max-permission", "ask"}, connect.ModeAsk, true, true},
		{[]string{"--max-permission=full_auto"}, connect.ModeFullAuto, true, true},
		{[]string{"--max-permission", "root"}, "", true, false},
		{[]string{"--max-permission=root"}, "", true, false},
		{[]string{"--max-permission"}, connect.DefaultMaxMode, false, true},
	} {
		got, found, ok := installedCeiling(c.args)
		if found != c.found || ok != c.ok || (ok && got != c.want) {
			t.Errorf("%q: %s %v %v, want %s %v %v", c.args, got, found, ok, c.want, c.found, c.ok)
		}
	}
}

// Story 78.33: --install --web-search is refused with a readable message
// before anything is written or any service manager runs.
func TestConnectInstallRefusesWebSearch(t *testing.T) {
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	calls := stubRunner(t, func(string) service.Result { return service.Result{} })
	out, code := runRoot(t, "connect", "--install", "--web-search")
	if code != ExitUsageError || !strings.Contains(out, "not yet supported for background connect") ||
		!strings.Contains(out, "archivist connect --web-search") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if len(*calls) != 0 {
		t.Fatalf("service manager ran %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd", "user", service.UnitName)); err == nil {
		t.Fatal("a unit was written")
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents")); err == nil {
		t.Fatal("a launch agent was written")
	}
}
