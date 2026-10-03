package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/connect"
	"github.com/spf13/cobra"
)

const connectLong = `Connect your own Claude Code or Codex to the Mosaic workspace (preview).

archivist connect runs in the foreground and keeps one outbound connection to
the Mosaic agent relay; nothing listens on your machine. When you start a
"My Claude Code" or "My Codex" session in the workspace, it runs claude or
codex on this machine with your own subscription login, in a fresh temporary
directory, with Mosaic search and read tools (archivist mcp serve under a
short-lived, session-scoped task token). Ctrl-C stops every session and
revokes its tokens.

Tool calls that change anything ask for your approval in the workspace.
Claude Code: "Allow for session" applies to every later call of that tool
(any input) in that session without asking again, and "Deny for session"
refuses it the same way; restarting archivist connect clears these rules.
Codex: "Allow for session" is Codex's accept for session (the identical
command or file again runs without asking), and "Deny for session" is
Codex's cancel (deny and end the turn).

Codex sessions run with a private Codex home per session that only links
your ChatGPT login (auth.json); your Codex config, AGENTS.md, rules, hooks,
plugins and MCP servers are not loaded and never written.

Requires an ak_ API key (archivist auth login) on a Pro account, and Claude
Code logged in with a claude.ai subscription or Codex logged in with
ChatGPT. API key logins are refused.

  archivist connect --check     report what was detected, connect nothing
  archivist connect             connect and serve sessions until Ctrl-C

Exit codes: 0 ok; 1 refused or stopped (feature off, superseded, too old);
2 bad flag or relay URL; 3 no usable harness (Claude Code not found, or no
usable Claude Code or Codex); 4 credential or Claude login problem.`

var (
	connectModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$`)
	connectEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
)

func newConnectCmd(version string) *cobra.Command {
	var check bool
	var model, effort, codexModel, codexEffort string
	c := &cobra.Command{
		Use:   "connect",
		Short: "Drive your own Claude Code or Codex from the Mosaic workspace (preview)",
		Long:  connectLong,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1,2,3,4",
			// A daemon that spawns harnesses is never an MCP tool.
			"mcp:hidden": "true",
		},
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if model != "" && !connectModelRe.MatchString(model) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: --claude-model %q is not a model name\n", model)
				return &ExitError{Code: ExitUsageError}
			}
			if effort != "" && !connectEfforts[effort] {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: --claude-effort must be low, medium, high, xhigh or max\n")
				return &ExitError{Code: ExitUsageError}
			}
			if codexModel != "" && !connectModelRe.MatchString(codexModel) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: --codex-model %q is not a model name\n", codexModel)
				return &ExitError{Code: ExitUsageError}
			}
			if codexEffort != "" && !slices.Contains(connect.CodexEfforts, codexEffort) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: --codex-effort must be one of %s\n", strings.Join(connect.CodexEfforts, ", "))
				return &ExitError{Code: ExitUsageError}
			}
			return runConnect(cmd, version, check, connectFlags{model: model, effort: effort,
				codexModel: codexModel, codexEffort: codexEffort})
		},
	}
	c.Flags().BoolVar(&check, "check", false, "Report detected harnesses and logins, then exit")
	c.Flags().StringVar(&model, "claude-model", "", "Claude model for sessions on this machine (local setting)")
	c.Flags().StringVar(&effort, "claude-effort", "", "Claude effort for sessions on this machine: low, medium, high, xhigh, max")
	c.Flags().StringVar(&codexModel, "codex-model", "", "Codex model for sessions on this machine (local setting; default: Codex's default model)")
	c.Flags().StringVar(&codexEffort, "codex-effort", "", "Codex reasoning effort for sessions on this machine: "+strings.Join(connect.CodexEfforts, ", "))
	return c
}

// connectFlags are the local harness settings (never from the relay).
type connectFlags struct {
	model, effort           string
	codexModel, codexEffort string
}

func runConnect(cmd *cobra.Command, version string, check bool, f connectFlags) error {
	stderr := cmd.ErrOrStderr()
	if !connect.Supported() {
		_, _ = fmt.Fprintln(stderr, "archivist connect: "+connect.ErrUnsupportedPlatform.Error())
		return &ExitError{Code: ExitGenericError}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	// The relay URL is checked before anything else, --check included.
	relayURL, err := relayURLFromEnv(os.Getenv("ARCHIVIST_RELAY_URL"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitUsageError}
	}
	childEnv, err := connect.BuildChildEnv(os.Environ(), nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	codexHome := connect.OwnerCodexHome(os.Environ(), home)
	detect := func(ctx context.Context) connect.Detection {
		return connect.DetectWith(ctx, connect.DetectOptions{LookPath: exec.LookPath, Run: connect.ExecRunner,
			Env: childEnv, Dir: home, CodexHome: codexHome})
	}

	if check {
		det := detect(cmd.Context())
		printDetection(cmd.OutOrStdout(), det)
		return connectExit(det)
	}

	// The owner's ak_ key mints tickets and task tokens; refuse anything else
	// before any network call.
	tokenFlag, _ := cmd.Root().PersistentFlags().GetString("token")
	token, _, err := auth.Resolve(tokenFlag)
	if err != nil || !strings.HasPrefix(token, "ak_") {
		msg := "archivist connect needs an ak_ API key. Run 'archivist auth login --token ak_...' or set ARCHIVIST_TOKEN."
		if err == nil {
			msg = "archivist connect needs an ak_ API key; mc_pat_ and task (mst_) credentials cannot mint relay tickets."
		}
		_, _ = fmt.Fprintln(stderr, msg)
		return &ExitError{Code: ExitAuthError}
	}

	det := detect(cmd.Context())
	if !det.Claude.Usable() && !det.Codex.Usable() {
		_, _ = fmt.Fprintln(stderr, "archivist connect: "+det.Claude.Problem)
		if det.Codex.Path != "" {
			_, _ = fmt.Fprintln(stderr, "archivist connect: "+det.Codex.Problem)
		}
		return connectExit(det)
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: locate this binary for the MCP server: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	stateDir, err := connect.DefaultStateDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	api := client.New(token, version)
	api.SetStderr(io.Discard)
	log := connect.NewLogger(stderr)
	baseURL := os.Getenv("ARCHIVIST_BASE_URL")
	cfg := connect.Config{
		API:        api,
		RelayURL:   relayURL,
		StateDir:   stateDir,
		Log:        log,
		Detect:     detect,
		AppVersion: version,
	}
	// Only a harness usable now is driven; the other's sessions are refused.
	if det.Claude.Usable() {
		cfg.Claude = connect.ClaudeConfig{Bin: det.Claude.Path, Model: f.model, Effort: f.effort,
			SettingSources: connect.DefaultSettingSources, Executable: exe, BaseURL: baseURL}
	}
	if det.Codex.Usable() {
		cfg.Codex = connect.CodexConfig{Bin: det.Codex.Path, Version: det.Codex.Version, Model: f.codexModel,
			Effort: f.codexEffort, OwnerHome: det.Codex.Home, Executable: exe, BaseURL: baseURL}
	}
	d, err := connect.New(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var harnesses []string
	if det.Claude.Usable() {
		harnesses = append(harnesses, fmt.Sprintf("Claude Code %s at %s (%s login, %s)", det.Claude.Version, det.Claude.Path,
			det.Claude.Auth.AuthMethod, det.Claude.Auth.SubscriptionType))
	}
	if det.Codex.Usable() {
		harnesses = append(harnesses, fmt.Sprintf("Codex %s at %s (login linked from %s)", det.Codex.Version, det.Codex.Path, det.Codex.Home))
	}
	log.Printf("archivist connect %s: %s; key fp:%s; relay %s. Ctrl-C stops.",
		version, strings.Join(harnesses, "; "), auth.Fingerprint(token), relayURL)
	err = d.Run(ctx)
	if left := d.LiveTokens(); len(left) > 0 {
		log.Printf("warning: %d task token(s) could not be revoked; they expire within 15 minutes", len(left))
	}
	var fatal *connect.FatalError
	switch {
	case err == nil:
		log.Printf("stopped")
		return nil
	case errors.Is(err, connect.ErrSuperseded):
		_, _ = fmt.Fprintln(stderr, "archivist connect: another archivist connect took over; this one stopped.")
		return &ExitError{Code: ExitGenericError}
	case errors.As(err, &fatal):
		_, _ = fmt.Fprintln(stderr, "archivist connect: "+fatal.Message)
		if fatal.Auth {
			return &ExitError{Code: ExitAuthError}
		}
		return &ExitError{Code: ExitGenericError}
	default:
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
}

// relayURLFromEnv returns the relay base URL. Tickets travel in the
// handshake, so only wss/https are accepted, except ws/http to a loopback
// host (a local relay).
func relayURLFromEnv(raw string) (string, error) {
	if raw == "" {
		return connect.DefaultRelayURL, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("ARCHIVIST_RELAY_URL %q is not a URL with a host", raw)
	}
	// Routes are appended to the URL: a query, fragment or userinfo would
	// corrupt them (and userinfo would travel with the ticket).
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil {
		return "", fmt.Errorf("ARCHIVIST_RELAY_URL must not carry a query, fragment or user info")
	}
	switch u.Scheme {
	case "wss", "https":
		return raw, nil
	case "ws", "http":
		host := u.Hostname()
		if host == "localhost" {
			return raw, nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return raw, nil
		}
		return "", fmt.Errorf("ARCHIVIST_RELAY_URL must use wss:// (cleartext %s:// is allowed only for a loopback relay)", u.Scheme)
	}
	return "", fmt.Errorf("ARCHIVIST_RELAY_URL scheme %q is not wss", u.Scheme)
}

// connectExit is the --check and startup exit code: 0 when any harness is
// usable. With no usable harness it is Claude Code's typed code when Codex
// is not installed (the 78.16 behaviour), else 3 (no usable harness).
func connectExit(det connect.Detection) error {
	if det.Claude.Usable() || det.Codex.Usable() {
		return nil
	}
	if det.Codex.Path == "" {
		return claudeExit(det.Claude)
	}
	return &ExitError{Code: ExitNotFound}
}

// claudeExit maps a detection result to the typed exit code.
func claudeExit(c connect.ClaudeInfo) error {
	switch c.ProblemCode {
	case "":
		return nil
	case "missing":
		return &ExitError{Code: ExitNotFound}
	case "auth":
		return &ExitError{Code: ExitAuthError}
	default:
		return &ExitError{Code: ExitGenericError}
	}
}

func printDetection(w io.Writer, det connect.Detection) {
	c := det.Claude
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("claude\n")
	if c.Path == "" {
		p("  path:     not found\n")
	} else {
		p("  path:     %s\n", c.Path)
		p("  version:  %s\n", orDash(c.Version))
		verdict := "ok"
		if !c.VersionOK {
			verdict = "below floor"
		}
		p("  floor:    %s (%s)\n", connect.ClaudeFloor, verdict)
		if c.Auth != nil {
			p("  loggedIn: %v\n", c.Auth.LoggedIn)
			p("  authMethod: %s\n", orDash(c.Auth.AuthMethod))
			p("  subscriptionType: %s\n", orDash(c.Auth.SubscriptionType))
		}
	}
	if c.Usable() {
		p("  status:   usable\n")
	} else {
		p("  status:   not usable: %s\n", c.Problem)
	}
	x := det.Codex
	p("codex\n")
	if x.Path == "" {
		p("  path:     not found\n")
		return
	}
	p("  path:     %s\n", x.Path)
	p("  version:  %s\n", orDash(x.Version))
	verdict := "ok"
	if !x.VersionOK {
		verdict = "below floor"
	}
	p("  floor:    %s (%s)\n", connect.CodexFloor, verdict)
	p("  loggedIn: %v\n", x.LoggedIn)
	if x.Login != "" {
		p("  login:    %s\n", x.Login)
	}
	if x.Home != "" {
		present := "auth.json present"
		if !x.AuthPresent {
			present = "no auth.json"
		}
		p("  home:     %s (%s)\n", x.Home, present)
	}
	if x.Usable() {
		p("  status:   usable\n")
	} else {
		p("  status:   not usable: %s\n", x.Problem)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
