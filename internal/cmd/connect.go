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
	"strings"
	"syscall"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/connect"
	"github.com/spf13/cobra"
)

const connectLong = `Connect your own Claude Code to the Mosaic workspace (preview).

archivist connect runs in the foreground and keeps one outbound connection to
the Mosaic agent relay; nothing listens on your machine. When you start a
"My Claude Code" session in the workspace, it runs claude on this machine
with your Claude subscription login, in a fresh temporary directory, with
Mosaic search and read tools (archivist mcp serve under a short-lived,
session-scoped task token). Tool calls that change anything ask for your
approval in the workspace. "Allow for session" applies to every later call of
that tool (any input) in that session without asking again. Ctrl-C stops
every session and revokes its tokens.

Requires an ak_ API key (archivist auth login) on a Pro account and Claude
Code logged in with a claude.ai subscription. API key logins are refused.

  archivist connect --check     report what was detected, connect nothing
  archivist connect             connect and serve sessions until Ctrl-C

Exit codes: 0 ok; 1 refused or stopped (feature off, superseded, too old);
2 bad flag; 3 Claude Code not found; 4 credential or Claude login problem.`

var (
	connectModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:\[\]-]{0,127}$`)
	connectEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
)

func newConnectCmd(version string) *cobra.Command {
	var check bool
	var model, effort string
	c := &cobra.Command{
		Use:   "connect",
		Short: "Drive your own Claude Code from the Mosaic workspace (preview)",
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
			return runConnect(cmd, version, check, model, effort)
		},
	}
	c.Flags().BoolVar(&check, "check", false, "Report detected harnesses and logins, then exit")
	c.Flags().StringVar(&model, "claude-model", "", "Claude model for sessions on this machine (local setting)")
	c.Flags().StringVar(&effort, "claude-effort", "", "Claude effort for sessions on this machine: low, medium, high, xhigh, max")
	return c
}

func runConnect(cmd *cobra.Command, version string, check bool, model, effort string) error {
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
	childEnv, err := connect.BuildChildEnv(os.Environ(), nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	detect := func(ctx context.Context) connect.Detection {
		return connect.Detect(ctx, exec.LookPath, connect.ExecRunner, childEnv, home)
	}

	if check {
		det := detect(cmd.Context())
		printDetection(cmd.OutOrStdout(), det)
		return claudeExit(det.Claude)
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
	if !det.Claude.Usable() {
		_, _ = fmt.Fprintln(stderr, "archivist connect: "+det.Claude.Problem)
		return claudeExit(det.Claude)
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
	relayURL, err := relayURLFromEnv(os.Getenv("ARCHIVIST_RELAY_URL"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitUsageError}
	}
	api := client.New(token, version)
	api.SetStderr(io.Discard)
	log := connect.NewLogger(stderr)
	d, err := connect.New(connect.Config{
		API:      api,
		RelayURL: relayURL,
		StateDir: stateDir,
		Claude: connect.ClaudeConfig{Bin: det.Claude.Path, Model: model, Effort: effort,
			SettingSources: connect.DefaultSettingSources, Executable: exe, BaseURL: os.Getenv("ARCHIVIST_BASE_URL")},
		Log:    log,
		Detect: detect,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("archivist connect %s: Claude Code %s at %s (%s login, %s); key fp:%s; relay %s. Ctrl-C stops.",
		version, det.Claude.Version, det.Claude.Path, det.Claude.Auth.AuthMethod, det.Claude.Auth.SubscriptionType,
		auth.Fingerprint(token), relayURL)
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
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("ARCHIVIST_RELAY_URL %q is not a URL", raw)
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
	p("codex\n")
	if det.Codex.Path == "" {
		p("  path:     not found\n")
	} else {
		p("  path:     %s\n", det.Codex.Path)
		p("  version:  %s\n", orDash(det.Codex.Version))
		p("  loggedIn: %v\n", det.Codex.LoggedIn)
	}
	p("  adapter:  not supported yet\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
