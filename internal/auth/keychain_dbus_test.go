//go:build linux || freebsd || openbsd || netbsd || dragonfly

package auth

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	dbus "github.com/godbus/dbus/v5"
	"github.com/zalando/go-keyring"
)

// TestSessionBusWithoutSecretServiceIsNoKeychain: a session bus with no
// Secret Service on it means the file is the store, never "locked".
func TestSessionBusWithoutSecretServiceIsNoKeychain(t *testing.T) {
	for _, err := range []error{
		dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown",
			Body: []any{"The name org.freedesktop.secrets was not provided by any .service files"}},
		dbus.NewError("org.freedesktop.DBus.Error.NameHasNoOwner", []any{"Could not get owner of name 'org.freedesktop.secrets': no such name"}),
		fmt.Errorf("open session: %w", dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}),
		errors.New("The name org.freedesktop.secrets was not provided by any .service files"),
		// A session bus that cannot be reached (stale address).
		&net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ENOENT)},
		&net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
		// A session bus this process cannot use (godbus v5.2.4 wording).
		errors.New("dbus: authentication failed"),
		fmt.Errorf("connect: %w", errors.New("dbus: authentication protocol error")),
		errors.New("dbus: invalid bus address (invalid or unsupported transport)"),
		errors.New("dbus: invalid bus address (no transport)"),
		errors.New("dbus: invalid address (neither path nor abstract set)"),
		errors.New("dbus: invalid address (both path and abstract set)"),
		errors.New("dbus: unsupported address (must set host and port)"),
		errors.New("dbus: couldn't determine address of session bus"),
	} {
		if !noKeychain(err) {
			t.Errorf("%v: want no keychain", err)
		}
	}
	for _, err := range []error{
		errors.New("no answer within 5s"),
		dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"},
		dbus.Error{Name: "org.freedesktop.Secret.Error.IsLocked"},
		dbus.Error{Name: "org.freedesktop.DBus.Error.AccessDenied"},
		errors.New("dbus: connection closed by user"),
		keyring.ErrNotFound,
	} {
		if noKeychain(err) {
			t.Errorf("%v: want a keychain (unreadable or empty), not no keychain", err)
		}
	}
}

// TestSavedTokenWithoutSecretServiceIsNoToken: with that answer, nothing
// saved reads as ErrNoToken (auth status exit 4 names no keychain error).
func TestSavedTokenWithoutSecretServiceIsNoToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", "")
	MockKeychainForTest(t, false)
	keyring.MockInitWithError(dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"})
	// With the marker, so the keychain is really asked.
	if err := writeKeychainMarker(); err != nil {
		t.Fatal(err)
	}
	if _, err := SavedToken(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("SavedToken: got %v, want ErrNoToken", err)
	}
	loc, err := SaveToken("ak_" + "0123456789abcdef0123456789abcdef")
	if err != nil || loc.Keychain {
		t.Fatalf("SaveToken: got (%+v, %v), want the file", loc, err)
	}
	existed, _, err := DeleteCredentials()
	if err != nil || !existed {
		t.Fatalf("DeleteCredentials: got (%v, %v)", existed, err)
	}
}
