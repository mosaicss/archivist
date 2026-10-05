//go:build windows

package fsutil

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// retryableRename reports a rename refused because the target is open
// without FILE_SHARE_DELETE (another process reading a token file).
func retryableRename(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// retryableRemove reports a removal refused while a handle in the tree is
// still open or a deletion is still pending.
func retryableRemove(err error) bool {
	return retryableRename(err) || errors.Is(err, windows.ERROR_DIR_NOT_EMPTY)
}

// readFile reads name through a handle that shares read, write and delete
// access (os.Open shares no delete access, so a concurrent rename over the
// file, or an open during one, fails with a sharing violation).
func readFile(name string) ([]byte, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(h), name)
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}
