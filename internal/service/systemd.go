package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mosaicss/archivist/internal/fsutil"
)

// defaultLingerDir holds one file per user with lingering on (systemd-logind).
const defaultLingerDir = "/var/lib/systemd/linger"

// Who turned lingering on, as the marker records it (Story 94.6).
const (
	lingerByArchivist = "archivist"
	lingerByUser      = "user"
)

// LingerMarkerPath is ~/.archivist/connect/linger: "archivist" when
// Archivist turned lingering on (so uninstall turns it off), "user" when it
// was already on.
func LingerMarkerPath(home string) string {
	return filepath.Join(home, ".archivist", "connect", "linger")
}

// readLingerMarker returns the marker's owner, or "" when there is none.
func readLingerMarker(home string) string {
	b, err := fsutil.ReadFile(LingerMarkerPath(home))
	if err != nil {
		return ""
	}
	switch v := strings.TrimSpace(string(b)); v {
	case lingerByArchivist, lingerByUser:
		return v
	}
	return ""
}

// lingerOn reports whether lingering is on for the user (the file systemd-logind
// keeps per user) and whether that could be read at all.
func (o Options) lingerOn() (on, known bool) {
	if o.User == "" || strings.ContainsAny(o.User, `/\`) {
		return false, false
	}
	dir := o.LingerDir
	if dir == "" {
		dir = defaultLingerDir
	}
	_, err := os.Stat(filepath.Join(dir, o.User))
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, os.ErrNotExist):
		return false, true
	}
	return false, false
}

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
	if err := writeFileAtomic(path, []byte(RenderSystemdUnit(s.opts.Binary, s.opts.Args, s.opts.Env)), 0o644); err != nil {
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
	// Story 94.6: who turned lingering on decides whether uninstall turns it
	// off; read before asking for it.
	before, known := s.opts.lingerOn()
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
		rep.LingerByArchivist = s.recordLinger(before, known, existed)
	}
	return rep, nil
}

// recordLinger writes the linger marker after lingering was enabled and
// reports whether Archivist owns it (Story 94.6, strict): "archivist" only
// when this install saw lingering off and turned it on; an earlier record
// is kept otherwise; "user" when it was already on at a first install. A
// unit installed before the marker existed (v0.2.34), or a linger state
// that could not be read, has unknown provenance: nothing is recorded, and
// uninstall leaves lingering on.
func (s *systemd) recordLinger(before, known, existed bool) bool {
	owner := readLingerMarker(s.opts.Home)
	switch {
	case known && !before:
		owner = lingerByArchivist
	case owner != "":
	case known && !existed:
		owner = lingerByUser
	default:
		return false
	}
	if err := writeFileAtomic(LingerMarkerPath(s.opts.Home), []byte(owner+"\n"), 0o600); err != nil {
		return false
	}
	return owner == lingerByArchivist
}

// LingerRelease is what ReleaseLinger did.
type LingerRelease int

const (
	// LingerLeft: nothing to do (the user's lingering, or it is off).
	LingerLeft LingerRelease = iota
	// LingerTurnedOff: Archivist had turned lingering on; disable-linger ran.
	LingerTurnedOff
	// LingerUnknownOwner: lingering is (or may be) on and no record says
	// Archivist turned it on (an install from before the marker, or a state
	// that could not be read); it stays on.
	LingerUnknownOwner
)

// LingerReleaser is a manager that may have turned lingering on (systemd).
type LingerReleaser interface {
	// ReleaseLinger turns lingering off when Archivist turned it on; call it
	// on uninstall, before ~/.archivist/connect is removed, with whether the
	// unit was installed.
	ReleaseLinger(ctx context.Context, unitInstalled bool) (LingerRelease, error)
}

// ReleaseLinger turns lingering off (loginctl disable-linger) only when the
// marker says Archivist turned it on. A marker saying the user had it on
// leaves it silently; no marker with the unit installed and lingering on
// (or unreadable) reports LingerUnknownOwner and leaves it.
func (s *systemd) ReleaseLinger(ctx context.Context, unitInstalled bool) (LingerRelease, error) {
	switch readLingerMarker(s.opts.Home) {
	case lingerByUser:
		return LingerLeft, nil
	case "":
		if on, known := s.opts.lingerOn(); unitInstalled && (on || !known) {
			return LingerUnknownOwner, nil
		}
		return LingerLeft, nil
	}
	args := []string{"--no-ask-password", "disable-linger"}
	if s.opts.User != "" {
		args = append(args, s.opts.User)
	}
	r, err := s.opts.run(ctx, "loginctl", args...)
	switch {
	case err != nil:
		return LingerLeft, fmt.Errorf("loginctl is not available: %w", err)
	case !r.OK():
		return LingerLeft, commandError("loginctl --no-ask-password disable-linger", r)
	}
	_ = os.Remove(LingerMarkerPath(s.opts.Home))
	return LingerTurnedOff, nil
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
	st.Args = installedArgs(st.Path)
	r, err := s.systemctl(ctx, "is-active", "--quiet", UnitName)
	if err != nil {
		return st, err
	}
	st.Running = r.OK()
	return st, nil
}
