package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// systemd manages ~/.config/systemd/user/archivist-connect.service.
type systemd struct{ opts Options }

func newSystemd(opts Options) *systemd { return &systemd{opts: opts} }

func (s *systemd) Name() string { return "systemd" }

func (s *systemd) Path() string {
	base := s.opts.ConfigHome
	if base == "" || !filepath.IsAbs(base) {
		base = filepath.Join(s.opts.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user", UnitName)
}

func (s *systemd) systemctl(ctx context.Context, args ...string) (Result, error) {
	r, err := s.opts.run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil {
		return r, fmt.Errorf("systemd user services are not available here (systemctl: %w). Run archivist connect in a terminal instead", err)
	}
	return r, nil
}

// Install writes the unit, reloads the user manager, enables and starts it
// (restarting it when it already ran, so a new binary or environment takes
// effect) and asks for lingering so the user manager, and with it the
// service, starts at boot without a login. A failed reload or enable
// removes a unit file this call created.
func (s *systemd) Install(ctx context.Context) (InstallReport, error) {
	if s.opts.Binary == "" || !filepath.IsAbs(s.opts.Binary) {
		return InstallReport{}, fmt.Errorf("service: binary path %q is not absolute", s.opts.Binary)
	}
	path := s.Path()
	rep := InstallReport{Path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return rep, err
	}
	if err := EnsureLog(s.opts.Home); err != nil {
		return rep, err
	}
	existed := fileExists(path)
	if err := writeFileAtomic(path, []byte(RenderSystemdUnit(s.opts.Binary, s.opts.Env)), 0o644); err != nil {
		return rep, err
	}
	fail := func(err error) (InstallReport, error) {
		if !existed {
			_ = os.Remove(path)
			_, _ = s.systemctl(ctx, "daemon-reload")
		}
		return rep, err
	}
	r, err := s.systemctl(ctx, "daemon-reload")
	if err != nil {
		return fail(err)
	}
	if !r.OK() {
		return fail(commandError("systemctl --user daemon-reload", r))
	}
	active, _ := s.systemctl(ctx, "is-active", "--quiet", UnitName)
	if r, err = s.systemctl(ctx, "enable", "--now", UnitName); err != nil {
		return fail(err)
	}
	if !r.OK() {
		return fail(commandError("systemctl --user enable --now "+UnitName, r))
	}
	if active.OK() {
		if r, err = s.systemctl(ctx, "restart", UnitName); err != nil {
			return rep, err
		}
		if !r.OK() {
			return rep, commandError("systemctl --user restart "+UnitName, r)
		}
		rep.Restarted = true
	}
	// --no-ask-password: a polkit prompt must never block a piped install.
	args := []string{"--no-ask-password", "enable-linger"}
	if s.opts.User != "" {
		args = append(args, s.opts.User)
	}
	lr, lerr := s.opts.run(ctx, "loginctl", args...)
	switch {
	case lerr != nil:
		rep.Linger = "loginctl is not available: " + lerr.Error()
	case !lr.OK():
		rep.Linger = commandError("loginctl --no-ask-password enable-linger", lr).Error()
	default:
		rep.Linger = "enabled"
		rep.LingerOK = true
	}
	return rep, nil
}

// Uninstall disables and stops the unit, removes it and reloads.
func (s *systemd) Uninstall(ctx context.Context) (bool, error) {
	path := s.Path()
	if !fileExists(path) {
		return false, nil
	}
	r, err := s.systemctl(ctx, "disable", "--now", UnitName)
	var stopErr error
	switch {
	case err != nil:
		stopErr = err
	case !r.OK():
		stopErr = commandError("systemctl --user disable --now "+UnitName, r)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	_, _ = s.systemctl(ctx, "daemon-reload")
	return true, stopErr
}

// Status reads is-active's exit code.
func (s *systemd) Status(ctx context.Context) (Status, error) {
	st := Status{Manager: s.Name(), Path: s.Path(), Installed: fileExists(s.Path())}
	if !st.Installed {
		return st, nil
	}
	r, err := s.systemctl(ctx, "is-active", "--quiet", UnitName)
	if err != nil {
		return st, err
	}
	st.Running = r.OK()
	return st, nil
}
