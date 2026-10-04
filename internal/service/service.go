// Package service installs `archivist connect` as a background user service
// (Story 78.30): a launchd agent on macOS, a systemd user unit on Linux. The
// service runs `archivist connect --service` with a captured environment
// and the saved credentials file; it never carries ARCHIVIST_TOKEN.
//
// Every manager command goes through an injectable Runner, and status is
// read from exit codes only (launchctl print output is not an API).
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Result is a finished manager command: its exit code and combined output.
type Result struct {
	ExitCode int
	Output   string
}

// OK reports a zero exit.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Runner runs one manager command. err is set only when the command could
// not run at all (not found, not executable); a non-zero exit is a Result.
type Runner func(ctx context.Context, name string, args ...string) (Result, error)

// ExecRunner runs the command for real.
func ExecRunner(ctx context.Context, name string, args ...string) (Result, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return Result{ExitCode: exitErr.ExitCode(), Output: string(out)}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return Result{Output: string(out)}, nil
}

// Options configure a Manager.
type Options struct {
	// Home is the user's home directory.
	Home string
	// Binary is the stable absolute path of the archivist binary the
	// service runs.
	Binary string
	// Env is written into the service definition (see CaptureEnv).
	Env []EnvVar
	// Run runs manager commands (ExecRunner when nil).
	Run Runner
	// UID is the user id (launchd gui/<uid> domain).
	UID int
	// User is the user name (loginctl enable-linger).
	User string
	// ConfigHome is $XDG_CONFIG_HOME when absolute, else empty (~/.config).
	ConfigHome string
	// Sleep waits between launchd polls (time.Sleep when nil).
	Sleep func(time.Duration)
	// Alive reports whether a process id is alive (signal 0 when nil).
	Alive func(pid int) bool
}

func (o Options) run(ctx context.Context, name string, args ...string) (Result, error) {
	if o.Run == nil {
		return ExecRunner(ctx, name, args...)
	}
	return o.Run(ctx, name, args...)
}

func (o Options) alive(pid int) bool {
	if o.Alive != nil {
		return o.Alive(pid)
	}
	return processAlive(pid)
}

// processAlive sends signal 0: nil means the process exists and is this
// user's (EPERM, another user's process reusing the id, is not ours).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func (o Options) sleep(d time.Duration) {
	if o.Sleep == nil {
		time.Sleep(d)
		return
	}
	o.Sleep(d)
}

// Status is what the service manager reports, from exit codes only.
type Status struct {
	// Manager is "systemd" or "launchd".
	Manager string
	// Path is the unit or plist path.
	Path string
	// Installed is true when the unit or plist file exists.
	Installed bool
	// Loaded is launchd's job loaded (launchctl print exits 0); systemd
	// leaves it false.
	Loaded bool
	// Running is systemd's is-active (Linux), or on macOS the job loaded
	// and the daemon whose pid is in service.pid alive.
	Running bool
}

// InstallReport says what Install did.
type InstallReport struct {
	Path string
	// Restarted is true when a running service was restarted to pick up
	// the new definition or binary.
	Restarted bool
	// Linger is the Linux linger outcome ("enabled", or why it failed);
	// empty on macOS.
	Linger string
	// LingerOK is true when loginctl enable-linger succeeded.
	LingerOK bool
}

// Manager installs, removes and reports the background service.
type Manager interface {
	// Name is "systemd" or "launchd".
	Name() string
	// Path is the unit or plist file.
	Path() string
	Install(ctx context.Context) (InstallReport, error)
	// Uninstall stops and removes the service; removed is false when
	// nothing was installed.
	Uninstall(ctx context.Context) (removed bool, err error)
	Status(ctx context.Context) (Status, error)
}

// ErrUnsupported is returned by New where no background service exists
// (Windows: the daemon itself needs a Job Object first).
var ErrUnsupported = errors.New("background connect is not available on this platform")

// New returns this platform's manager, or ErrUnsupported.
func New(opts Options) (Manager, error) {
	if opts.Home == "" || !filepath.IsAbs(opts.Home) {
		return nil, fmt.Errorf("service: home directory %q is not absolute", opts.Home)
	}
	return newPlatform(opts)
}

// LogPath is the service mode log file, ~/.archivist/connect/connect.log.
func LogPath(home string) string {
	return filepath.Join(home, ".archivist", "connect", "connect.log")
}

// PIDPath is the file the running service daemon writes its process id to,
// ~/.archivist/connect/service.pid.
func PIDPath(home string) string {
	return filepath.Join(home, ".archivist", "connect", "service.pid")
}

// WritePID records pid in PIDPath (0600) and returns a function that
// removes the file again, unless another daemon has replaced it since.
func WritePID(home string, pid int) (func(), error) {
	path := PIDPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	content := strconv.Itoa(pid) + "\n"
	if err := writeFileAtomic(path, []byte(content), 0o600); err != nil {
		return nil, err
	}
	return func() {
		if b, err := os.ReadFile(path); err == nil && string(b) == content {
			_ = os.Remove(path)
		}
	}, nil
}

// readPID returns the pid recorded in PIDPath, or 0.
func readPID(home string) int {
	b, err := os.ReadFile(PIDPath(home))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// maxLogBytes rotates the log at startup once it passes 10 MiB (one
// previous file, connect.log.1, is kept).
const maxLogBytes = 10 << 20

// OpenLog opens the service log for appending: directory 0700, file 0600
// (also when launchd created it first). A log over 10 MiB is rotated to
// connect.log.1 first.
func OpenLog(home string) (*os.File, error) {
	path := LogPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// EnsureLog creates ~/.archivist/connect (0700) and the 0600 log file, so
// neither a service manager nor a first write creates them wider.
func EnsureLog(home string) error {
	f, err := OpenLog(home)
	if err != nil {
		return err
	}
	return f.Close()
}

// LastLogLine returns the log's last non-empty line (at most the last 4 KiB
// are read), or "".
func LastLogLine(home string) string {
	f, err := os.Open(LogPath(home))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	const tail = 4096
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		_, _ = f.Seek(info.Size()-tail, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, tail))
	lines := strings.Split(string(b), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// writeFileAtomic writes data (mode perm) through a temporary file in the
// same directory and a rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// commandError describes a failed manager command with its output.
func commandError(what string, r Result) error {
	out := r.Output
	if len(out) > 2000 {
		out = out[:2000]
	}
	if out == "" {
		return fmt.Errorf("%s failed (exit %d)", what, r.ExitCode)
	}
	return fmt.Errorf("%s failed (exit %d): %s", what, r.ExitCode, strings.TrimSpace(out))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
