//go:build linux || freebsd || openbsd || netbsd || dragonfly

package auth

import (
	"errors"
	"net"
	"strings"
	"syscall"

	dbus "github.com/godbus/dbus/v5"
)

// noSecretServiceNames are the D-Bus errors of a session bus that has no
// Secret Service (no keyring installed or running): "no keychain", never
// "locked".
var noSecretServiceNames = []string{
	"org.freedesktop.DBus.Error.ServiceUnknown",
	"org.freedesktop.DBus.Error.NameHasNoOwner",
}

// noSecretService reports whether err says there is no Secret Service to
// use: the session bus cannot be reached at all (a stale
// DBUS_SESSION_BUS_ADDRESS: the dial fails), or it has no Secret Service on
// it. A reachable bus whose Secret Service is locked is not one of them.
func noSecretService(err error) bool {
	if err == nil {
		return false
	}
	var op *net.OpError
	if (errors.As(err, &op) && op.Op == "dial") || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var name string
	var value dbus.Error
	var ptr *dbus.Error
	switch {
	case errors.As(err, &ptr) && ptr != nil:
		name = ptr.Name
	case errors.As(err, &value):
		name = value.Name
	}
	for _, n := range noSecretServiceNames {
		if name == n {
			return true
		}
	}
	return strings.Contains(err.Error(), "was not provided by any .service files")
}
