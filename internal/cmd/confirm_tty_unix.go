//go:build !windows

package cmd

import (
	"io"
	"os"
)

// openTerminal opens the controlling terminal for the pairing confirmation
// (Story 94.3). It reaches the terminal even when stdin and stdout are pipes
// (curl ... | bash); without a controlling terminal the open fails (ENXIO)
// and pairing turns to the browser confirmation. runPair also needs stdout
// or stderr to be a terminal before it asks here.
func openTerminal() (io.ReadWriteCloser, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

// flushPendingInput discards input typed before the pairing question (an
// Enter pressed while the install downloaded would otherwise answer it),
// best effort: anything that is not the opened terminal file, or a failed
// flush, is ignored.
func flushPendingInput(tty io.ReadWriter) {
	f, ok := tty.(*os.File)
	if !ok {
		return
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) { _ = flushInputFD(int(fd)) })
}
