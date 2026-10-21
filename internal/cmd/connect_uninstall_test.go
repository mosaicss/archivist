package cmd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/service"
	"github.com/zalando/go-keyring"
)

// Story 94.6: the start question, the keychain and a clean uninstall.

func linuxOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("systemd flow")
	}
}

func unitPathOf(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", service.UnitName)
}

func TestConnectInstallAskWithoutTerminalSaysTheDefault(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	out, code := runRoot(t, "connect", "--install", "--ask")
	if code != 0 || !strings.Contains(out, "Starting Archivist when your computer starts (the default).\n") || !fileExists(unitPathOf(home)) {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestConnectInstallAskYes(t *testing.T) {
	linuxOnly(t)
	for _, answer := range []string{"\n", "y\n", "YES\n", "maybe\n\n", ""} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			home := sandbox(t)
			saveKey(t, home, pairedKey)
			stubRunner(t, func(string) service.Result { return service.Result{} })
			tty := useTerminal(t, answer)
			out, code := runRoot(t, "connect", "--install", "--ask")
			if code != 0 || !fileExists(unitPathOf(home)) || !strings.Contains(out, "Background connect ") {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if !strings.HasPrefix(tty.written(), startQuestion) {
				t.Fatalf("terminal %q", tty.written())
			}
		})
	}
}

func TestConnectInstallAskNoInstallsNothing(t *testing.T) {
	linuxOnly(t)
	for _, answer := range []string{"n\n", "No\n", "what\nn\n"} {
		t.Run(strings.TrimSpace(answer), func(t *testing.T) {
			home := sandbox(t)
			saveKey(t, home, pairedKey)
			calls := stubRunner(t, func(string) service.Result { return service.Result{} })
			tty := useTerminal(t, answer)
			var flushed int
			prev := flushConfirmInput
			flushConfirmInput = func(io.ReadWriter) { flushed++ }
			t.Cleanup(func() { flushConfirmInput = prev })
			out, code := runRoot(t, "connect", "--install", "--ask")
			if code != 0 || !strings.Contains(out, startDeclined) || fileExists(unitPathOf(home)) || len(*calls) != 0 {
				t.Fatalf("exit %d calls %v\n%s", code, *calls, out)
			}
			if flushed != 1 || !strings.Contains(tty.written(), "(recommended) [Y/n] ") {
				t.Fatalf("flushed %d, terminal %q", flushed, tty.written())
			}
			// The key stays where it was: nothing else changed.
			if saved, ok := credentials(t, home); !ok || saved != pairedKey {
				t.Fatalf("key %q %v", saved, ok)
			}
		})
	}
}

func TestConnectInstallAskInterruptAborts(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	calls := stubRunner(t, func(string) service.Result { return service.Result{} })
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	tty := useTerminalReader(t, r)
	prev := pairInterrupt
	pairInterrupt = func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		go func() { <-tty.reading; cancel() }()
		return ctx, cancel
	}
	t.Cleanup(func() { pairInterrupt = prev })
	out, code := runRoot(t, "connect", "--install", "--ask")
	if code != ExitGenericError || fileExists(unitPathOf(home)) || len(*calls) != 0 {
		t.Fatalf("exit %d calls %v\n%s", code, *calls, out)
	}
}

func TestConnectInstallAskSkippedWhenInstalled(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	pairServiceInstalled = func() bool { return true }
	tty := useTerminal(t, "n\n")
	out, code := runRoot(t, "connect", "--install", "--ask")
	if code != 0 || tty.written() != "" || !fileExists(unitPathOf(home)) {
		t.Fatalf("exit %d terminal %q\n%s", code, tty.written(), out)
	}
}

// Acceptance: a saved ak_ key and an installed service; uninstall revokes
// the key with itself, removes the unit, the credentials, ~/.archivist/connect
// and this user's work folders, and turns off the lingering Archivist
// turned on.
func TestConnectUninstallCleansUp(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	saveKey(t, home, pairedKey)
	calls := stubRunner(t, func(line string) service.Result {
		if strings.Contains(line, "is-active") {
			return service.Result{ExitCode: 3}
		}
		return service.Result{}
	})
	if out, code := runRoot(t, "connect", "--install"); code != 0 {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	if b, err := os.ReadFile(service.LingerMarkerPath(home)); err != nil || strings.TrimSpace(string(b)) != "archivist" {
		t.Fatalf("linger marker %q %v", b, err)
	}
	work := filepath.Join(tmp, "archivist-connect-123")
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(tmp, "unrelated")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "archivist-connect-link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := writePauseMarker(filepath.Join(home, ".archivist", "connect")); err != nil {
		t.Fatal(err)
	}
	revoked := revokeServer(t)
	t.Setenv("ARCHIVIST_RELAY_URL", newFakePauseRelay(t).wsURL())
	out, code := runRoot(t, "connect", "--uninstall")
	if code != 0 {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if got := revoked(); len(got) != 1 || got[0] != pairedKey {
		t.Fatalf("revoked %v", got)
	}
	for _, p := range []string{unitPathOf(home), filepath.Join(home, ".archivist", "credentials"), filepath.Join(home, ".archivist", "connect"), work} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s kept (%v)", p, err)
		}
	}
	for _, p := range []string{other, link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", p, err)
		}
	}
	if !slicesContain(*calls, "loginctl --no-ask-password disable-linger") {
		t.Fatalf("calls %v", *calls)
	}
	for _, want := range []string{"Background connect removed", "Lingering turned off", "no longer works", "Removed 1 session work folder(s)."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	noKeyLeaks(t, out, pairedKey)
}

// The revoke fails: the files go anyway, and uninstall exits 1 saying to
// remove the key in Mosaic.
func TestConnectUninstallRevokeFailureStillRemovesFiles(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	if _, code := runRoot(t, "connect", "--install"); code != 0 {
		t.Fatal("install")
	}
	fake := newFakeMosaic(t, redeemBody(accountEmail))
	fake.revokeAs[pairedKey] = 502
	out, code := runRoot(t, "connect", "--uninstall")
	if code != ExitGenericError || !strings.Contains(out, "Remove this computer's key in Mosaic settings.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials kept")
	}
	if _, err := os.Stat(filepath.Join(home, ".archivist", "connect")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("connect dir kept: %v", err)
	}
	// An already revoked key (401) counts as revoked.
	saveKey(t, home, pairedKey)
	fake.revokeAs[pairedKey] = 401
	if out, code = runRoot(t, "connect", "--uninstall"); code != 0 || !strings.Contains(out, "Background connect is not installed.") {
		t.Fatalf("second uninstall: exit %d\n%s", code, out)
	}
}

// Nothing installed: the same cleanup and revoke; lingering is untouched
// (no marker, no unit). An mc_pat_ login is neither revoked nor removed.
func TestConnectUninstallNothingInstalled(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	calls := stubRunner(t, func(string) service.Result { return service.Result{} })
	revoked := revokeServer(t)
	out, code := runRoot(t, "connect", "--uninstall")
	if code != 0 || !strings.Contains(out, "Background connect is not installed.") || len(revoked()) != 1 {
		t.Fatalf("exit %d revoked %v\n%s", code, revoked(), out)
	}
	if slicesContain(*calls, "loginctl") {
		t.Fatalf("lingering touched: %v", *calls)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials kept")
	}

	saveKey(t, home, "mc_pat_legacy_token_123")
	out, code = runRoot(t, "connect", "--uninstall")
	if code != 0 || len(revoked()) != 1 {
		t.Fatalf("mc_pat_: exit %d revoked %v\n%s", code, revoked(), out)
	}
	if saved, ok := credentials(t, home); !ok || saved != "mc_pat_legacy_token_123" {
		t.Fatalf("mc_pat_ login removed: %q %v", saved, ok)
	}
}

// Acceptance: with a usable keychain, pairing leaves no credentials file and
// every reader gets the key; uninstall revokes and removes it there.
func TestConnectPairSavesToTheKeychain(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	auth.MockKeychainForTest(t, false)
	fake := newFakeMosaic(t, redeemBody(accountEmail))
	useTerminal(t, "\n")
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 || !strings.Contains(out, "Saved:   in the keychain") {
		t.Fatalf("pair: exit %d\n%s", code, out)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("a credentials file was written")
	}
	if token, err := auth.SavedToken(); err != nil || token != pairedKey {
		t.Fatalf("SavedToken: %q %v", token, err)
	}
	if token, source, err := auth.Resolve(""); err != nil || token != pairedKey || source != auth.SourceKeychain {
		t.Fatalf("Resolve: %q %v %v", token, source, err)
	}
	stubRunner(t, func(string) service.Result { return service.Result{} })
	if out, _ := runRoot(t, "connect", "--status"); !strings.Contains(out, "Key:      saved, ak_paireds...789") {
		t.Fatalf("status:\n%s", out)
	}
	out, code = runRoot(t, "connect", "--uninstall")
	if code != 0 || !strings.Contains(out, "Removed the saved key from the keychain.") {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if _, revoked, _ := fake.state(); len(revoked) != 1 || revoked[0] != pairedKey {
		t.Fatalf("revoked %v", revoked)
	}
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("key kept: %v", err)
	}
}

// --install moves a key saved in the file into the keychain.
func TestConnectInstallMovesTheKeyIntoTheKeychain(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	auth.MockKeychainForTest(t, false)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	out, code := runRoot(t, "connect", "--install")
	if code != 0 || !strings.Contains(out, "The saved key moved to the keychain.") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials file kept")
	}
	if token, err := auth.SavedToken(); err != nil || token != pairedKey {
		t.Fatalf("SavedToken: %q %v", token, err)
	}
}

// A keychain that cannot be read yet (Linux before login) holds the service
// start: one log line, retries, no file, and a clean stop.
func TestWaitForKeyringWaitsWithoutWritingAFile(t *testing.T) {
	home := sandbox(t)
	auth.MockKeychainForTest(t, true)
	prev := keyringRetry
	keyringRetry = 5 * time.Millisecond
	t.Cleanup(func() { keyringRetry = prev })
	logs := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := waitForKeyring(ctx, connect.NewLogger(logs)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if n := strings.Count(logs.String(), "waiting for the keyring"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logs.String())
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("a credentials file was written")
	}
	// A readable key (here, the file) ends the wait at once.
	saveKey(t, home, pairedKey)
	if err := waitForKeyring(context.Background(), connect.NewLogger(logs)); err != nil {
		t.Fatalf("got %v", err)
	}
}

// A keychain that cannot be read is named, not reported as "no key":
// connect --status and doctor say to unlock it.
func TestLockedKeychainIsNamedInStatusAndDoctor(t *testing.T) {
	linuxOnly(t)
	sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, true)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	out, _ := runRoot(t, "connect", "--status")
	if !strings.Contains(out, "Key:      saved in the keychain, which could not be read") || strings.Contains(out, "Key:      none") {
		t.Fatalf("status:\n%s", out)
	}
	out, code := runRoot(t, "doctor", "--no-network", "--format", "text")
	if code == 0 || !strings.Contains(out, "the keychain could not be read") || strings.Contains(out, "no credential found") {
		t.Fatalf("doctor: exit %d\n%s", code, out)
	}
}

// Three answers that are neither yes nor no fall back to yes.
func TestConnectInstallAskThreeUnclearAnswersMeanYes(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	tty := useTerminal(t, "what\nhuh\nmaybe\nn\n")
	out, code := runRoot(t, "connect", "--install", "--ask")
	if code != 0 || !fileExists(unitPathOf(home)) || strings.Contains(out, startDeclined) {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if n := strings.Count(tty.written(), startQuestion); n != 3 {
		t.Fatalf("asked %d times: %q", n, tty.written())
	}
}

// A session bus that cannot be reached (a stale DBUS_SESSION_BUS_ADDRESS)
// is no keychain, never a locked one: logout, auth status and connect
// --status behave as with the file alone.
func TestUnreachableSessionBusIsNoKeychain(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	keyring.MockInitWithError(&net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ENOENT)})
	stubRunner(t, func(string) service.Result { return service.Result{ExitCode: 3} })
	out, code := runRoot(t, "auth", "status")
	if code != ExitAuthError || strings.Contains(out, "could not be read") {
		t.Fatalf("auth status: exit %d\n%s", code, out)
	}
	out, _ = runRoot(t, "connect", "--status")
	if !strings.Contains(out, "Key:      none") {
		t.Fatalf("connect --status:\n%s", out)
	}
	if out, code = runRoot(t, "auth", "logout"); code != 0 || !strings.Contains(out, "no saved key found") {
		t.Fatalf("logout, nothing saved: exit %d\n%s", code, out)
	}
	saveKey(t, home, pairedKey)
	if out, code = runRoot(t, "auth", "logout"); code != 0 || !strings.Contains(out, "Logged out: deleted "+filepath.Join(home, ".archivist", "credentials")) {
		t.Fatalf("logout, file saved: exit %d\n%s", code, out)
	}
}

// A keychain that answers with an error (locked): logout removes the file,
// says the keychain item could not be removed and exits 1.
func TestLogoutWithALockedKeychainExitsOne(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	saveKey(t, home, pairedKey)
	auth.MockKeychainForTest(t, true)
	out, code := runRoot(t, "auth", "logout")
	if code != ExitGenericError || !strings.Contains(out, "Could not delete credentials") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials file kept")
	}
}

// Re-pairing while the keychain cannot be read: the earlier key could not
// be revoked, and the output says it may still be active.
func TestConnectPairUnreadableKeychainNamesTheEarlierKey(t *testing.T) {
	sandbox(t)
	auth.MockKeychainForTest(t, true)
	fake := newFakeMosaic(t, redeemBody(accountEmail))
	useTerminal(t, "\n")
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 || !strings.Contains(out, "the key this computer used before could not be read from the keychain, so it may still be active. Remove it in Mosaic settings") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, revoked, _ := fake.state(); len(revoked) != 0 {
		t.Fatalf("revoked %v", revoked)
	}
}
