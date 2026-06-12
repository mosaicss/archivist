package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Tokens are issued from Clerk's UserProfile popup, reached via the user
// avatar on any signed-in page on mosaic-finance.com. The 36.2 cleanup
// removed the standalone /account/cli-tokens page; this URL points to a
// stable signed-in landing so users can open the avatar menu from there.
const dashboardURL = "https://mosaic-finance.com/chat"

// newAuthCmd returns the `archivist auth` command with real subcommands.
func newAuthCmd(version string) *cobra.Command {
	authCmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage credentials (~/.archivist/credentials via auth login; ARCHIVIST_TOKEN and --token override)",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1,2,4,5",
		},
	}

	authCmd.AddCommand(newAuthLoginCmd(version))
	authCmd.AddCommand(newAuthStatusCmd(version))
	authCmd.AddCommand(newAuthWhoamiCmd(version))
	authCmd.AddCommand(newAuthLogoutCmd())

	return authCmd
}

func newAuthLoginCmd(version string) *cobra.Command {
	var openBrowser bool
	var tokenFlag string

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Verify and save your Archivist CLI token to ~/.archivist/credentials",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,4,5",
			// No mcp:read-only — login writes local state (the credentials file).
			// Hidden from MCP: --open launches a browser; token setup is an
			// operator action, not an agent tool (Story 39.7).
			"mcp:hidden": "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if tokenFlag != "" {
				return loginWithToken(cmd, version, tokenFlag)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), `To set up your Archivist CLI token:

1. Open: %s (sign in if prompted).
2. Click your user avatar (top right) → Manage account → API keys.
3. Click "Add new key", give it a name, and copy the token (starts with ak_...).
4. Save it once with:
   archivist auth login --token ak_...

For CI or scripting, the environment variable override still works:
   export ARCHIVIST_TOKEN=ak_...
`, dashboardURL)

			if openBrowser {
				if err := openURL(dashboardURL); err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not open browser: %v\n", err)
				}
			}

			// Interactive terminals get an echo off paste prompt. Agents and
			// pipes (stdin not a TTY) get instructions only and must never
			// block waiting for input. Check stdin's fd, not stdout: chat.go's
			// isTerminal inspects an io.Writer and cannot answer this.
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return nil
			}
			_, _ = fmt.Fprint(cmd.OutOrStdout(), "\nPaste a token to save it now (input hidden, Enter to skip): ")
			raw, err := term.ReadPassword(int(os.Stdin.Fd()))
			_, _ = fmt.Fprintln(cmd.OutOrStdout())
			if err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"Could not read input: %v. Run 'archivist auth login --token ak_...' instead.\n", err)
				return nil
			}
			pasted := strings.TrimSpace(string(raw))
			if pasted == "" {
				return nil
			}
			return loginWithToken(cmd, version, pasted)
		},
	}

	cmd.Flags().BoolVar(&openBrowser, "open", false, "Open the token dashboard in your default browser")
	cmd.Flags().StringVar(&tokenFlag, "token", "", "Token to verify and save (ak_...)")
	return cmd
}

// loginWithToken runs the validate, verify, write sequence. No failure mode
// writes the credentials file.
func loginWithToken(cmd *cobra.Command, version, token string) error {
	if err := auth.ValidateTokenFormat(token); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%v\nNothing was saved.\n", err)
		return &ExitError{Code: ExitAuthError}
	}

	c := client.New(token, version)
	resp, err := c.GetCLITokens(cmd.Context())
	if err != nil {
		if errors.Is(err, client.ErrUnauthorized) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"Token rejected by the server (invalid or revoked). Nothing was saved.\nCreate a new key via your avatar menu (Manage account → API keys) at %s\n",
				dashboardURL)
			return &ExitError{Code: ExitAuthError}
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"Could not verify token against the server: %v\nNothing was saved. If the server is down, export ARCHIVIST_TOKEN=ak_... as a temporary workaround.\n",
			err)
		return &ExitError{Code: ExitServerError}
	}

	path, err := auth.SaveToken(token)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"Token verified but saving failed: %v\nSet ARCHIVIST_TOKEN=ak_... as a workaround.\n", err)
		return &ExitError{Code: ExitGenericError}
	}

	account := resp.UserEmail
	if account == "" {
		account = "unknown"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Token verified and saved.\n  Account: %s (tier: %s)\n  Key:     %s\n  File:    %s\nEvery new terminal can now run archivist without setup.\n",
		account, resp.Tier, auth.MaskToken(token), path)
	return nil
}

func newAuthStatusCmd(version string) *cobra.Command {
	var formatJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Validate the current CLI token and show identity",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,4,5",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuthStatus(cmd, version, formatJSON)
		},
	}

	cmd.Flags().BoolVar(&formatJSON, "format", false, "Output as JSON (pass --format json)")
	cmd.Flags().Lookup("format").NoOptDefVal = "true"
	// Support --format json as a string flag too
	cmd.ResetFlags()
	cmd.Flags().String("format", "", "Output format: json")

	return cmd
}

func newAuthWhoamiCmd(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Alias for auth status",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,4,5",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAuthStatus(cmd, version, false)
		},
	}
	cmd.Flags().String("format", "", "Output format: json")
	return cmd
}

func newAuthLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete the saved credentials file",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1",
			"mcp:hidden":          "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			existed, path, err := auth.DeleteCredentials()
			if err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Could not delete credentials: %v\n", err)
				return &ExitError{Code: ExitGenericError}
			}
			if !existed {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Logged out: no credentials file found, nothing to delete.")
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Logged out: deleted %s\n", path)
			}
			if os.Getenv("ARCHIVIST_TOKEN") != "" {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(),
					"Note: ARCHIVIST_TOKEN is still set in this environment; logout does not unset it.")
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Logout does not revoke the token itself. To revoke it, visit %s (avatar menu → Manage account → API keys).\n",
				dashboardURL)
			return nil
		},
	}
}

func runAuthStatus(cmd *cobra.Command, version string, _ bool) error {
	tokenFlag, _ := cmd.Root().PersistentFlags().GetString("token")
	token, source, err := auth.Resolve(tokenFlag)
	if err != nil {
		if errors.Is(err, auth.ErrNoToken) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"No credential found. Run 'archivist auth login --token ak_...' to save one, or set ARCHIVIST_TOKEN.\n")
		} else {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		}
		return &ExitError{Code: ExitAuthError}
	}

	c := client.New(token, version)
	resp, err := c.GetCLITokens(context.Background())
	if err != nil {
		if errors.Is(err, client.ErrUnauthorized) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"Token invalid or revoked. Create a new key via your avatar menu (Manage account → API keys) at %s\n", dashboardURL)
			return &ExitError{Code: ExitAuthError}
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Server error: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}

	formatFlag, _ := cmd.Flags().GetString("format")
	if formatFlag == "json" {
		var keyID, issued string
		if len(resp.Tokens) > 0 {
			keyID = resp.Tokens[0].KeyID
			issued = resp.Tokens[0].CreatedAt
		}
		out := map[string]string{
			"email":  resp.UserEmail,
			"tier":   resp.Tier,
			"key_id": keyID,
			"issued": issued,
			"source": source.String(),
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	var keyInfo string
	if len(resp.Tokens) > 0 {
		t := resp.Tokens[0]
		keyInfo = fmt.Sprintf(" [key: %s issued %s]", t.KeyID, t.CreatedAt[:10])
	}
	identity := fmt.Sprintf("Logged in (tier: %s)", resp.Tier)
	if resp.UserEmail != "" {
		identity = fmt.Sprintf("Logged in as %s (tier: %s)", resp.UserEmail, resp.Tier)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"%s%s\nCredential source: %s\n",
		identity, keyInfo, describeSource(source))

	// Shadow note: a flag or env credential silently masking a saved file is
	// exactly the two-terminals confusion this command exists to dispel.
	if source == auth.SourceFlag || source == auth.SourceEnv {
		if path, pathErr := auth.CredentialsPath(); pathErr == nil {
			if _, statErr := os.Stat(path); statErr == nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"Note: credentials file present but overridden by %s.\n", source)
			}
		}
	}
	return nil
}

// describeSource renders the ladder rung for human output, naming the
// credentials file path (with the home directory shortened to ~) for the
// file rung. The caller supplies the "Credential source:" label.
func describeSource(source auth.Source) string {
	if source != auth.SourceFile {
		return source.String()
	}
	path, err := auth.CredentialsPath()
	if err != nil {
		return "file"
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		path = strings.Replace(path, home, "~", 1)
	}
	return fmt.Sprintf("file (%s)", path)
}

// openURL opens a URL in the default system browser.
func openURL(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "linux":
		cmd = "xdg-open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		return fmt.Errorf("unsupported OS: %s", runtime.GOOS)
	}
	return exec.Command(cmd, args...).Start()
}
