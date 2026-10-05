//go:build windows

package service

import "os/user"

// newPlatform returns the per-user logon scheduled task manager (Story
// 78.34). The task names the current user, and its name is derived from the
// account's SID (user.Current().Uid on Windows), when the caller gave none.
func newPlatform(opts Options) (Manager, error) {
	if opts.User == "" || opts.SID == "" {
		if u, err := user.Current(); err == nil {
			if opts.User == "" {
				opts.User = u.Username
			}
			if opts.SID == "" {
				opts.SID = u.Uid
			}
		}
	}
	return newSchtasks(opts), nil
}
