//go:build !windows

package connect

import (
	"fmt"
	"os"
	"syscall"
)

// cwdOwnerProblem is why a session folder is not this user's private
// directory (owner uid and mode 0700 exactly), "" when it is.
func cwdOwnerProblem(st os.FileInfo) string {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return "has no owner information"
	}
	if int(sys.Uid) != os.Getuid() {
		return fmt.Sprintf("is owned by uid %d, not %d", sys.Uid, os.Getuid())
	}
	if perm := st.Mode().Perm(); perm != 0o700 || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return fmt.Sprintf("has mode %v, not 0700", st.Mode())
	}
	return ""
}
