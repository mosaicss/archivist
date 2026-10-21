//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package cmd

import "golang.org/x/sys/unix"

// fread is FREAD from <sys/fcntl.h>: TIOCFLUSH with it discards unread
// input only (what tcflush(fd, TCIFLUSH) does on these systems).
const fread = 0x1

// flushInputFD discards the terminal's unread input: the TIOCFLUSH ioctl,
// whose argument points to FREAD.
func flushInputFD(fd int) error {
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fread)
}
