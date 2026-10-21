//go:build windows

package cmd

import "os"

// ownedByUser is true on Windows: the temporary directory is the user's own
// (%LOCALAPPDATA%\Temp).
func ownedByUser(os.FileInfo) bool { return true }
