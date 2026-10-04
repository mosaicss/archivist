//go:build linux

package service

// newPlatform returns the systemd user unit manager.
func newPlatform(opts Options) (Manager, error) { return newSystemd(opts), nil }
