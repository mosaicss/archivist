package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/spf13/cobra"
)

const dashboardURL = "https://mosaic-finance.com/account/cli-tokens"

// newAuthCmd returns the `archivist auth` command with real subcommands.
func newAuthCmd(version string) *cobra.Command {
	authCmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage credentials (env var ARCHIVIST_TOKEN; dashboard issues tokens)",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1,2,4",
		},
	}

	authCmd.AddCommand(newAuthLoginCmd())
	authCmd.AddCommand(newAuthStatusCmd(version))
	authCmd.AddCommand(newAuthWhoamiCmd(version))
	authCmd.AddCommand(newAuthLogoutCmd())

	return authCmd
}

func newAuthLoginCmd() *cobra.Command {
	var openBrowser bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Print setup instructions for the Archivist CLI token",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), `To set up your Archivist CLI token:

1. Open: %s
2. Click "Issue Token" and copy the token.
3. Add to your shell profile:
   export ARCHIVIST_TOKEN=mc_pat_...

Or pass per-command with --token mc_pat_...
`, dashboardURL)

			if openBrowser {
				if err := openURL(dashboardURL); err != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not open browser: %v\n", err)
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&openBrowser, "open", false, "Open the token dashboard in your default browser")
	return cmd
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
		Short: "Auth logout is not available — revoke tokens on the dashboard",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0",
			"mcp:hidden":          "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Auth logout is not available. To revoke a token, visit %s\n", dashboardURL)
			return nil
		},
	}
}

func runAuthStatus(cmd *cobra.Command, version string, _ bool) error {
	tokenFlag, _ := cmd.Root().Flags().GetString("token")
	token, err := auth.ResolveToken(tokenFlag)
	if err != nil {
		if errors.Is(err, auth.ErrNoToken) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"No ARCHIVIST_TOKEN set. Run 'archivist auth login' for setup instructions.\n")
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
				"Token invalid or revoked. Re-issue at %s\n", dashboardURL)
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
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Logged in as %s (tier: %s)%s\n", resp.UserEmail, resp.Tier, keyInfo)
	return nil
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
