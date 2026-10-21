//go:build !(linux || freebsd || openbsd || netbsd || dragonfly)

package auth

// noSecretService is Linux and BSD only (the Secret Service over D-Bus).
func noSecretService(error) bool { return false }
