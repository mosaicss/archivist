package cmd

// Story 78.30: one-step agent connect. `--pair` redeems a workspace pairing
// code for a new ak_ key, `--install`/`--uninstall`/`--status` manage the
// background user service, and the hidden `--service` is what that service
// runs: the normal daemon, logging to ~/.archivist/connect/connect.log, with
// terminal outcomes mapped to exit 0 so the manager does not restart them.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/service"
	"github.com/spf13/cobra"
)

// serviceRunner runs service manager commands; tests replace it.
var serviceRunner service.Runner = service.ExecRunner

// pairInvalidMessage is the one answer for every unusable pairing code,
// whether refused locally or by the server.
const pairInvalidMessage = "This pairing code is not valid or has expired. Get a new code in Mosaic and run the command again."

// setupMode is which one-step setup flag was given.
type setupMode struct {
	pair, install, uninstall, status, service bool
}

func (m setupMode) count() int {
	n := 0
	for _, b := range []bool{m.pair, m.install, m.uninstall, m.status, m.service} {
		if b {
			n++
		}
	}
	return n
}

// validate enforces: one setup flag at most, none with --check or the
// session-bound flags, harness settings only for the daemon (plain or
// --service), and positional arguments only as the rest of a --pair code.
func (m setupMode) validate(stderr io.Writer, args []string, check, session, harnessFlags bool) error {
	usage := func(msg string) error {
		_, _ = fmt.Fprintln(stderr, "Error: "+msg)
		return &ExitError{Code: ExitUsageError}
	}
	if len(args) > 0 && !m.pair {
		return usage(fmt.Sprintf("unknown command %q for \"archivist connect\"", args[0]))
	}
	n := m.count()
	if n == 0 {
		return nil
	}
	if n > 1 {
		return usage("--pair, --install, --uninstall and --status are used one at a time")
	}
	if check || session {
		return usage("--pair, --install, --uninstall, --status and --service cannot be combined with --check, --session, --agent or --prompt-file")
	}
	if harnessFlags && !m.service {
		return usage("--claude-model, --claude-effort, --codex-model and --codex-effort apply only when connect runs")
	}
	return nil
}

// validateControls checks the Story 78.32 flags against the setup mode:
// --max-permission only with --install (written into the service) or
// --service (what the service runs), and a valid mode with --install;
// --mode, --model and --effort never with a setup flag (they need --session,
// which no setup flag takes). The plain daemon and --session check them in
// permissionFlags; --service checks the mode in runServiceMode, which logs
// a bad one and exits 0 (an exit 2 would restart every 10 s forever).
func (m setupMode) validateControls(stderr io.Writer, maxPermission string, flags sessionControlFlags) error {
	if m.count() == 0 {
		return nil
	}
	usage := func(format string, args ...any) error {
		_, _ = fmt.Fprintf(stderr, "Error: "+format+"\n", args...)
		return &ExitError{Code: ExitUsageError}
	}
	for _, f := range []struct{ name, value string }{{"--mode", flags.mode}, {"--model", flags.model}, {"--effort", flags.effort}} {
		if f.value != "" {
			return usage("%s needs --session (workspace sessions choose it in the workspace)", f.name)
		}
	}
	if maxPermission == "" {
		return nil
	}
	if !m.install && !m.service {
		return usage("--max-permission applies only when connect runs or with --install")
	}
	if _, ok := connect.ParseMode(maxPermission); !ok && m.install {
		return usage("--max-permission %q is not a mode; use one of %s", maxPermission, connect.ModeHelp())
	}
	return nil
}

// serviceArgs are the daemon flags an install writes into the service
// definition after `connect --service` (Story 78.32: the ceiling, when
// chosen; without it the service runs the default ceiling).
func serviceArgs(maxPermission string) []string {
	if maxPermission == "" {
		return nil
	}
	return []string{"--max-permission", maxPermission}
}

// installedCeiling is the ceiling an installed service runs, from its
// definition's --max-permission (the default when absent), whether the flag
// was found (either form) and whether the value read is a known mode.
func installedCeiling(args []string) (mode connect.Mode, found, ok bool) {
	for i, a := range args {
		if a == "--max-permission" && i+1 < len(args) {
			mode, ok = connect.ParseMode(args[i+1])
			return mode, true, ok
		}
		if v, cut := strings.CutPrefix(a, "--max-permission="); cut {
			mode, ok = connect.ParseMode(v)
			return mode, true, ok
		}
	}
	return connect.DefaultMaxMode, false, true
}

// runPair redeems a pairing code and saves the new key.
func runPair(cmd *cobra.Command, version, raw string) error {
	stderr := cmd.ErrOrStderr()
	code, err := client.NormalizePairingCode(raw)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "archivist connect: "+pairInvalidMessage)
		return &ExitError{Code: ExitAuthError}
	}
	device, _ := os.Hostname()
	c := client.New("", version)
	res, err := c.RedeemPairing(cmd.Context(), code, device)
	if err != nil {
		if errors.Is(err, client.ErrPairingCodeInvalid) {
			_, _ = fmt.Fprintln(stderr, "archivist connect: "+pairInvalidMessage)
			return &ExitError{Code: ExitAuthError}
		}
		var apiErr *client.APIError
		if errors.As(err, &apiErr) {
			f := mapAPIError(apiErr)
			// FEATURE_DISABLED (and any unknown route) is a refusal, not a
			// missing resource.
			if apiErr.Status == http.StatusNotFound {
				f.exitCode = ExitGenericError
			}
			return reportFailure(cmd, f, "")
		}
		return failFromDo(cmd, err, "")
	}
	// A valid key saved before stays active in Clerk; say so once.
	previous, perr := auth.SavedToken()
	path, err := auth.SaveToken(res.Key)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: paired, but saving the key failed: %v\nGet a new code in Mosaic and try again.\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	name := res.Name
	if name == "" {
		name = res.KeyID
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Paired as %s.\n  Key:  %s\n  File: %s\n", name, auth.MaskToken(res.Key), path)
	if perr == nil && previous != res.Key && !auth.IsTaskToken(previous) {
		_, _ = fmt.Fprintf(stderr, "Note: the key saved here before (%s) is still active. Revoke it in Mosaic if you no longer need it.\n", auth.MaskToken(previous))
	}
	if os.Getenv("ARCHIVIST_TOKEN") != "" {
		_, _ = fmt.Fprintln(stderr, "Warning: ARCHIVIST_TOKEN is set in this environment and takes precedence over the saved key here. Unset it to use the paired key. The background service always uses the saved key.")
	}
	return nil
}

// newServiceManager builds this platform's manager, or prints why there is
// none (exit 1).
func newServiceManager(cmd *cobra.Command, args []string) (service.Manager, string, error) {
	stderr := cmd.ErrOrStderr()
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return nil, "", &ExitError{Code: ExitGenericError}
	}
	opts := service.Options{Home: home, Run: serviceRunner, UID: os.Getuid(), Env: service.CaptureEnv(os.Environ()), Args: args}
	if u, err := user.Current(); err == nil {
		opts.User = u.Username
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		opts.ConfigHome = xdg
	}
	if bin, err := stableBinary(); err == nil {
		// Windows runs the console-less companion next to it (Story 78.34).
		opts.Binary = serviceBinary(bin)
	}
	m, err := service.New(opts)
	if err != nil {
		if errors.Is(err, service.ErrUnsupported) {
			_, _ = fmt.Fprintln(stderr, "archivist connect: Background connect is not available on this system yet.")
		} else {
			_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		}
		return nil, "", &ExitError{Code: ExitGenericError}
	}
	return m, home, nil
}

// stableBinary is the path the service runs: the archivist on PATH when it
// is this same binary (a package manager's symlink survives upgrades, a
// versioned Cellar or Caskroom path does not), else this executable.
func stableBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	if p, err := exec.LookPath("archivist"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			if r, err := filepath.EvalSymlinks(abs); err == nil && r == resolved {
				return abs, nil
			}
		}
	}
	if filepath.IsAbs(exe) {
		return exe, nil
	}
	return resolved, nil
}

// runServiceInstall installs and starts the background service, running
// `connect --service` with --max-permission when one was given. Without
// the flag, an installed service's own --max-permission is kept, so an
// update (the install script reinstalls on every update) never changes the
// ceiling; --max-permission auto_edits returns to the default.
func runServiceInstall(cmd *cobra.Command, maxPermission string) error {
	stderr, stdout := cmd.ErrOrStderr(), cmd.OutOrStdout()
	if !connect.Supported() {
		_, _ = fmt.Fprintln(stderr, "archivist connect: Background connect is not available on this system.")
		return &ExitError{Code: ExitGenericError}
	}
	// The service never sees ARCHIVIST_TOKEN: it needs a saved owner key.
	token, err := auth.SavedToken()
	if err != nil || !strings.HasPrefix(token, "ak_") {
		_, _ = fmt.Fprintln(stderr, "archivist connect --install needs a saved ak_ key. Run 'archivist connect --pair CODE' with a code from Mosaic (or 'archivist auth login --token ak_...') first.")
		return &ExitError{Code: ExitAuthError}
	}
	if err := checkServiceBinary(stderr); err != nil {
		return err
	}
	kept := false
	if maxPermission == "" {
		cur, _, err := newServiceManager(cmd, nil)
		if err != nil {
			return err
		}
		if b, err := os.ReadFile(cur.Path()); err == nil {
			// Only a flag in the definition counts; none means the default.
			if c, found, ok := installedCeiling(service.ServiceArgs(string(b))); found && ok {
				maxPermission, kept = string(c), true
			}
		}
	}
	m, home, err := newServiceManager(cmd, serviceArgs(maxPermission))
	if err != nil {
		return err
	}
	rep, err := m.Install(cmd.Context())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: could not install the background service: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	verb := "installed and started"
	if rep.Restarted {
		verb = "updated and restarted"
	}
	_, _ = fmt.Fprintf(stdout, "Background connect %s (%s: %s).\n", verb, m.Name(), rep.Path)
	switch {
	case m.Name() == "launchd" || m.Name() == "Task Scheduler":
		_, _ = fmt.Fprintln(stdout, "It runs now and starts again at every login.")
	case rep.LingerOK:
		_, _ = fmt.Fprintln(stdout, "It runs now and starts again at boot (lingering enabled).")
	default:
		_, _ = fmt.Fprintf(stdout, "It runs now and starts again at your next login. Starting at boot needs lingering, which could not be enabled: %s\n", rep.Linger)
	}
	ceiling, _, _ := installedCeiling(serviceArgs(maxPermission))
	switch {
	case kept:
		_, _ = fmt.Fprintf(stdout, "Permission ceiling: %s (%s), kept from the installed service; change it with 'archivist connect --install --max-permission <mode>'.\n", ceiling, ceiling.Label())
	case maxPermission == "":
		_, _ = fmt.Fprintf(stdout, "Permission ceiling: %s (%s), the default; choose another with 'archivist connect --install --max-permission <mode>'.\n", ceiling, ceiling.Label())
	default:
		_, _ = fmt.Fprintf(stdout, "Permission ceiling: %s (%s).\n", ceiling, ceiling.Label())
	}
	_, _ = fmt.Fprintf(stdout, "Log: %s\nCheck it with 'archivist connect --status'; remove it with 'archivist connect --uninstall'.\n", service.LogPath(home))
	if os.Getenv("ARCHIVIST_TOKEN") != "" {
		_, _ = fmt.Fprintln(stderr, "Note: the service uses the saved key, not ARCHIVIST_TOKEN.")
	}
	return nil
}

// runServiceUninstall stops and removes the background service.
func runServiceUninstall(cmd *cobra.Command) error {
	m, _, err := newServiceManager(cmd, nil)
	if err != nil {
		return err
	}
	removed, err := m.Uninstall(cmd.Context())
	if err != nil && !removed {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist connect: could not remove the background service: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	if !removed {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Background connect is not installed.")
		return nil
	}
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: stopping the service reported: %v\n", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Background connect removed (%s). Your saved key is kept; revoke it in Mosaic if you no longer need it.\n", m.Path())
	return nil
}

// runServiceStatus reports the service and the saved key: exit 0 when the
// service runs (systemd: is-active; launchd: loaded and its daemon alive)
// and a saved ak_ key exists, else 1.
func runServiceStatus(cmd *cobra.Command) error {
	m, home, err := newServiceManager(cmd, nil)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	st, err := m.Status(cmd.Context())
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist connect: %v\n", err)
	}
	state := "not installed (run 'archivist connect --install')"
	switch {
	case st.Installed && st.Running:
		state = "running"
	case st.Installed && st.Loaded:
		state = "loaded, not running (the daemon stopped; see the log, then run 'archivist connect --install' to start it again)"
	case st.Installed:
		state = "installed, not running (run 'archivist connect --install' to start it again)"
	}
	_, _ = fmt.Fprintf(out, "Service:  %s (%s: %s)\n", state, m.Name(), m.Path())
	if st.Installed {
		if ceiling, _, ok := installedCeiling(st.Args); ok {
			_, _ = fmt.Fprintf(out, "Ceiling:  %s (%s)\n", ceiling, ceiling.Label())
		} else {
			_, _ = fmt.Fprintln(out, "Ceiling:  unreadable (run 'archivist connect --install --max-permission <mode>' again)")
		}
	}
	token, terr := auth.SavedToken()
	keyOK := terr == nil && strings.HasPrefix(token, "ak_")
	if keyOK {
		_, _ = fmt.Fprintf(out, "Key:      saved, %s (fp:%s)\n", auth.MaskToken(token), auth.Fingerprint(token))
	} else {
		_, _ = fmt.Fprintln(out, "Key:      none (run 'archivist connect --pair CODE' with a code from Mosaic)")
	}
	_, _ = fmt.Fprintf(out, "Log:      %s\n", service.LogPath(home))
	if line := service.LastLogLine(home); line != "" {
		_, _ = fmt.Fprintf(out, "Last log: %s\n", connect.Scrub(line))
	}
	if st.Installed && st.Running && keyOK {
		return nil
	}
	return &ExitError{Code: ExitGenericError}
}

// runServiceMode is what the service definition runs (on Windows through
// the supervisor, runServiceSupervisor): the normal daemon
// with its output in the log file, using the saved credentials file only
// (ARCHIVIST_TOKEN and --token are ignored, even when the user manager or
// launchctl setenv injects them). It writes ~/.archivist/connect/service.pid
// while it runs, so --status can tell a loaded launchd job from a live
// daemon. A terminal outcome (stopped, superseded, credential or login
// problem, feature off, no usable harness, bad configuration, including a
// --max-permission this version does not know, as after a downgrade) exits
// 0 so the manager leaves it stopped until 'archivist connect --install', the
// next login (macOS, Windows logon task, Linux without lingering) or the next
// boot; an unexpected failure exits non-zero and the manager (on Windows the
// supervisor) restarts it after 10 s.
func runServiceMode(cmd *cobra.Command, version string, f connectFlags, maxPermission string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	logf, err := service.OpenLog(home)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist connect: open log: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	defer func() { _ = logf.Close() }()
	cmd.SetOut(logf)
	cmd.SetErr(logf)
	log := connect.NewLogger(logf)
	log.Printf("service start %s (pid %d, %s)", version, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	// Windows: the task runs a supervisor that runs this same command as
	// the daemon (Story 78.34); elsewhere the service manager supervises.
	defer setServiceCrashOutput(logf)()
	if serviceSupervisorNeeded() {
		return runServiceSupervisor(cmd.Context(), log, home, serviceChildArgs(cmd))
	}
	var refusal strings.Builder
	maxMode, err := permissionFlags(&refusal, maxPermission, sessionControlFlags{}, nil)
	if err != nil {
		log.Printf("%s; run 'archivist connect --install' (add --max-permission <mode> to choose one) to rewrite the service", strings.TrimSpace(refusal.String()))
		return serviceExit(log, err)
	}
	f.maxMode = maxMode
	log.Printf("permission ceiling %s", f.maxMode)
	savedKeyOnly(cmd)
	removePID, err := service.WritePID(home, os.Getpid())
	if err != nil {
		log.Printf("warning: could not write %s: %v", service.PIDPath(home), err)
	} else {
		defer removePID()
	}
	return serviceExit(log, runConnect(cmd, version, false, f))
}

// savedKeyOnly makes the credential ladder fall through to the saved
// credentials file: ARCHIVIST_TOKEN is removed from this process and the
// --token flag cleared.
func savedKeyOnly(cmd *cobra.Command) {
	_ = os.Unsetenv("ARCHIVIST_TOKEN")
	if fl := cmd.Root().PersistentFlags().Lookup("token"); fl != nil {
		_ = fl.Value.Set("")
	}
}

// serviceExit maps a daemon result to the service exit.
func serviceExit(log *connect.Logger, err error) error {
	var exitErr *ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr) && !exitErr.unexpected:
		log.Printf("service stopped (exit %d); it stays stopped until 'archivist connect --install', the next login (macOS, Windows logon task, Linux without lingering) or the next boot", exitErr.Code)
		return nil
	default:
		log.Printf("service failed; it restarts in 10 s")
		return err
	}
}
