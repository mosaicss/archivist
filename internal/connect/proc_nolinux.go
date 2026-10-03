//go:build !linux

package connect

// Without PR_SET_CHILD_SUBREAPER, orphans go to init/launchd; the stop path
// relies on the descendant snapshots each harness records (Proc.Snapshot).

func enableSubreaper() error { return nil }

func sweepOrphans(owner *Proc, all bool) {}

// SweepAllOrphans is a no-op where the daemon cannot adopt orphans.
func SweepAllOrphans() {}

func reapZombies() {}
