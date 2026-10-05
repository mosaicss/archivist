//go:build !windows

package connect

// HarnessLookPath is look itself outside Windows.
func HarnessLookPath(look LookPath) LookPath { return look }
