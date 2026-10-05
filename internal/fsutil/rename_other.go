//go:build !windows

package fsutil

import "os"

// retryableRename is false outside Windows: a rename there either works or
// fails for good.
func retryableRename(error) bool { return false }

func readFile(name string) ([]byte, error) { return os.ReadFile(name) }

// retryableRemove is false outside Windows.
func retryableRemove(error) bool { return false }
