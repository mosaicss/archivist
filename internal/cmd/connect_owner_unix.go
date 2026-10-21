//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// ownedByUser reports whether info belongs to this user (a shared /tmp
// holds other users' folders too).
func ownedByUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
