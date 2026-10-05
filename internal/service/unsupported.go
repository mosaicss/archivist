//go:build !darwin && !linux && !windows

package service

// newPlatform refuses: this platform has no background service. Install
// and pair still work there.
func newPlatform(Options) (Manager, error) { return nil, ErrUnsupported }
