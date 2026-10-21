//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package cmd

// flushInputFD is not available here: typed ahead input stays queued.
func flushInputFD(int) error { return nil }
