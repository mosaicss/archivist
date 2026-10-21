//go:build linux

package cmd

import "golang.org/x/sys/unix"

// flushInputFD discards the terminal's unread input: tcflush(fd, TCIFLUSH),
// the TCFLSH ioctl with TCIFLUSH as its value.
func flushInputFD(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
