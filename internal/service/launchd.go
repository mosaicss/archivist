package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// launchd manages ~/Library/LaunchAgents/com.mosaic-finance.archivist.connect.plist
// in the gui/<uid> domain.
type launchd struct{ opts Options }

func newLaunchd(opts Options) *launchd { return &launchd{opts: opts} }

func (l *launchd) Name() string { return "launchd" }

func (l *launchd) Path() string {
	return filepath.Join(l.opts.Home, "Library", "LaunchAgents", Label+".plist")
}

func (l *launchd) domain() string { return fmt.Sprintf("gui/%d", l.opts.UID) }

func (l *launchd) target() string { return l.domain() + "/" + Label }

func (l *launchd) launchctl(ctx context.Context, args ...string) (Result, error) {
	r, err := l.opts.run(ctx, "launchctl", args...)
	if err != nil {
		return r, fmt.Errorf("launchctl is not available (%w). Run archivist connect in a terminal instead", err)
	}
	return r, nil
}

// loaded reports whether the job is loaded (launchctl print exits 0).
func (l *launchd) loaded(ctx context.Context) (bool, error) {
	r, err := l.launchctl(ctx, "print", l.target())
	if err != nil {
		return false, err
	}
	return r.OK(), nil
}

// Polling bounds: bootout returns before the job has left the domain, and
// a bootstrap inside that window fails with "5: Input/output error".
const (
	launchdPoll         = 250 * time.Millisecond
	launchdUnloadWait   = 40 // polls (10 s)
	launchdBootstraps   = 4
	launchdBootstrapGap = time.Second
)

// Install writes the plist, boots out a loaded copy and waits until it has
// left the domain, clears a disabled override, then bootstraps the agent
// (RunAtLoad starts it). A failed bootstrap removes a plist this call
// created.
func (l *launchd) Install(ctx context.Context) (InstallReport, error) {
	if l.opts.Binary == "" || !filepath.IsAbs(l.opts.Binary) {
		return InstallReport{}, fmt.Errorf("service: binary path %q is not absolute", l.opts.Binary)
	}
	path := l.Path()
	rep := InstallReport{Path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return rep, err
	}
	if err := EnsureLog(l.opts.Home); err != nil {
		return rep, err
	}
	was, err := l.loaded(ctx)
	if err != nil {
		return rep, err
	}
	existed := fileExists(path)
	plist := RenderLaunchdPlist(l.opts.Binary, l.opts.Home, LogPath(l.opts.Home), l.opts.Env)
	if err := writeFileAtomic(path, []byte(plist), 0o644); err != nil {
		return rep, err
	}
	if was {
		if _, err := l.launchctl(ctx, "bootout", l.target()); err != nil {
			return rep, err
		}
		for i := 0; i < launchdUnloadWait; i++ {
			if still, _ := l.loaded(ctx); !still {
				break
			}
			l.opts.sleep(launchdPoll)
		}
		rep.Restarted = true
	}
	// A job disabled earlier (launchctl disable) refuses to bootstrap.
	_, _ = l.launchctl(ctx, "enable", l.target())
	var last Result
	for i := 0; i < launchdBootstraps; i++ {
		if i > 0 {
			l.opts.sleep(launchdBootstrapGap)
		}
		if last, err = l.launchctl(ctx, "bootstrap", l.domain(), path); err != nil {
			break
		}
		if last.OK() {
			return rep, nil
		}
	}
	if err == nil {
		err = commandError("launchctl bootstrap "+l.domain(), last)
	}
	if !existed {
		_ = os.Remove(path)
	}
	return rep, err
}

// Uninstall boots the job out and removes the plist.
func (l *launchd) Uninstall(ctx context.Context) (bool, error) {
	path := l.Path()
	was, err := l.loaded(ctx)
	if err != nil {
		return false, err
	}
	if !was && !fileExists(path) {
		return false, nil
	}
	var stopErr error
	if was {
		r, err := l.launchctl(ctx, "bootout", l.target())
		switch {
		case err != nil:
			stopErr = err
		case !r.OK():
			stopErr = commandError("launchctl bootout "+l.target(), r)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, stopErr
}

// Status reads launchctl print's exit code (loaded or not) and the daemon's
// service.pid: a loaded job whose daemon exited 0 (superseded, no harness,
// revoked key) stays loaded, so running also needs that pid alive.
func (l *launchd) Status(ctx context.Context) (Status, error) {
	st := Status{Manager: l.Name(), Path: l.Path(), Installed: fileExists(l.Path())}
	loaded, err := l.loaded(ctx)
	if err != nil {
		return st, err
	}
	st.Loaded = loaded
	st.Running = loaded && l.opts.alive(readPID(l.opts.Home))
	return st, nil
}
