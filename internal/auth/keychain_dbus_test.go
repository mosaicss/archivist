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
	} {
		if !noKeychain(err) {
			t.Errorf("%v: want no keychain", err)
		}
	}
	for _, err := range []error{
		errors.New("no answer within 5s"),
		dbus.Error{Name: "org.freedesktop.DBus.Error.NoReply"},
		dbus.Error{Name: "org.freedesktop.Secret.Error.IsLocked"},
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
