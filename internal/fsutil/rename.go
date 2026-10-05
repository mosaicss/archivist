// Package fsutil holds small file system helpers shared by packages that
// replace files atomically.
package fsutil

import (
	"os"
	"time"
)

// Rename is os.Rename, retried briefly on Windows when the target is held
// open by a reader that did not allow deletion (a sharing violation or an
// access denied answer that clears within moments), about one second in
// total (Story 78.34). Elsewhere it is os.Rename exactly.
func Rename(oldpath, newpath string) error {
	err := os.Rename(oldpath, newpath)
	for i := 0; err != nil && i < renameRetries && retryableRename(err); i++ {
		time.Sleep(renameRetryGap)
		err = os.Rename(oldpath, newpath)
	}
	return err
}

// ReadFile is os.ReadFile for a file another process replaces atomically
// (Rename): on Windows the file is opened with FILE_SHARE_DELETE, so the
// writer's replace never meets this reader's handle, and an open refused
// while the replace holds the file (a sharing violation) is retried for
// about one second (Story 78.34). Elsewhere it is os.ReadFile exactly.
func ReadFile(name string) ([]byte, error) {
	b, err := readFile(name)
	for i := 0; err != nil && i < renameRetries && retryableRename(err); i++ {
		time.Sleep(renameRetryGap)
		b, err = readFile(name)
	}
	return b, err
}

// RemoveAll is os.RemoveAll, retried on Windows for about two seconds while
// files or directories in the tree are still held open (a process that was
// just ended releases its handles, its working directory among them, as it
// exits) or are pending deletion (Story 78.34). Elsewhere it is
// os.RemoveAll exactly.
func RemoveAll(path string) error {
	err := os.RemoveAll(path)
	for i := 0; err != nil && i < removeRetries && retryableRemove(err); i++ {
		time.Sleep(renameRetryGap)
		err = os.RemoveAll(path)
	}
	return err
}

// removeRetries is the RemoveAll budget: 40 retries 50 ms apart.
const removeRetries = 40

// Retry budget: 20 retries 50 ms apart.
const (
	renameRetries  = 20
	renameRetryGap = 50 * time.Millisecond
)
