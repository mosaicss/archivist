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

// unusableBusErrors are the D-Bus library's errors (plain strings in godbus
// v5.2.4: auth.go, conn.go, transport_unix.go, transport_tcp.go) for a
// session bus this process cannot use at all: it refused our
// authentication, or its address names no usable transport.
var unusableBusErrors = []string{
	"dbus: authentication failed",
	"dbus: authentication protocol error",
	"dbus: invalid bus address",
	"dbus: invalid address",
	"dbus: unsupported address",
	"dbus: invalid tcp family",
	"dbus: couldn't determine address of session bus",
}

// noSecretService reports whether err says there is no Secret Service to
// use: the session bus cannot be reached at all (a stale
// DBUS_SESSION_BUS_ADDRESS: the dial fails), cannot be used (authentication
// refused, an address with no usable transport), or has no Secret Service
// on it. A reachable bus whose Secret Service is locked is not one of them.
func noSecretService(err error) bool {
	if err == nil {
		return false
	}
	var op *net.OpError
	if (errors.As(err, &op) && op.Op == "dial") || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	msg := err.Error()
	for _, s := range unusableBusErrors {
		if strings.Contains(msg, s) {
			return true
		}
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
	return strings.Contains(msg, "was not provided by any .service files")
}
