package cmd

import (
	"bytes"
	"context"
	"os"
	"runtime"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/doctor"
	"github.com/spf13/cobra"
)

func newDoctorCmd(version, commit, date string) *cobra.Command {
	var formatFlag string
	var quietFlag bool
	var noNetworkFlag bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run health checks on credentials, connectivity, version, and quota",
		Long: `Run a suite of health checks that diagnose auth, connectivity, version, and quota issues.

Checks run in order and independently; a single failure does not abort the rest.

Exit codes:
  0 = all checks passed (WARNs are not failures)
  4 = auth failure (missing credential, invalid token, revoked, no CLI scope)
  5 = server unreachable or version block
  1 = other failure`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,1,4,5",
			"mcp:read-only":       "true",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd, version, commit, date, formatFlag, quietFlag, noNetworkFlag)
		},
	}

	cmd.Flags().StringVar(&formatFlag, "format", "", "Output format: json")
	cmd.Flags().BoolVar(&quietFlag, "quiet", false, "Suppress passing checks; print only FAILs and WARNs")
	cmd.Flags().BoolVar(&noNetworkFlag, "no-network", false, "Skip server-side checks (5-8)")

	return cmd
}

func runDoctor(cmd *cobra.Command, version, commit, date, formatFlag string, quiet, noNetwork bool) error {
	out := cmd.OutOrStdout()

	// Auto-JSON when stdout is not a TTY (only when writing to the real os.Stdout)
	if formatFlag == "" && out == os.Stdout {
		fi, err := os.Stdout.Stat()
		if err == nil && (fi.Mode()&os.ModeCharDevice) == 0 {
			formatFlag = "json"
		}
	}

	// Resolve token via the shared auth ladder (flag > env > file) without
	// failing — doctor reports a missing credential as check 3 FAIL and a
	// malformed one as check 4 FAIL. Resolve returns the found token and its
	// source even when the only problem is the format, so the checks can
	// still report on what was found.
	tokenFlag, _ := cmd.Root().PersistentFlags().GetString("token")
	token, source, _ := auth.Resolve(tokenFlag)
	credsPath, _ := auth.CredentialsPath()

	baseURL := os.Getenv("ARCHIVIST_BASE_URL")

	cfg := &doctor.RunConfig{
		Token:           token,
		TokenSource:     source.String(),
		CredentialsPath: credsPath,
		Version:         version,
		Commit:          commit,
		Date:            date,
		NoNetwork:       noNetwork,
		BaseURL:         baseURL,
	}

	ctx := context.Background()

	// Run all checks
	var results []doctor.CheckResult
	var probe *doctor.ServerProbeResult
	var userInfo *doctor.CLITokensInfo

	// Check 1: Binary version (always runs)
	results = append(results, doctor.Check1Binary(cfg))

	// Check 2: Skill version (always runs)
	results = append(results, doctor.Check2Skill(cfg))

	// Check 3: Credentials present
	check3 := doctor.Check3Credentials(cfg)
	results = append(results, check3)

	// Check 4: Token format (only if credentials present)
	if check3.Status == doctor.StatusFail {
		results = append(results, doctor.CheckResult{Name: "Token", Status: doctor.StatusSkip, Detail: "skipped: no credential"})
	} else {
		results = append(results, doctor.Check4Token(cfg))
	}

	credsFailed := check3.Status == doctor.StatusFail

	// Checks 5-8: Network checks (skip if --no-network or creds failed)
	if noNetwork {
		results = append(results, doctor.CheckResult{Name: "Server", Status: doctor.StatusSkip, Detail: "skipped: --no-network"})
		results = append(results, doctor.CheckResult{Name: "MinVersion", Status: doctor.StatusSkip, Detail: "skipped: --no-network"})
		results = append(results, doctor.CheckResult{Name: "User", Status: doctor.StatusSkip, Detail: "skipped: --no-network"})
		results = append(results, doctor.CheckResult{Name: "Quota", Status: doctor.StatusSkip, Detail: "skipped: --no-network"})
	} else if credsFailed {
		results = append(results, doctor.CheckResult{Name: "Server", Status: doctor.StatusSkip, Detail: "skipped: no credential"})
		results = append(results, doctor.CheckResult{Name: "MinVersion", Status: doctor.StatusSkip, Detail: "skipped: no credential"})
		results = append(results, doctor.CheckResult{Name: "User", Status: doctor.StatusSkip, Detail: "skipped: no credential"})
		results = append(results, doctor.CheckResult{Name: "Quota", Status: doctor.StatusSkip, Detail: "skipped: no credential"})
	} else {
		// Check 5: Server reachable (unauthenticated /health probe)
		check5, serverProbe := doctor.Check5Server(ctx, cfg)
		probe = serverProbe
		results = append(results, check5)

		// Check 6: Min version (depends on server probe)
		results = append(results, doctor.Check6MinVersion(cfg, probe))

		// Check 7: User / tier / CLI scope (authenticated /account/cli-tokens call)
		check7, ui := doctor.Check7User(ctx, cfg, probe)
		userInfo = ui
		results = append(results, check7)

		// Check 8: Quota (from probe or user check headers)
		results = append(results, doctor.Check8Quota(cfg, userInfo, probe))
	}

	exitCode := doctor.ResolveExitCode(results)

	if formatFlag == "json" {
		report := doctor.BuildJSONReport(results, cfg, userInfo, probe)
		report.Binary.OS = runtime.GOOS
		report.Binary.Arch = runtime.GOARCH
		var buf bytes.Buffer
		if err := doctor.PrintJSON(&buf, report); err != nil {
			return &ExitError{Code: ExitGenericError}
		}
		_, _ = out.Write(buf.Bytes())
	} else {
		doctor.PrintHuman(out, results, noNetwork, quiet)
	}

	if exitCode != 0 {
		return &ExitError{Code: exitCode}
	}
	return nil
}
