//go:build !windows

package connect

// envKey is the form environment keys are compared in: exact on Unix.
func envKey(k string) string { return k }

// platformAllowKeys adds nothing outside Windows.
var platformAllowKeys []string
