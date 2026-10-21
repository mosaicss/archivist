package cmd

// Story 78.30: one-step agent connect. `--pair` redeems a workspace pairing
// code for a new ak_ key, `--install`/`--uninstall`/`--status` manage the
// background user service, and the hidden `--service` is what that service
// runs: the normal daemon, logging to ~/.archivist/connect/connect.log, with
// terminal outcomes mapped to exit 0 so the manager does not restart them.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/fsutil"
	"github.com/mosaicss/archivist/internal/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// serviceRunner runs service manager commands; tests replace it.
var serviceRunner service.Runner = service.ExecRunner

// pairInvalidMessage is the one answer for every unusable pairing code,
// whether refused locally or by the server.
const pairInvalidMessage = "This pairing code is not valid or has expired. Get a new code in Mosaic and run the command again."

// openConfirmTerminal opens the terminal the pairing confirmation asks on
// (Story 94.3); tests replace it.
var openConfirmTerminal = openTerminal

// flushConfirmInput discards terminal input typed ahead of the pairing
// question, right before it is written; tests replace it.
var flushConfirmInput = flushPendingInput

// pairOutputVisible reports whether a person can see the pairing question:
// stdout or stderr is itself a terminal (on Windows, a console). An agent
// that captures the output through pipes cannot answer a terminal it does
// not show, so pairing then turns to the browser. Tests replace it.
var pairOutputVisible = func() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) || term.IsTerminal(int(os.Stderr.Fd()))
}

// pairInterrupt is the context the pairing confirmation waits under, done on
// Ctrl-C, SIGTERM or SIGHUP (the terminal closed): an interrupted answer is
// a no. Tests replace it.
var pairInterrupt = func(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, pairSignals...)
}

// pairSignals end a pairing confirmation: Ctrl-C, SIGTERM and SIGHUP (the
// terminal closed). Windows delivers the first two.
var pairSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

// pairBrowserAvailable reports whether a browser can open the confirm page
// (Story 94.3); tests replace it.
var pairBrowserAvailable = browserAvailable

// openPairBrowser opens the confirm page; its channel gets the opener's exit.
// Tests replace it.
var openPairBrowser = startBrowser

// pairPollEvery is the poll interval for the server's suggestion, 1 to 10
// seconds (2 when it suggests none); tests replace it.
var pairPollEvery = func(seconds int) time.Duration {
	switch {
	case seconds <= 0:
		seconds = 2
	case seconds > 10:
		seconds = 10
	}
	return time.Duration(seconds) * time.Second
}

// pairServiceInstalled reports whether the background service is installed,
// so a manual re-pair can say to restart it; tests replace it.
var pairServiceInstalled = serviceInstalled

// pairRevokeTimeout bounds each key revoke (and decline) while pairing.
const pairRevokeTimeout = 10 * time.Second

// pairPollTimeout bounds one poll of a pending pairing.
const pairPollTimeout = 15 * time.Second

// pairPendingMax caps how long the CLI waits for the browser, whatever the
// server says.
const pairPendingMax = 10 * time.Minute

// accountStatusTimeout bounds the --status account lookup.
const accountStatusTimeout = 5 * time.Second

// setupMode is which one-step setup flag was given (Story 94.6 adds
// --pause and --resume).
type setupMode struct {
	pair, trust, install, uninstall, status, service, pause, resume bool
}

func (m setupMode) count() int {
	n := 0
	for _, b := range []bool{m.pair, m.trust, m.install, m.uninstall, m.status, m.service, m.pause, m.resume} {
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
		return usage("--pair, --trust, --install, --uninstall, --status, --pause and --resume are used one at a time")
	}
	if check || session {
		return usage("--pair, --trust, --install, --uninstall, --status, --pause, --resume and --service cannot be combined with --check, --session, --agent or --prompt-file")
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

// runPair redeems a pairing code, names the account it belongs to, and saves
// the new key only once that account is confirmed (Story 94.3). The
// confirmation is chosen before any request: the terminal, when it opens and
// stdout or stderr shows it, asks "This computer will connect to <email>.
// Continue? [Y/n]"; with no terminal a person can see, the
// workspace confirm page in this computer's browser answers for the account
// signed in there (ARCHIVIST_PAIR_CONFIRM=browser chooses it); with neither,
// pairing stops before the redeem. Nothing is saved before a confirmation. A
// declined terminal pairing revokes the new key; a confirmed one revokes the
// ak_ key it replaced.
func runPair(cmd *cobra.Command, version, raw string) error {
	stderr := cmd.ErrOrStderr()
	// Story 94.7: CODE.FP also names the browser that showed the code; it is
	// checked before anything is sent and never leaves this computer.
	raw, browser, withBrowser := strings.Cut(raw, ".")
	if withBrowser {
		browser = strings.TrimSpace(browser)
		if !connect.ValidBrowserCode(browser) {
			_, _ = fmt.Fprintln(stderr, "archivist connect: "+pairBrowserInvalidMessage)
			return &ExitError{Code: ExitUsageError}
		}
	}
	code, err := client.NormalizePairingCode(raw)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "archivist connect: "+pairInvalidMessage)
		return &ExitError{Code: ExitAuthError}
	}
	// Chosen before the redeem, so pairing where nothing can answer spends
	// no code and creates no key.
	confirm := client.ConfirmTerminal
	var tty io.ReadWriteCloser
	if os.Getenv("ARCHIVIST_PAIR_CONFIRM") == "browser" {
		confirm = client.ConfirmBrowser
	} else if tty, err = openConfirmTerminal(); err != nil || !pairOutputVisible() {
		if err == nil {
			_ = tty.Close()
		}
		tty = nil
		if !pairBrowserAvailable() {
			_, _ = fmt.Fprintln(stderr, "archivist connect: pairing asks you to confirm the account, in a terminal or in a browser on this computer, and neither is available here. Run the command in a terminal.")
			return &ExitError{Code: ExitGenericError}
		}
		confirm = client.ConfirmBrowser
	}
	if tty != nil {
		defer func() { _ = tty.Close() }()
	}
	device, _ := os.Hostname()
	c := client.New("", version)
	res, pending, err := c.RedeemPairing(cmd.Context(), code, device, confirm)
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
	if confirm == client.ConfirmBrowser {
		if res != nil {
			// A server that minted at once cannot wait for the browser.
			_ = revokeKey(cmd.Context(), version, res.Key)
			return pairNotNamed(stderr)
		}
		if res, err = confirmInBrowser(cmd, c, pending); err != nil {
			return err
		}
	} else {
		if res == nil {
			_ = c.DeclinePairing(context.WithoutCancel(cmd.Context()), pending)
			return pairNotNamed(stderr)
		}
		email := printableText(res.Email)
		if email == "" {
			// A server that names no account: nothing to confirm, so nothing is kept.
			_ = revokeKey(cmd.Context(), version, res.Key)
			return pairNotNamed(stderr)
		}
		if !confirmAccount(cmd.Context(), tty, email) {
			if err := revokeKey(cmd.Context(), version, res.Key); err != nil {
				_, _ = fmt.Fprintf(stderr, "Note: the unused new key could not be removed (%v). You can revoke it in Mosaic.\n", err)
			}
			_, _ = fmt.Fprintln(stderr, "Pairing cancelled. Nothing was saved.")
			return &ExitError{Code: ExitGenericError}
		}
	}
	return savePairedKey(cmd, version, res, browser)
}

// pairNotNamed is the refusal for a redeem answer this CLI cannot confirm
// (no account named, or a server that predates the browser confirmation).
func pairNotNamed(stderr io.Writer) error {
	_, _ = fmt.Fprintln(stderr, "archivist connect: Mosaic did not name the account for this code, so nothing was saved. Get a new code in Mosaic and try again.")
	return &ExitError{Code: ExitServerError}
}

// savePairedKey saves a confirmed key, revokes the ak_ key it replaced and
// says so, then records the pairing in the trusted browser list (Story
// 94.7): browser ("" = none named) is trusted, and the list is emptied first
// when it was kept for another account.
func savePairedKey(cmd *cobra.Command, version string, res *client.PairingResult, browser string) error {
	stderr, stdout := cmd.ErrOrStderr(), cmd.OutOrStdout()
	email := printableText(res.Email)
	if email == "" {
		_ = revokeKey(cmd.Context(), version, res.Key)
		return pairNotNamed(stderr)
	}
	previous, perr := auth.SavedToken()
	loc, err := auth.SaveToken(res.Key)
	if err != nil {
		// The new key is unusable without the file: remove it too.
		_ = revokeKey(cmd.Context(), version, res.Key)
		_, _ = fmt.Fprintf(stderr, "archivist connect: paired, but saving the key failed: %v\nGet a new code in Mosaic and try again.\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	name := printableText(res.Name)
	if name == "" {
		name = res.KeyID
	}
	_, _ = fmt.Fprintf(stdout, "Paired as %s.\n  Account: %s\n  Key:     %s\n%s", name, email, auth.MaskToken(res.Key), savedLine(loc))
	if err := pairBrowsers(email, browser); err != nil {
		_, _ = fmt.Fprintf(stderr, "Note: the list of browsers that can use this computer was not updated (%v). In Mosaic, open Connect your computer to add this browser.\n", err)
	} else if browser != "" {
		_, _ = fmt.Fprintln(stdout, browserTrustedMessage)
	}
	if perr == nil && previous != res.Key && !auth.IsTaskToken(previous) {
		// Only a saved ak_ key is revoked, with itself as the credential.
		revoked := false
		if strings.HasPrefix(previous, "ak_") {
			err := revokeKey(cmd.Context(), version, previous)
			revoked = err == nil || errors.Is(err, client.ErrUnauthorized)
		}
		if revoked {
			_, _ = fmt.Fprintf(stdout, "The key saved here before (%s) no longer works.\n", auth.MaskToken(previous))
		} else {
			_, _ = fmt.Fprintf(stderr, "Note: the key saved here before (%s) is still active. Revoke it in Mosaic if you no longer need it.\n", auth.MaskToken(previous))
		}
	}
	if errors.Is(perr, auth.ErrKeychainUnavailable) {
		// The replaced key could not be read, so it was not revoked.
		_, _ = fmt.Fprintln(stderr, "Note: the key this computer used before could not be read from the keychain, so it may still be active. Remove it in Mosaic settings if you no longer need it.")
	}
	if os.Getenv("ARCHIVIST_TOKEN") != "" {
		_, _ = fmt.Fprintln(stderr, "Warning: ARCHIVIST_TOKEN is set in this environment and takes precedence over the saved key here. Unset it to use the paired key. The background service always uses the saved key.")
	}
	if pairServiceInstalled() {
		_, _ = fmt.Fprintln(stdout, "Run 'archivist connect --install' to restart background connect with the new key.")
	}
	return nil
}

// pairBrowserInvalidMessage answers a --pair value whose browser code
// (after the dot) is malformed.
const pairBrowserInvalidMessage = "the browser code after the pairing code is not valid, so nothing was sent. Copy the command from Mosaic again."

// browserTrustedMessage confirms a browser was added (--pair CODE.FP,
// --trust).
const browserTrustedMessage = "This browser can now use this computer."

// pairBrowsers records a pairing in the trusted browser list (Story 94.7).
func pairBrowsers(account, browser string) error {
	dir, err := connect.DefaultStateDir()
	if err != nil {
		return err
	}
	return connect.PairBrowsers(dir, account, browser)
}

// runTrust adds a browser to the list of browsers that can use this
// computer (Story 94.7): `archivist connect --trust <code>` with the code
// Mosaic shows in that browser. A running connect announces it at its next
// ping. A malformed code exits 2.
func runTrust(cmd *cobra.Command, code string) error {
	stderr := cmd.ErrOrStderr()
	code = strings.TrimSpace(code)
	if !connect.ValidBrowserCode(code) {
		_, _ = fmt.Fprintln(stderr, "archivist connect: this browser code is not valid. Copy the command from Mosaic again.")
		return &ExitError{Code: ExitUsageError}
	}
	dir, err := connect.DefaultStateDir()
	if err == nil {
		_, err = connect.TrustBrowser(dir, code, connect.ViaTrust)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: the browser was not added: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), browserTrustedMessage)
	return nil
}

// browsersLine is the --status count of browsers that can use this computer.
func browsersLine() string {
	dir, err := connect.DefaultStateDir()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	l, err := connect.LoadBrowsers(dir)
	if err != nil {
		return "unreadable (pair again with the command Mosaic shows to rewrite it)"
	}
	return strconv.Itoa(len(l.Browsers))
}

// confirmInBrowser waits for the workspace confirm page (Story 94.3, owner
// D6): it opens the page in this computer's browser, where a browser signed
// in to the code's account confirms with no click, and polls until the key
// comes back, the browser refuses, the pairing expires or the user presses
// Ctrl-C (which declines it). No key exists before the confirmed poll.
func confirmInBrowser(cmd *cobra.Command, c *client.Client, p *client.PendingPairing) (*client.PairingResult, error) {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	decline := func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), pairRevokeTimeout)
		defer cancel()
		_ = c.DeclinePairing(ctx, p)
	}
	email := printableText(p.Email)
	if email == "" {
		decline()
		return nil, pairNotNamed(stderr)
	}
	ctx, stop := pairInterrupt(cmd.Context())
	defer stop()
	_, _ = fmt.Fprintf(stdout, "This computer will connect to %s. Confirm in the browser where you use Mosaic; it opens now.\n", email)
	linkShown := false
	showLink := func() {
		if !linkShown {
			linkShown = true
			_, _ = fmt.Fprintf(stdout, "Open this link in the browser where you use Mosaic: %s\n", p.ConfirmURL)
		}
	}
	opened, err := openPairBrowser(p.ConfirmURL)
	if err != nil {
		showLink()
	}
	expires := time.Duration(p.ExpiresInSeconds) * time.Second
	if expires > pairPendingMax {
		expires = pairPendingMax
	}
	deadline := time.Now().Add(expires)
	every := pairPollEvery(p.PollIntervalSeconds)
	timer := time.NewTimer(every)
	defer timer.Stop()
	notInTime := func() error {
		_, _ = fmt.Fprintln(stderr, "archivist connect: this computer was not confirmed in time. Nothing was saved. Get a new code in Mosaic and run the command again.")
		return &ExitError{Code: ExitGenericError}
	}
	for {
		select {
		case <-ctx.Done():
			decline()
			_, _ = fmt.Fprintln(stderr, "Pairing cancelled. Nothing was saved.")
			return nil, &ExitError{Code: ExitGenericError}
		case err := <-opened:
			// The opener finished: a failure means the page may not be open.
			opened = nil
			if err != nil {
				showLink()
			}
			continue
		case <-timer.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pairPollTimeout)
		r, err := c.PollPairing(pctx, p)
		cancel()
		var apiErr *client.APIError
		switch {
		case ctx.Err() != nil:
			// Interrupted while the key came back: it is never saved, so
			// remove it before the select above declines.
			if err == nil && r.State == client.PollConfirmed && r.Key != nil {
				_ = revokeKey(cmd.Context(), c.Version, r.Key.Key)
			}
			continue
		case errors.Is(err, client.ErrPairingCodeInvalid):
			return nil, notInTime()
		case errors.As(err, &apiErr) && apiErr.Code == "CLERK_ERROR":
			_, _ = fmt.Fprintln(stderr, "archivist connect: Mosaic could not create the key. Nothing was saved. Get a new code in Mosaic and try again.")
			return nil, &ExitError{Code: ExitServerError}
		case err == nil && r.State == client.PollConfirmed:
			return r.Key, nil
		case err == nil && r.State == client.PollRefused:
			_, _ = fmt.Fprintln(stderr, "archivist connect: "+pairRefusedMessage(r.Reason))
			return nil, &ExitError{Code: ExitGenericError}
		case err == nil && r.State == client.PollExpired:
			return nil, notInTime()
		}
		// Pending, or a passing failure: ask again until the deadline.
		if time.Now().After(deadline) {
			return nil, notInTime()
		}
		timer.Reset(every)
	}
}

// pairRefusedMessage is the plain reason a browser refused a pairing.
func pairRefusedMessage(reason string) string {
	switch reason {
	case "other_account":
		return "this computer was not connected. The code came from a different Mosaic account than the one signed in to your browser. Nothing was saved."
	case "signed_out":
		return "this computer was not connected because your browser is not signed in to Mosaic. Sign in, get a new code and run the command again. Nothing was saved."
	}
	return "this computer was not connected. Nothing was saved. Get a new code in Mosaic and try again."
}

// confirmAccount asks on the terminal whether to connect to email. Enter,
// y or yes (any case) is a yes; any other answer, end of input, Ctrl-C,
// SIGTERM or SIGHUP is a no.
func confirmAccount(ctx context.Context, tty io.ReadWriter, email string) bool {
	ctx, stop := pairInterrupt(ctx)
	defer stop()
	// Typed ahead input (an Enter while the redeem ran) must not answer.
	flushConfirmInput(tty)
	_, _ = fmt.Fprintf(tty, "This computer will connect to %s. Continue? [Y/n] ", email)
	answer := make(chan string, 1)
	go func() {
		line, err := bufio.NewReader(tty).ReadString('\n')
		if err != nil {
			close(answer)
			return
		}
		answer <- line
	}()
	select {
	case <-ctx.Done():
		_, _ = fmt.Fprintln(tty)
		return false
	case line, ok := <-answer:
		if !ok {
			_, _ = fmt.Fprintln(tty)
			return false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return true
		}
		return false
	}
}

// revokeKey revokes key with itself as the credential (DELETE
// /account/cli-tokens/current), bounded, and even after an interrupt.
func revokeKey(ctx context.Context, version, key string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pairRevokeTimeout)
	defer cancel()
	return client.New(key, version).RevokeCurrentKey(ctx)
}

// accountLine is the --status account: the saved key's account email, or
// why it is not known.
func accountLine(ctx context.Context, version, token string) string {
	ctx, cancel := context.WithTimeout(ctx, accountStatusTimeout)
	defer cancel()
	res, err := client.New(token, version).GetCLITokens(ctx)
	switch {
	case errors.Is(err, client.ErrUnauthorized):
		return "this key is no longer accepted (run 'archivist connect --pair CODE')"
	case err != nil || printableText(res.UserEmail) == "":
		return "unknown (could not reach Mosaic)"
	}
	return printableText(res.UserEmail)
}

// printableText keeps printable characters only, so text from the server
// cannot move the cursor or change the terminal.
func printableText(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return -1
		}
		return r
	}, s))
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
	m, err := service.New(serviceOptions(home, args))
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

// serviceLingerDir is systemd-logind's linger directory ("" = the system's);
// tests replace it.
var serviceLingerDir = ""

// serviceOptions is this user's service manager configuration.
func serviceOptions(home string, args []string) service.Options {
	opts := service.Options{Home: home, Run: serviceRunner, UID: os.Getuid(), Env: service.CaptureEnv(os.Environ()), Args: args,
		LingerDir: serviceLingerDir}
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
	return opts
}

// serviceInstalled reports whether this user's background service
// definition exists (no manager command runs).
func serviceInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	m, err := service.New(serviceOptions(home, nil))
	if err != nil {
		return false
	}
	_, err = os.Stat(m.Path())
	return err == nil
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

// startQuestion is the installers' one question (Story 94.6, owner D1).
const startQuestion = "Start Archivist when your computer starts? (recommended) [Y/n] "

// startDeclined is what a "no" to startQuestion prints.
const startDeclined = "Archivist will not start by itself. Run 'archivist connect' when you want to use your agents from Mosaic."

// askStartAtLogin asks startQuestion the way pairing asks (Story 94.3): only
// on a terminal that opens while stdout or stderr shows it, with input typed
// ahead discarded. Enter, y or yes (or the end of input) is yes, n or no is
// no, anything else asks again (three times, then yes). With no terminal
// the answer is yes, said in one line. Ctrl-C, SIGTERM or SIGHUP aborts.
func askStartAtLogin(cmd *cobra.Command) (bool, error) {
	stdout := cmd.OutOrStdout()
	tty, err := openConfirmTerminal()
	if err != nil || !pairOutputVisible() {
		if err == nil {
			_ = tty.Close()
		}
		_, _ = fmt.Fprintln(stdout, "Starting Archivist when your computer starts (the default).")
		return true, nil
	}
	defer func() { _ = tty.Close() }()
	ctx, stop := pairInterrupt(cmd.Context())
	defer stop()
	flushConfirmInput(tty)
	lines := make(chan string)
	ended := make(chan struct{})
	go func() {
		r := bufio.NewReader(tty)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				close(ended)
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	for range 3 {
		_, _ = fmt.Fprint(tty, startQuestion)
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(tty)
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Install stopped. Run 'archivist connect --install' to set up background connect later.")
			return false, &ExitError{Code: ExitGenericError}
		case <-ended:
			_, _ = fmt.Fprintln(tty)
			return true, nil
		case line := <-lines:
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "", "y", "yes":
				return true, nil
			case "n", "no":
				return false, nil
			}
		}
	}
	return true, nil
}

// runServiceInstall installs and starts the background service, running
// `connect --service` with --max-permission when one was given. Without
// the flag, an installed service's own --max-permission is kept, so an
// update (the install script reinstalls on every update) never changes the
// ceiling; --max-permission auto_edits returns to the default. With ask
// (the installers' first install, Story 94.6) and no service installed yet,
// it first asks whether to start Archivist when the computer starts; no
// installs nothing and exits 0. A key saved in the credentials file moves
// into the keychain where one is usable.
func runServiceInstall(cmd *cobra.Command, maxPermission string, ask bool) error {
	stderr, stdout := cmd.ErrOrStderr(), cmd.OutOrStdout()
	if !connect.Supported() {
		_, _ = fmt.Fprintln(stderr, "archivist connect: Background connect is not available on this system.")
		return &ExitError{Code: ExitGenericError}
	}
	// The service never sees ARCHIVIST_TOKEN: it needs a saved owner key.
	token, err := auth.SavedToken()
	if errors.Is(err, auth.ErrKeychainUnavailable) {
		_, _ = fmt.Fprintf(stderr, "archivist connect --install: the saved key is in the keychain, which could not be read (%v). Unlock it (log in) and try again.\n", err)
		return &ExitError{Code: ExitAuthError}
	}
	if err != nil || !strings.HasPrefix(token, "ak_") {
		_, _ = fmt.Fprintln(stderr, "archivist connect --install needs a saved ak_ key. Run 'archivist connect --pair CODE' with a code from Mosaic (or 'archivist auth login --token ak_...') first.")
		return &ExitError{Code: ExitAuthError}
	}
	if err := checkServiceBinary(stderr); err != nil {
		return err
	}
	if ask && !pairServiceInstalled() {
		start, err := askStartAtLogin(cmd)
		if err != nil {
			return err
		}
		if !start {
			_, _ = fmt.Fprintln(stdout, startDeclined)
			return nil
		}
	}
	if moved, err := auth.MigrateToKeychain(); moved {
		_, _ = fmt.Fprintln(stdout, "The saved key moved to the keychain.")
	} else if err != nil {
		_, _ = fmt.Fprintf(stderr, "Note: the saved key stays in its file: %v\n", err)
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

// runServiceUninstall stops and removes the background service, then
// cleans this computer up (Story 94.6): lingering is turned off when
// Archivist turned it on (Linux), a saved ak_ key is revoked (DELETE
// /account/cli-tokens/current with itself) and removed from the keychain
// and the credentials file, and ~/.archivist/connect and the session work
// folders are removed. An mc_pat_ login is left alone. The files go even
// when the revoke fails, which exits 1 and says to remove the key in Mosaic.
func runServiceUninstall(cmd *cobra.Command, version string) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	m, home, err := newServiceManager(cmd, nil)
	if err != nil {
		return err
	}
	unitInstalled := fileExists(m.Path())
	removed, err := m.Uninstall(cmd.Context())
	if err != nil && !removed {
		_, _ = fmt.Fprintf(stderr, "archivist connect: could not remove the background service: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	if !removed {
		_, _ = fmt.Fprintln(stdout, "Background connect is not installed.")
	} else {
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "Warning: stopping the service reported: %v\n", err)
		}
		_, _ = fmt.Fprintf(stdout, "Background connect removed (%s).\n", m.Path())
	}
	if lr, ok := m.(service.LingerReleaser); ok {
		released, err := lr.ReleaseLinger(cmd.Context(), unitInstalled)
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(stderr, "Note: lingering stays on: %v\n", err)
		case released == service.LingerTurnedOff:
			_, _ = fmt.Fprintln(stdout, "Lingering turned off (Archivist had turned it on).")
		case released == service.LingerUnknownOwner:
			_, _ = fmt.Fprintln(stdout, "Lingering stays on. Run 'loginctl disable-linger' if nothing else needs it.")
		}
	}
	revokeFailed := !removeSavedKey(cmd, version)
	removeConnectFiles(stdout, stderr, home)
	if revokeFailed {
		_, _ = fmt.Fprintln(stderr, "Remove this computer's key in Mosaic settings.")
		return &ExitError{Code: ExitGenericError}
	}
	return nil
}

// removeSavedKey revokes and removes a saved ak_ key for --uninstall; false
// when the key could not be revoked (it is removed locally anyway). An
// mc_pat_ login stays saved and is not revoked.
func removeSavedKey(cmd *cobra.Command, version string) bool {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	token, err := auth.SavedToken()
	switch {
	case errors.Is(err, auth.ErrNoToken):
		return true
	case err == nil && !strings.HasPrefix(token, "ak_"):
		return true
	}
	revoked := false
	if err == nil {
		// A paused computer's relay state goes first, while the key works.
		clearRelayPause(cmd.Context(), stderr, version, token)
		rerr := revokeKey(cmd.Context(), version, token)
		revoked = rerr == nil || errors.Is(rerr, client.ErrUnauthorized)
		if revoked {
			_, _ = fmt.Fprintf(stdout, "This computer's key (%s) no longer works.\n", auth.MaskToken(token))
		} else {
			_, _ = fmt.Fprintf(stderr, "Note: this computer's key (%s) could not be revoked: %v\n", auth.MaskToken(token), rerr)
		}
	} else {
		_, _ = fmt.Fprintf(stderr, "Note: the saved key could not be read, so it was not revoked: %v\n", err)
	}
	if existed, where, derr := auth.DeleteCredentials(); derr != nil {
		_, _ = fmt.Fprintf(stderr, "Note: the saved key could not be removed: %v\n", derr)
	} else if existed {
		_, _ = fmt.Fprintf(stdout, "Removed the saved key from %s.\n", where)
	}
	return revoked
}

// removeConnectFiles removes ~/.archivist/connect (session records, logs,
// task tokens, Codex homes, the pause and linger markers) and the session
// work folders this user left in the temporary directory.
func removeConnectFiles(stdout, stderr io.Writer, home string) {
	dir := filepath.Join(home, ".archivist", "connect")
	if _, err := os.Lstat(dir); err == nil {
		if err := fsutil.RemoveAll(dir); err != nil {
			_, _ = fmt.Fprintf(stderr, "Note: could not remove %s: %v\n", dir, err)
		} else {
			_, _ = fmt.Fprintf(stdout, "Removed %s.\n", dir)
		}
	}
	n := 0
	for _, w := range workFolders(os.TempDir()) {
		if err := fsutil.RemoveAll(w); err != nil {
			_, _ = fmt.Fprintf(stderr, "Note: could not remove %s: %v\n", w, err)
			continue
		}
		n++
	}
	if n > 0 {
		_, _ = fmt.Fprintf(stdout, "Removed %d session work folder(s).\n", n)
	}
}

// workFolders lists this user's session working directories in tmp
// (archivist-connect-*): directories, never links, owned by this user.
func workFolders(tmp string) []string {
	matches, _ := filepath.Glob(filepath.Join(tmp, "archivist-connect-*"))
	var out []string
	for _, p := range matches {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || !ownedByUser(info) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runServiceStatus reports the service, the saved key and its account
// (Story 94.3; the account lookup never changes the exit): exit 0 when the
// service runs (systemd: is-active; launchd: loaded and its daemon alive)
// and a saved ak_ key exists, else 1.
func runServiceStatus(cmd *cobra.Command, version string) error {
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
	if stateDir, err := connect.DefaultStateDir(); err == nil && pausedHere(stateDir) {
		_, _ = fmt.Fprintln(out, "Paused:   yes, on this computer (resume with 'archivist connect --resume' or in Mosaic)")
	}
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
		_, _ = fmt.Fprintf(out, "Account:  %s\n", accountLine(cmd.Context(), version, token))
	} else if errors.Is(terr, auth.ErrKeychainUnavailable) {
		_, _ = fmt.Fprintf(out, "Key:      saved in the keychain, which could not be read (%v); unlock it (log in) and run this again\n", terr)
	} else {
		_, _ = fmt.Fprintln(out, "Key:      none (run 'archivist connect --pair CODE' with a code from Mosaic)")
	}
	_, _ = fmt.Fprintf(out, "Browsers: %s\n", browsersLine())
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
	if err := waitForKeyring(cmd.Context(), log); err != nil {
		return serviceExit(log, nil)
	}
	removePID, err := service.WritePID(home, os.Getpid())
	if err != nil {
		log.Printf("warning: could not write %s: %v", service.PIDPath(home), err)
	} else {
		defer removePID()
	}
	return serviceExit(log, runConnect(cmd, version, false, f))
}

// keyringRetry is how often the service asks a keychain that is not
// readable yet (a var so tests can shorten it).
var keyringRetry = 10 * time.Second

// waitForKeyring holds the service start while the saved key is in a
// keychain that cannot be read yet (Story 94.6: on Linux a desktop keyring
// stays locked until the user logs in, and a lingering service starts at
// boot). No file is written meanwhile. It returns nil once the key reads
// (or another outcome takes over), ctx's error when the service is stopped.
func waitForKeyring(ctx context.Context, log *connect.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	said := false
	for {
		_, err := auth.SavedToken()
		if !errors.Is(err, auth.ErrKeychainUnavailable) {
			if said {
				log.Printf("the keyring is available")
			}
			return nil
		}
		if !said {
			said = true
			log.Printf("waiting for the keyring (it opens when you log in): %v", err)
		}
		t := time.NewTimer(keyringRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
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
