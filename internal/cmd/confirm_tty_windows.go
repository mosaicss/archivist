//go:build windows

package cmd

import (
	"errors"
	"io"
	"os"
)

// consoleTerminal reads the console input buffer and writes the active
// screen buffer.
type consoleTerminal struct{ in, out *os.File }

func (t consoleTerminal) Read(p []byte) (int, error)  { return t.in.Read(p) }
func (t consoleTerminal) Write(p []byte) (int, error) { return t.out.Write(p) }
func (t consoleTerminal) Close() error                { return errors.Join(t.in.Close(), t.out.Close()) }

// openTerminal opens this process's console for the pairing confirmation
// (Story 94.3): CONIN$ and CONOUT$ reach the console even when stdin and
// stdout are redirected. A process without a console cannot open them, and
// pairing then turns to the browser confirmation. runPair also needs
// stdout or stderr to be a console before it asks here.
func openTerminal() (io.ReadWriteCloser, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	return consoleTerminal{in: in, out: out}, nil
}
