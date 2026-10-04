//go:build darwin

package service

// newPlatform returns the launchd agent manager.
func newPlatform(opts Options) (Manager, error) { return newLaunchd(opts), nil }
