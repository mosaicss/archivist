package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// A key saved with auth login lives in the operating system's keychain where
// one is usable and no credentials file exists yet (Story 94.6): the macOS
// login keychain, the Secret Service (GNOME Keyring, KWallet) on Linux, or the
// Windows Credential Manager. The 0600 credentials file is the store where no
// keychain can be used (a headless Linux machine has no session bus), once it
// exists, and always on a computer that runs connect (SaveTokenToFile,
// MoveKeychainKeyToFile).
//
// The item is service "archivist", account the absolute credentials file path
// (one item per home directory). Every keychain call is time bounded, at most
// one call is in flight at a time, and on Linux and the BSDs a call is made
// only when a session bus already runs: the D-Bus library would otherwise
// start one. A session bus without a Secret Service is "no keychain".
//
// The keychain is read, moved or deleted only when the marker file
// ~/.archivist/keychain (no secret in it) says Archivist saved an item
// there: a computer that never saved a key in the keychain never asks it
// anything (no unlock prompt, no wait for a locked keyring). Saving an item
// writes the marker; removing the item (or finding it gone) removes it.

// keychainService is the keychain item's service name.
const keychainService = "archivist"

// keychainTimeout bounds every keychain read and delete (a locked Secret
// Service waits on an unlock prompt with no timeout of its own);
// keychainWriteTimeout bounds a save, long enough for a person to answer an
// unlock prompt while pairing, logging in or installing.
var (
	keychainTimeout      = 5 * time.Second
	keychainWriteTimeout = 30 * time.Second
)

// errNoKeychain means no keychain can be used here: the file is the store.
var errNoKeychain = errors.New("no keychain is available on this computer")

// ErrKeychainUnavailable means a keychain exists but could not be read in
// time or refused the read (on Linux, typically a keyring that stays locked
// until the user logs in). The background service waits for it.
var ErrKeychainUnavailable = errors.New("the keychain could not be read")

// keychainState is the provider seam: available reports whether a keychain
// may be called at all. Test binaries start with no usable keychain (see
// init), so file based tests keep working on machines that have one.
var keychainState = struct {
	sync.Mutex
	available func() bool
}{available: keychainAvailable}

func init() {
	if testing.Testing() {
		keyring.MockInitWithError(errNoKeychain)
		keychainState.available = func() bool { return false }
	}
}

// MockKeychainForTest gives the rest of one test an in memory keychain that
// works (or, with fail set, refuses every call with ErrKeychainUnavailable's
// cause), and restores the test binary's default (no keychain) afterwards.
// It panics outside a test binary.
func MockKeychainForTest(t interface{ Cleanup(func()) }, fail bool) {
	if !testing.Testing() {
		panic("auth.MockKeychainForTest called outside a test binary")
	}
	keychainState.Lock()
	keychainState.available = func() bool { return true }
	keychainState.Unlock()
	if fail {
		keyring.MockInitWithError(errors.New("keychain locked"))
	} else {
		keyring.MockInit()
	}
	t.Cleanup(func() {
		keychainState.Lock()
		keychainState.available = func() bool { return false }
		keychainState.Unlock()
		keyring.MockInitWithError(errNoKeychain)
	})
}

func keychainUsable() bool {
	keychainState.Lock()
	defer keychainState.Unlock()
	return keychainState.available()
}

// keychainAvailable reports whether this platform has a keychain this
// process can reach without starting anything: always on macOS and Windows;
// on Linux and the BSDs only when the D-Bus library will find a running
// session bus without autolaunching one. godbus v5.2.4 takes
// DBUS_SESSION_BUS_ADDRESS (anything but "autolaunch:"), then
// /run/user/<uid>/bus, then the address in /run/user/<uid>/dbus-session,
// and otherwise runs dbus-launch; $XDG_RUNTIME_DIR is never consulted.
func keychainAvailable() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		if addr := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); addr != "" {
			return addr != "autolaunch:" && addr != "disabled:"
		}
		dir := sessionBusRuntimeDir()
		if info, err := os.Stat(filepath.Join(dir, "bus")); err == nil {
			return info.Mode()&os.ModeSocket != 0
		}
		data, err := os.ReadFile(filepath.Join(dir, "dbus-session"))
		if err != nil {
			return false
		}
		addr, ok := strings.CutPrefix(string(data), "DBUS_SESSION_BUS_ADDRESS=")
		addr = strings.TrimRight(addr, "\n\r")
		return ok && addr != "" && addr != "autolaunch:" && addr != "disabled:"
	}
	return false
}

// sessionBusRuntimeDir is the directory godbus looks in for a session bus,
// /run/user/<uid> (a var so tests can replace it).
var sessionBusRuntimeDir = func() string { return fmt.Sprintf("/run/user/%d", os.Getuid()) }

// keychainMarkerContent is the marker file's text (it holds no secret).
const keychainMarkerContent = "The key saved with 'archivist auth login' is in this computer's keychain.\n"

// keychainMarkerPath is ~/.archivist/keychain, beside the credentials file.
func keychainMarkerPath() (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), "keychain"), nil
}

// keychainMarked reports whether the marker says an item was saved.
func keychainMarked() bool {
	path, err := keychainMarkerPath()
	if err != nil {
		return false
	}
	_, err = os.Lstat(path)
	return err == nil
}

// writeKeychainMarker records that an item was saved (0600, parent 0700).
func writeKeychainMarker() error {
	path, err := keychainMarkerPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(keychainMarkerContent), 0o600)
}

// removeKeychainMarker records that no item is saved any more.
func removeKeychainMarker() {
	if path, err := keychainMarkerPath(); err == nil {
		_ = os.Remove(path)
	}
}

// keychainAccount is the item's account: the absolute credentials path.
func keychainAccount() (string, error) {
	path, err := CredentialsPath()
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

// errKeychainTimeout is a keychain call that did not answer in time; it may
// still complete later (for example once an unlock prompt is answered).
var errKeychainTimeout = errors.New("no answer within")

// keyringSet is keyring.Set (a var so tests can make a save slow).
var keyringSet = keyring.Set

// keychainSlot holds the one keychain call allowed in flight. A call that
// outlives its deadline keeps the slot until it really ends (for example
// once the user answers an unlock prompt), so retries wait for it instead of
// stacking more calls and prompts.
var keychainSlot = make(chan struct{}, 1)

// bounded runs one keychain call within timeout, the wait for an earlier
// call still in flight included. A call still running at the deadline is
// abandoned (its result is dropped) and keeps the slot until it returns.
func bounded[T any](timeout time.Duration, call func() (T, error)) (T, error) {
	var zero T
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case keychainSlot <- struct{}{}:
	case <-timer.C:
		return zero, fmt.Errorf("%w %s (an earlier keychain request is still waiting)", errKeychainTimeout, timeout)
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-keychainSlot }()
		v, err := call()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-timer.C:
		return zero, fmt.Errorf("%w %s", errKeychainTimeout, timeout)
	}
}

// noKeychain reports an error that means "use the file": no keychain here,
// or (Linux and the BSDs) a session bus with no Secret Service on it.
func noKeychain(err error) bool {
	return errors.Is(err, errNoKeychain) || errors.Is(err, keyring.ErrUnsupportedPlatform) || noSecretService(err)
}

// keychainGet reads the saved key: ("", errNoKeychain) where none is usable
// or the marker says nothing was saved there, ("", keyring.ErrNotFound) when
// the item is gone (the marker goes too), and a wrapped
// ErrKeychainUnavailable when the keychain did not answer or refused.
func keychainGet() (string, error) {
	if !keychainMarked() || !keychainUsable() {
		return "", errNoKeychain
	}
	account, err := keychainAccount()
	if err != nil {
		return "", err
	}
	token, err := bounded(keychainTimeout, func() (string, error) { return keyring.Get(keychainService, account) })
	switch {
	case err == nil:
		return token, nil
	case errors.Is(err, keyring.ErrNotFound):
		removeKeychainMarker()
		return "", keyring.ErrNotFound
	case noKeychain(err):
		return "", errNoKeychain
	}
	return "", fmt.Errorf("%w: %v", ErrKeychainUnavailable, err)
}

// keychainSet writes the marker and then stores the key; any error means
// the file is used instead. The marker goes first so an item a timed out
// save still creates later is found (a marker without an item heals: a read
// or delete that finds nothing removes it). A save that failed outright
// removes a marker it wrote; one that timed out keeps it.
func keychainSet(token string) error {
	if !keychainUsable() {
		return errNoKeychain
	}
	account, err := keychainAccount()
	if err != nil {
		return err
	}
	had := keychainMarked()
	if err := writeKeychainMarker(); err != nil {
		return fmt.Errorf("record the keychain item: %w", err)
	}
	_, err = bounded(keychainWriteTimeout, func() (struct{}, error) { return struct{}{}, keyringSet(keychainService, account, token) })
	if err != nil && !had && !errors.Is(err, errKeychainTimeout) {
		removeKeychainMarker()
	}
	return err
}

// keychainDelete removes the saved key; existed is false when there was
// none (no marker, nothing found, or no keychain here). The marker goes
// once the item is known to be gone; it stays while the item may remain.
func keychainDelete() (bool, error) {
	if !keychainMarked() || !keychainUsable() {
		return false, nil
	}
	account, err := keychainAccount()
	if err != nil {
		return false, err
	}
	_, err = bounded(keychainTimeout, func() (struct{}, error) { return struct{}{}, keyring.Delete(keychainService, account) })
	switch {
	case err == nil:
		removeKeychainMarker()
		return true, nil
	case errors.Is(err, keyring.ErrNotFound):
		removeKeychainMarker()
		return false, nil
	case noKeychain(err):
		return false, nil
	}
	return false, fmt.Errorf("the copy of the key in the keychain could not be removed: %w", err)
}
