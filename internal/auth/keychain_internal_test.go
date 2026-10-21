package auth

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// TestKeychainCallsAreTimeBounded: a keychain that never answers (a locked
// Secret Service waiting on its unlock prompt) costs at most the timeout,
// and keeps the one call slot until it really ends: later calls wait for it
// instead of stacking more calls (and unlock prompts).
func TestKeychainCallsAreTimeBounded(t *testing.T) {
	block := make(chan struct{})
	var calls atomic.Int32
	start := time.Now()
	_, err := bounded(50*time.Millisecond, func() (string, error) {
		calls.Add(1)
		<-block
		return "late", nil
	})
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("bounded call took %s", elapsed)
	}
	// The abandoned call still runs: a retry times out without a new call.
	_, err = bounded(50*time.Millisecond, func() (string, error) {
		calls.Add(1)
		return "stacked", nil
	})
	if err == nil || !strings.Contains(err.Error(), "still waiting") {
		t.Fatalf("retry while a call is in flight: got %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("keychain calls made: %d, want 1", n)
	}
	// Once it ends, calls run again.
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for {
		v, err := bounded(50*time.Millisecond, func() (string, error) { return "now", nil })
		if err == nil {
			if v != "now" {
				t.Fatalf("got %q", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never freed: %v", err)
		}
	}
	if _, err := bounded(time.Second, func() (int, error) { return 0, errors.New("refused") }); err == nil || err.Error() != "refused" {
		t.Fatalf("error passes through: got %v", err)
	}
}

// TestKeychainWritesAllowAnUnlockPrompt: saves wait 30 s (a person answers
// the unlock prompt), reads and deletes 5 s.
func TestKeychainWritesAllowAnUnlockPrompt(t *testing.T) {
	if keychainWriteTimeout != 30*time.Second || keychainTimeout != 5*time.Second {
		t.Fatalf("timeouts: write %s, read %s", keychainWriteTimeout, keychainTimeout)
	}
}

// TestKeychainAvailableNeedsARunningSessionBus: on Linux the keychain is
// used only when the D-Bus library finds a running session bus the way
// godbus does (DBUS_SESSION_BUS_ADDRESS, /run/user/<uid>/bus, then
// /run/user/<uid>/dbus-session); a custom XDG_RUNTIME_DIR with a bus socket
// is not one of them (godbus would autolaunch).
func TestKeychainAvailableNeedsARunningSessionBus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the session bus rule applies to Linux and the BSDs")
	}
	runDir := t.TempDir()
	prev := sessionBusRuntimeDir
	sessionBusRuntimeDir = func() string { return runDir }
	t.Cleanup(func() { sessionBusRuntimeDir = prev })
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	if keychainAvailable() {
		t.Fatal("no bus address and no bus in the runtime dir: want unavailable")
	}
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	xl, err := net.Listen("unix", filepath.Join(xdg, "bus"))
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	defer func() { _ = xl.Close() }()
	if keychainAvailable() {
		t.Fatal("a bus socket under a custom XDG_RUNTIME_DIR: godbus does not look there, want unavailable")
	}
	session := filepath.Join(runDir, "dbus-session")
	for content, want := range map[string]bool{
		"DBUS_SESSION_BUS_ADDRESS=unix:abstract=/tmp/dbus-x\n": true,
		"DBUS_SESSION_BUS_ADDRESS=autolaunch:\n":               false,
		"DBUS_SESSION_BUS_ADDRESS=\n":                          false,
		"unix:path=/tmp/bus\n":                                 false,
	} {
		if err := os.WriteFile(session, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := keychainAvailable(); got != want {
			t.Errorf("dbus-session %q: got %v, want %v", content, got, want)
		}
	}
	if err := os.Remove(session); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(runDir, "bus"))
	if err != nil {
		t.Skipf("unix socket: %v", err)
	}
	defer func() { _ = l.Close() }()
	if !keychainAvailable() {
		t.Fatal("a bus socket in /run/user/<uid>: want available")
	}
	for addr, want := range map[string]bool{
		"unix:path=/run/user/1000/bus": true,
		"autolaunch:":                  false,
		"disabled:":                    false,
	} {
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", addr)
		if got := keychainAvailable(); got != want {
			t.Errorf("DBUS_SESSION_BUS_ADDRESS=%q: got %v, want %v", addr, got, want)
		}
	}
}

// TestTestBinariesStartWithoutAKeychain: the seam every other package's
// tests rely on (Windows test runners have a real Credential Manager).
func TestTestBinariesStartWithoutAKeychain(t *testing.T) {
	if keychainUsable() {
		t.Fatal("a test binary must start with no usable keychain")
	}
	if _, err := keychainGet(); !errors.Is(err, errNoKeychain) {
		t.Fatalf("keychainGet: got %v, want errNoKeychain", err)
	}
	if err := keychainSet("ak_x"); !errors.Is(err, errNoKeychain) {
		t.Fatalf("keychainSet: got %v, want errNoKeychain", err)
	}
}

// TestATimedOutSaveKeepsTheMarker: a save that does not answer in time
// (an unlock prompt answered late) may still create the item; the marker,
// written first, stays, so a later move or delete reaches that item. A save
// that fails outright removes the marker it wrote.
func TestATimedOutSaveKeepsTheMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	MockKeychainForTest(t, false)
	release := make(chan struct{})
	done := make(chan struct{})
	prevSet, prevTimeout := keyringSet, keychainWriteTimeout
	keyringSet = func(service, user, secret string) error {
		defer close(done)
		<-release
		return keyring.Set(service, user, secret)
	}
	keychainWriteTimeout = 50 * time.Millisecond
	t.Cleanup(func() { keyringSet, keychainWriteTimeout = prevSet, prevTimeout })

	loc, err := SaveToken("ak_latesave1234567")
	if err != nil || loc.Keychain {
		t.Fatalf("SaveToken: got (%+v, %v), want the file", loc, err)
	}
	if !keychainMarked() {
		t.Fatal("a timed out save removed the marker")
	}
	// The late answer creates the item.
	close(release)
	<-done
	account, err := keychainAccount()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get(keychainService, account); err != nil {
		t.Fatalf("late item: %v", err)
	}
	// The next connect start (the file holds the key) removes it.
	if moved, err := MoveKeychainKeyToFile(); moved || err != nil {
		t.Fatalf("move: got (%v, %v)", moved, err)
	}
	if _, err := keyring.Get(keychainService, account); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("the late item stayed: %v", err)
	}
	if keychainMarked() {
		t.Fatal("the marker stayed after the item went")
	}

	// A save refused outright: the marker it wrote goes.
	if err := os.Remove(filepath.Join(home, ".archivist", "credentials")); err != nil {
		t.Fatal(err)
	}
	keyringSet = func(string, string, string) error { return errors.New("refused") }
	if loc, err := SaveToken("ak_refusedsave1234"); err != nil || loc.Keychain {
		t.Fatalf("SaveToken: got (%+v, %v), want the file", loc, err)
	}
	if keychainMarked() {
		t.Fatal("a refused save left its marker")
	}
}
