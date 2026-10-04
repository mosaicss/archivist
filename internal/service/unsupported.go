//go:build !darwin && !linux

package service

// newPlatform refuses: Windows (and any other platform) has no background
// service yet. Install and pair still work there.
func newPlatform(Options) (Manager, error) { return nil, ErrUnsupported }
