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

// markKeychain writes the keychain marker (~/.archivist/keychain), as a
// keychain save does: with a refusing keychain it stands for a saved item
// that cannot be read now.
func markKeychain(t *testing.T, home string) {
	t.Helper()
	path := filepath.Join(home, ".archivist", "keychain")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("marker\n"), 0o600); err != nil {
		t.Fatal(err)
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

// Connect keeps its key in the credentials file: with a usable keychain,
// pairing writes the 0600 file and no keychain item, and an earlier key held
// in the keychain is revoked and its item deleted; uninstall revokes and
// removes the file.
func TestConnectPairSavesToTheFileNotTheKeychain(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	auth.MockKeychainForTest(t, false)
	if loc, err := auth.SaveToken(earlierKey); err != nil || !loc.Keychain {
		t.Fatalf("earlier key: %+v %v", loc, err)
	}
	useBrowser(t, &fakeBrowser{available: true})
	fake := browserMosaic(t, 300, confirmedPoll())
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 || strings.Contains(out, "in the keychain") || !strings.Contains(out, "File:    "+filepath.Join(home, ".archivist", "credentials")) {
		t.Fatalf("pair: exit %d\n%s", code, out)
	}
	if saved, ok := credentials(t, home); !ok || saved != pairedKey {
		t.Fatalf("file %q %v", saved, ok)
	}
	if token, source, err := auth.Resolve(""); err != nil || token != pairedKey || source != auth.SourceFile {
		t.Fatalf("Resolve: %q %v %v", token, source, err)
	}
	if _, revoked, _ := fake.state(); len(revoked) != 1 || revoked[0] != earlierKey {
		t.Fatalf("revoked %v", revoked)
	}
	// The keychain item is gone: removing the saved key finds only the file.
	if _, where, err := auth.DeleteCredentials(); err != nil || where != filepath.Join(home, ".archivist", "credentials") {
		t.Fatalf("delete: %q %v", where, err)
	}
	saveKey(t, home, pairedKey)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	out, code = runRoot(t, "connect", "--uninstall")
	if code != 0 || !strings.Contains(out, "Removed the saved key from "+filepath.Join(home, ".archivist", "credentials")) {
		t.Fatalf("uninstall: exit %d\n%s", code, out)
	}
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("key kept: %v", err)
	}
}

// --install moves a key held in the keychain into the credentials file.
func TestConnectInstallMovesAKeychainKeyToTheFile(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	auth.MockKeychainForTest(t, false)
	if loc, err := auth.SaveToken(pairedKey); err != nil || !loc.Keychain {
		t.Fatalf("SaveToken: %+v %v", loc, err)
	}
	stubRunner(t, func(string) service.Result { return service.Result{} })
	path := filepath.Join(home, ".archivist", "credentials")
	out, code := runRoot(t, "connect", "--install")
	if code != 0 || !strings.Contains(out, "The saved key moved to "+path+", where connect keeps it.") {
		t.Fatalf("install: exit %d\n%s", code, out)
	}
	if saved, ok := credentials(t, home); !ok || saved != pairedKey {
		t.Fatalf("file %q %v", saved, ok)
	}
	if _, where, err := auth.DeleteCredentials(); err != nil || where != path {
		t.Fatalf("keychain item kept: %q %v", where, err)
	}
}

// Before the daemon starts, a saved key held in the keychain moves to the
// file, whatever key connect itself runs with; an item left beside the file
// is deleted; a failure warns once and is retried at the next start. With
// nothing recorded in the keychain, nothing happens.
func TestKeepConnectKeyInFile(t *testing.T) {
	home := sandbox(t)
	path := filepath.Join(home, ".archivist", "credentials")
	auth.MockKeychainForTest(t, true)
	logs := &syncBuffer{}
	keepConnectKeyInFile(connect.NewLogger(logs))
	if _, ok := credentials(t, home); ok || logs.String() != "" {
		t.Fatalf("no marker, locked keychain: the keychain was asked:\n%s", logs.String())
	}
	auth.MockKeychainForTest(t, false)
	if _, err := auth.SaveToken(pairedKey); err != nil {
		t.Fatal(err)
	}
	keepConnectKeyInFile(connect.NewLogger(logs))
	if saved, ok := credentials(t, home); !ok || saved != pairedKey || !strings.Contains(logs.String(), "the saved key moved to "+path) {
		t.Fatalf("file %q %v\n%s", saved, ok, logs.String())
	}
	if _, where, _ := auth.DeleteCredentials(); where != path {
		t.Fatalf("keychain item kept: %q", where)
	}
	// An item left in the keychain beside the file: a locked keychain warns
	// once and keeps it; the next start removes it.
	if _, err := auth.SaveToken(earlierKey); err != nil {
		t.Fatal(err)
	}
	saveKey(t, home, pairedKey)
	auth.MockKeychainForTest(t, true)
	logs = &syncBuffer{}
	keepConnectKeyInFile(connect.NewLogger(logs))
	if n := strings.Count(logs.String(), "warning: the key saved in the keychain could not be moved or removed"); n != 1 || !strings.Contains(logs.String(), "tries again at its next start") {
		t.Fatalf("warnings %d:\n%s", n, logs.String())
	}
	if !fileExists(filepath.Join(home, ".archivist", "keychain")) {
		t.Fatal("the marker went while the item remains")
	}
	// It answers again (a fresh in memory keychain still holding the item).
	auth.MockKeychainForTest(t, false)
	if err := keyring.Set("archivist", path, earlierKey); err != nil {
		t.Fatal(err)
	}
	logs = &syncBuffer{}
	keepConnectKeyInFile(connect.NewLogger(logs))
	if logs.String() != "" || fileExists(filepath.Join(home, ".archivist", "keychain")) {
		t.Fatalf("retry: marker kept or logged:\n%s", logs.String())
	}
	if _, err := keyring.Get("archivist", path); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("the leftover item stayed: %v", err)
	}
	if saved, ok := credentials(t, home); !ok || saved != pairedKey {
		t.Fatalf("file %q %v", saved, ok)
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
	// Nothing ever saved in the keychain (no marker): no wait at all.
	if err := waitForKeyring(context.Background(), connect.NewLogger(logs)); err != nil || logs.String() != "" {
		t.Fatalf("no marker: got %v\n%s", err, logs.String())
	}
	markKeychain(t, home)
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
	home := sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, true)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	// Nothing ever saved in the keychain: a locked one is not mentioned.
	out, _ := runRoot(t, "connect", "--status")
	if !strings.Contains(out, "Key:      none") || strings.Contains(out, "keychain") {
		t.Fatalf("no marker, status:\n%s", out)
	}
	markKeychain(t, home)
	out, _ = runRoot(t, "connect", "--status")
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
	// With the marker, so the keychain is really asked.
	markKeychain(t, home)
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
	// Nothing ever saved in the keychain: only the file, exit 0.
	if out, code := runRoot(t, "auth", "logout"); code != 0 || !strings.Contains(out, "Logged out: deleted "+filepath.Join(home, ".archivist", "credentials")) {
		t.Fatalf("no marker: exit %d\n%s", code, out)
	}
	saveKey(t, home, pairedKey)
	markKeychain(t, home)
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
	home := sandbox(t)
	auth.MockKeychainForTest(t, true)
	useBrowser(t, &fakeBrowser{available: true})
	fake := browserMosaic(t, 300, confirmedPoll())
	// A first pairing on a computer that never saved a key in the
	// keychain: a locked one is not asked and no note is printed.
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 || strings.Contains(out, "keychain") {
		t.Fatalf("no marker: exit %d\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(home, ".archivist", "credentials")); err != nil {
		t.Fatal(err)
	}
	markKeychain(t, home)
	out, code = runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 || !strings.Contains(out, "the key this computer used before could not be read from the keychain, so it may still be active. Remove it in Mosaic settings") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, revoked, _ := fake.state(); len(revoked) != 0 {
		t.Fatalf("revoked %v", revoked)
	}
}

// Uninstall of a computer that never saved a key in the keychain, with a
// locked keyring and nothing saved: the keychain is not asked and the exit
// is 0.
func TestConnectUninstallNothingSavedLockedKeyringExitsZero(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, true)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	out, code := runRoot(t, "connect", "--uninstall")
	if code != 0 || strings.Contains(out, "keychain") || strings.Contains(out, "could not") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if fileExists(filepath.Join(home, ".archivist", "keychain")) {
		t.Fatal("a marker was written")
	}
}

// A key in the file and an item in a keychain that cannot be reached now:
// uninstall says the file was removed and, separately, that the keychain
// copy could not be removed; the exit is unchanged (the revoke worked).
func TestConnectUninstallReportsEachStore(t *testing.T) {
	linuxOnly(t)
	home := sandbox(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	saveKey(t, home, pairedKey)
	markKeychain(t, home)
	auth.MockKeychainForTest(t, true)
	stubRunner(t, func(string) service.Result { return service.Result{} })
	revoked := revokeServer(t)
	out, code := runRoot(t, "connect", "--uninstall")
	path := filepath.Join(home, ".archivist", "credentials")
	if code != 0 || len(revoked()) != 1 {
		t.Fatalf("exit %d revoked %v\n%s", code, revoked(), out)
	}
	for _, want := range []string{"Removed the saved key from " + path + ".", "Note: the copy of the key in the keychain could not be removed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials file kept")
	}
	noKeyLeaks(t, out, pairedKey)
}
