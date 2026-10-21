//go:build windows

package connect

import "os"

// cwdOwnerProblem checks nothing on Windows: the folder sits in the
// user's profile, whose ACL keeps other accounts out.
func cwdOwnerProblem(os.FileInfo) string { return "" }
