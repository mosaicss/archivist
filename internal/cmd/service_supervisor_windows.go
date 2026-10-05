//go:build windows

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"

	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/service"
	"github.com/mosaicss/archivist/internal/winjob"
)

// supervisedEnvKey marks the daemon child the supervisor starts.
const supervisedEnvKey = "ARCHIVIST_SERVICE_SUPERVISED"

// companionExe is the console-less build of archivist (-H windowsgui) the
// scheduled task runs, so no console or Terminal window opens at login.
const companionExe = "archivistw.exe"

// installPS1 is the Windows one-liner that installs both executables.
const installPS1 = "& ([scriptblock]::Create((irm https://github.com/mosaicss/archivist/releases/latest/download/install.ps1)))"

// serviceBinary is archivistw.exe next to the stable archivist.exe.
func serviceBinary(stable string) string {
	return filepath.Join(filepath.Dir(stable), companionExe)
}

// checkServiceBinary refuses --install when archivistw.exe is missing (an
// npm install copies only archivist.exe).
func checkServiceBinary(stderr io.Writer) error {
	stable, err := stableBinary()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: locate this binary: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	companion := serviceBinary(stable)
	if st, err := os.Stat(companion); err != nil || !st.Mode().IsRegular() {
		_, _ = fmt.Fprintf(stderr, "archivist connect: background connect needs %s next to %s, and it is missing. Install archivist with install.ps1 (in PowerShell):\n  %s\n", companionExe, stable, installPS1)
		return &ExitError{Code: ExitGenericError}
	}
	return nil
}

// serviceSupervisorNeeded is true in the process the task starts and false
// in the daemon child it supervises.
func serviceSupervisorNeeded() bool { return os.Getenv(supervisedEnvKey) != "1" }

// runServiceSupervisor runs this same binary with args (connect --service
// and the flags this command line gave, serviceChildArgs) as the daemon
// child, with the captured service environment (service.env) and the
// supervised marker, inside a kill on close job: when Task Scheduler ends
// the task, or at logoff, the supervisor is terminated, its job handle
// closes and the daemon's whole tree dies with it.
func runServiceSupervisor(ctx context.Context, log *connect.Logger, home string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("supervisor: locate this binary: %v", err)
		return err
	}
	log.Printf("supervisor: running the daemon (%s)", filepath.Base(exe))
	return superviseDaemon(ctx, log, func() (func() error, error) {
		return startSupervisedDaemon(exe, args, home)
	})
}

// startSupervisedDaemon starts exe args in home inside its own job, with
// service.env applied to this environment (keys matched case-insensitively,
// ARCHIVIST_TOKEN dropped) and the supervised marker set.
func startSupervisedDaemon(exe string, args []string, home string) (func() error, error) {
	c := exec.Command(exe, args...)
	c.Dir = home
	c.Env = service.MergeEnv(os.Environ(), service.ReadEnvFile(home), true, supervisedEnvKey+"=1")
	job, err := winjob.Start(c)
	if err != nil {
		return nil, err
	}
	return func() error {
		err := c.Wait()
		_ = job.Close()
		return err
	}, nil
}

// setServiceCrashOutput sends fatal runtime output (a panic or fatal error)
// to the service log as well: archivistw.exe has no stderr anyone sees. The
// runtime keeps its own duplicate of the log handle; the returned function
// releases it (SetCrashOutput(nil)), so the log can be rotated or removed
// once service mode returns.
func setServiceCrashOutput(logf *os.File) func() {
	if debug.SetCrashOutput(logf, debug.CrashOptions{}) != nil {
		return func() {}
	}
	return func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) }
}
