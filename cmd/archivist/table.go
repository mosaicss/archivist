package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/cascade"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/mosaicss/archivist/internal/cmd/flags"
	"github.com/mosaicss/archivist/internal/defaults"
	"github.com/mosaicss/archivist/internal/output"
	"github.com/mosaicss/archivist/internal/resolver"
	"github.com/mosaicss/archivist/internal/tablespec"
	"github.com/spf13/cobra"
)

// watchBaseURL is the base URL for watch/audit links.
const watchBaseURL = "https://mosaic-finance.com/chat/table/"

// sessionIDPrefix is the required prefix for valid session IDs.
const sessionIDPrefix = "sess_"

// issuerKeyPattern matches values that are already issuer keys (bypass resolution).
var issuerKeyPattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// newTableCmd builds and returns the root `archivist table` Cobra command and
// all subcommands. This is called from cmd/archivist/main.go's NewRootCmd.
func newTableCmd(version string) *cobra.Command {
	var (
		rowFlags    []string
		colFlags    []string
		topN        int
		stream      bool
		async       bool
		watch       bool
		formatFlag  string
	)

	tableCmd := &cobra.Command{
		Use:   "table",
		Short: "Build or run a research table over filings",
		Long: `Run a structured multi-entity research table.

Fluent flag mode (inline):
  archivist table --row "company=aapl_us,filing-type=10-K" \
                  --col "name=Revenue,source=filings,query=annual revenue"

Spec file mode:
  archivist table run ./spec.yaml

Saved session commands:
  archivist table list
  archivist table rerun <session_id>
  archivist table watch <session_id>

Defaults:
  When a row pins an issuer (issuer_key) without date_from / date_to,
  archivist fills "Last 6 months" automatically. Hard cap at 2 years
  for issuer-locked rows without a filing-type filter.
  Run 'archivist explain defaults' for details.`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,3,4,5,6,7,8",
			"mcp:read-only":       "false",
		},
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runTableFluent(cobraCmd, version, rowFlags, colFlags, topN, stream, async, watch, formatFlag)
		},
	}

	tableCmd.Flags().StringArrayVar(&rowFlags, "row", nil,
		`Row spec: comma-separated key=value pairs. Keys: company, custom, filing-type, date-from, date-to, exchange, sector, industry`)
	tableCmd.Flags().StringArrayVar(&colFlags, "col", nil,
		`Column spec: comma-separated key=value pairs. Keys: name, source, mode, query, semantic-query, keyword-query, web-query`)
	tableCmd.Flags().IntVar(&topN, "top-n", 5, "Number of source chunks per cell (1-20)")
	tableCmd.Flags().BoolVar(&stream, "stream", false, "Stream cell results via SSE; print progress to stderr")
	tableCmd.Flags().BoolVar(&async, "async", false, "Return task_id immediately; do not wait for full result")
	tableCmd.Flags().BoolVar(&watch, "watch", false, "Print watch URL to stderr before returning the full result")
	tableCmd.Flags().StringVar(&formatFlag, "format", "", "Output format: markdown (default TTY) | json | csv | xlsx")

	flags.RegisterAgentFlags(tableCmd)

	tableCmd.AddCommand(newTableRunCmd(version))
	tableCmd.AddCommand(newTableRerunCmd(version))
	tableCmd.AddCommand(newTableListCmd(version))
	tableCmd.AddCommand(newTableWatchCmd())

	return tableCmd
}

// ─── table run ───────────────────────────────────────────────────────────────

func newTableRunCmd(version string) *cobra.Command {
	var (
		stream     bool
		async      bool
		watch      bool
		formatFlag string
	)

	runCmd := &cobra.Command{
		Use:   "run <spec.yaml|spec.json>",
		Short: "Run a table from a spec file",
		Long: `Run a table research job from a YAML or JSON spec file.

  archivist table run ./q4-banking-comps.yaml
  cat spec.yaml | archivist table run --stdin

Defaults:
  When a row pins an issuer (issuer_key) without date_from / date_to,
  archivist fills "Last 6 months" automatically. Hard cap at 2 years
  for issuer-locked rows without a filing-type filter.
  Run 'archivist explain defaults' for details.`,
		Args: cobra.MaximumNArgs(1),
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,3,4,5,6,7,8",
			"mcp:read-only":       "false",
		},
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runTableFromSpec(cobraCmd, version, args, stream, async, watch, formatFlag)
		},
	}

	runCmd.Flags().BoolVar(&stream, "stream", false, "Stream cell results via SSE; print progress to stderr")
	runCmd.Flags().BoolVar(&async, "async", false, "Return task_id immediately; do not wait for full result")
	runCmd.Flags().BoolVar(&watch, "watch", false, "Print watch URL to stderr before returning the full result")
	runCmd.Flags().StringVar(&formatFlag, "format", "", "Output format: markdown (default TTY) | json | csv | xlsx")
	flags.RegisterAgentFlags(runCmd)

	return runCmd
}

// ─── table rerun ─────────────────────────────────────────────────────────────

func newTableRerunCmd(version string) *cobra.Command {
	var formatFlag string

	rerunCmd := &cobra.Command{
		Use:   "rerun <session_id>",
		Short: "Re-run a saved table session",
		Args:  cobra.ExactArgs(1),
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,3,4,5,6,7,8",
			"mcp:read-only":       "false",
		},
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runTableRerun(cobraCmd, version, args[0], formatFlag)
		},
	}

	rerunCmd.Flags().StringVar(&formatFlag, "format", "", "Output format: markdown | json | csv | xlsx")
	flags.RegisterAgentFlags(rerunCmd)
	return rerunCmd
}

// ─── table list ──────────────────────────────────────────────────────────────

func newTableListCmd(version string) *cobra.Command {
	var formatFlag string

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List saved table sessions",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,4,5",
			"mcp:read-only":       "true",
		},
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			return runTableList(cobraCmd, version, formatFlag)
		},
	}

	listCmd.Flags().StringVar(&formatFlag, "format", "", "Output format: table (default) | json")
	return listCmd
}

// ─── table watch ─────────────────────────────────────────────────────────────

func newTableWatchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch <session_id>",
		Short: "Print the web URL to watch a table session",
		Args:  cobra.ExactArgs(1),
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,3",
			"mcp:read-only":       "true",
		},
		RunE: func(cobraCmd *cobra.Command, args []string) error {
			sessionID := args[0]
			if !strings.HasPrefix(sessionID, sessionIDPrefix) {
				_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
					"invalid session ID %q: must start with %q\n", sessionID, sessionIDPrefix)
				return &cmd.ExitError{Code: cmd.ExitUsageError}
			}
			_, _ = fmt.Fprintln(cobraCmd.OutOrStdout(), watchBaseURL+sessionID)
			return nil
		},
	}
}

// ─── fluent flag mode execution ───────────────────────────────────────────────

func runTableFluent(
	cobraCmd *cobra.Command,
	version string,
	rowFlags, colFlags []string,
	topN int,
	stream, async, watch bool,
	formatFlag string,
) error {
	af := flags.ReadAgentFlags(cobraCmd)

	if len(rowFlags) == 0 || len(colFlags) == 0 {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(),
			"archivist table: at least one --row and one --col flag is required")
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}

	// Build spec from flags.
	spec, err := buildSpecFromFlags(rowFlags, colFlags, topN)
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "flag parse error: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}

	if err := spec.Validate(); err != nil {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), err.Error())
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}

	token, err := resolveToken(cobraCmd)
	if err != nil {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), err.Error())
		return &cmd.ExitError{Code: cmd.ExitAuthError}
	}
	c := client.New(token, version)

	// Company auto-resolution.
	if err := resolveCompanies(cobraCmd, c, spec); err != nil {
		return err
	}

	// Client-side date defaults (mirrors web row-filter-picker.tsx Last 6 months).
	applyRowDefaults(cobraCmd, spec, af.Quiet)

	// Cascade pre-validation.
	if err := validateCascade(cobraCmd, spec, formatFlag, af.DryRun); err != nil {
		return err
	}

	// Dry-run: print wire payload and exit.
	if af.DryRun {
		return printDryRun(cobraCmd, spec)
	}

	// Determine format.
	fmt_ := resolveFormat(cobraCmd, formatFlag)

	return executeTable(cobraCmd, c, spec, "", fmt_, stream, async, watch, af)
}

// ─── spec file mode execution ─────────────────────────────────────────────────

func runTableFromSpec(
	cobraCmd *cobra.Command,
	version string,
	args []string,
	stream, async, watch bool,
	formatFlag string,
) error {
	af := flags.ReadAgentFlags(cobraCmd)

	if af.Stdin && len(args) > 0 {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(),
			"--stdin and a file path argument are mutually exclusive")
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}

	var specName string
	var spec *tablespec.TableSpec
	var parseErr error

	if af.Stdin {
		specName = "stdin"
		spec, parseErr = parseSpecFromReader(os.Stdin, ".yaml")
	} else {
		if len(args) == 0 {
			_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(),
				"archivist table run: a spec file path or --stdin is required")
			return &cmd.ExitError{Code: cmd.ExitUsageError}
		}
		specPath := args[0]
		ext := strings.ToLower(filepath.Ext(specPath))
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
				"spec file must end in .yaml, .yml, or .json (got %q)\n", specPath)
			return &cmd.ExitError{Code: cmd.ExitUsageError}
		}
		f, err := os.Open(specPath)
		if err != nil {
			_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "File not found: %s\n", specPath)
			return &cmd.ExitError{Code: cmd.ExitNotFound}
		}
		defer func() { _ = f.Close() }()
		specName = strings.TrimSuffix(filepath.Base(specPath), filepath.Ext(specPath))
		spec, parseErr = parseSpecFromReader(f, ext)
	}

	if parseErr != nil {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), parseErr.Error())
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}

	if err := spec.Validate(); err != nil {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), err.Error())
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}

	token, err := resolveToken(cobraCmd)
	if err != nil {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), err.Error())
		return &cmd.ExitError{Code: cmd.ExitAuthError}
	}
	c := client.New(token, version)

	// Company auto-resolution.
	if err := resolveCompanies(cobraCmd, c, spec); err != nil {
		return err
	}

	// Client-side date defaults (mirrors web row-filter-picker.tsx Last 6 months).
	applyRowDefaults(cobraCmd, spec, af.Quiet)

	// Cascade pre-validation.
	if err := validateCascade(cobraCmd, spec, formatFlag, af.DryRun); err != nil {
		return err
	}

	// Dry-run: print wire payload and exit.
	if af.DryRun {
		return printDryRun(cobraCmd, spec)
	}

	fmt_ := resolveFormat(cobraCmd, formatFlag)

	return executeTable(cobraCmd, c, spec, specName, fmt_, stream, async, watch, af)
}

// ─── rerun execution ──────────────────────────────────────────────────────────

func runTableRerun(cobraCmd *cobra.Command, version, sessionID, formatFlag string) error {
	af := flags.ReadAgentFlags(cobraCmd)

	if !strings.HasPrefix(sessionID, sessionIDPrefix) {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
			"invalid session ID %q: must start with %q\n", sessionID, sessionIDPrefix)
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}

	token, err := resolveToken(cobraCmd)
	if err != nil {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), err.Error())
		return &cmd.ExitError{Code: cmd.ExitAuthError}
	}
	c := client.New(token, version)

	// Fetch the saved session spec.
	resp, err := c.Do(context.Background(), http.MethodGet, "/table-history/"+sessionID, nil)
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "server error: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "session not found: %s\n", sessionID)
		return &cmd.ExitError{Code: cmd.ExitNotFound}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "server returned %d\n", resp.StatusCode)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	// Decode session document (rows + columns fields).
	var sessionDoc struct {
		Rows    []tablespec.SpecRow    `json:"rows"`
		Columns []tablespec.SpecColumn `json:"columns"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessionDoc); err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "decode session: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	spec := &tablespec.TableSpec{
		TopN:    5,
		Rows:    sessionDoc.Rows,
		Columns: sessionDoc.Columns,
	}
	if len(spec.Rows) == 0 {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), "session has no rows")
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	// Cascade pre-validation.
	if err := validateCascade(cobraCmd, spec, formatFlag, af.DryRun); err != nil {
		return err
	}

	if af.DryRun {
		return printDryRun(cobraCmd, spec)
	}

	fmt_ := resolveFormat(cobraCmd, formatFlag)
	return executeTable(cobraCmd, c, spec, "", fmt_, false, false, false, af)
}

// ─── list execution ───────────────────────────────────────────────────────────

func runTableList(cobraCmd *cobra.Command, version, formatFlag string) error {
	token, err := resolveToken(cobraCmd)
	if err != nil {
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), err.Error())
		return &cmd.ExitError{Code: cmd.ExitAuthError}
	}
	c := client.New(token, version)

	resp, err := c.Do(context.Background(), http.MethodGet, "/table-history", nil)
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "server error: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "server returned %d\n", resp.StatusCode)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "read response: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	if formatFlag == "json" {
		_, _ = fmt.Fprintln(cobraCmd.OutOrStdout(), string(body))
		return nil
	}

	// Parse session list.
	var sessions []struct {
		ID         string `json:"id"`
		Title      string `json:"title"`
		UserTitle  string `json:"userTitle"`
		UpdatedAt  string `json:"updatedAt"`
		CellCount  int    `json:"cellCount"`
	}
	if err := json.Unmarshal(body, &sessions); err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "decode sessions: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	if len(sessions) == 0 {
		_, _ = fmt.Fprintln(cobraCmd.OutOrStdout(), "No saved table sessions.")
		return nil
	}

	w := cobraCmd.OutOrStdout()
	_, _ = fmt.Fprintf(w, "%-20s  %-40s  %5s  %s\n", "ID", "NAME", "CELLS", "UPDATED")
	for _, s := range sessions {
		name := s.UserTitle
		if name == "" {
			name = s.Title
		}
		updated := ""
		if len(s.UpdatedAt) >= 10 {
			updated = s.UpdatedAt[:10]
		}
		_, _ = fmt.Fprintf(w, "%-20s  %-40s  %5d  %s\n",
			truncate(s.ID, 20), truncate(name, 40), s.CellCount, updated)
	}
	return nil
}

// ─── shared helpers ───────────────────────────────────────────────────────────

// applyRowDefaults calls defaults.ApplyTableRowDefaults for every row in spec,
// emitting a one-line stderr notice per filled row (suppressed by --quiet).
func applyRowDefaults(cobraCmd *cobra.Command, spec *tablespec.TableSpec, quiet bool) {
	now := time.Now()
	for i := range spec.Rows {
		row, applied := defaults.ApplyTableRowDefaults(spec.Rows[i], now)
		if !applied.Filled {
			continue
		}
		spec.Rows[i] = row
		if !quiet {
			label := spec.Rows[i].Company
			if label == "" {
				label = fmt.Sprintf("#%d", i+1)
			}
			_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
				"[defaults applied] row %q: date_from=%s date_to=%s (last 6 months; see 'archivist explain defaults')\n",
				label, applied.DateFrom, applied.DateTo)
		}
	}
}

// resolveToken reads the token from --token flag or ARCHIVIST_TOKEN env var.
func resolveToken(cobraCmd *cobra.Command) (string, error) {
	tokenFlag, _ := cobraCmd.Root().Flags().GetString("token")
	token, err := auth.ResolveToken(tokenFlag)
	if err != nil {
		return "", fmt.Errorf("no ARCHIVIST_TOKEN set -- run 'archivist auth login' for setup instructions")
	}
	return token, nil
}

// resolveFormat returns the output.Format for the given flag value and TTY state.
func resolveFormat(cobraCmd *cobra.Command, formatFlag string) output.Format {
	if formatFlag != "" {
		return output.ParseFormat(formatFlag)
	}
	// Auto-JSON when stdout is not a TTY and no format was specified.
	fi, err := os.Stdout.Stat()
	if err != nil || (fi.Mode()&os.ModeCharDevice) == 0 {
		return output.FormatJSON
	}
	return output.FormatMarkdown
}

// parseSpecFromReader dispatches YAML or JSON parsing based on file extension.
func parseSpecFromReader(r io.Reader, ext string) (*tablespec.TableSpec, error) {
	if ext == ".json" {
		return tablespec.ParseJSON(r)
	}
	return tablespec.ParseYAML(r)
}

// buildSpecFromFlags translates --row and --col flag strings into a TableSpec.
func buildSpecFromFlags(rowFlags, colFlags []string, topN int) (*tablespec.TableSpec, error) {
	var rows []tablespec.SpecRow
	for _, rf := range rowFlags {
		row, err := parseRowFlag(rf)
		if err != nil {
			return nil, fmt.Errorf("--row %q: %w", rf, err)
		}
		rows = append(rows, row)
	}

	var cols []tablespec.SpecColumn
	for _, cf := range colFlags {
		col, err := parseColFlag(cf)
		if err != nil {
			return nil, fmt.Errorf("--col %q: %w", cf, err)
		}
		cols = append(cols, col)
	}

	return &tablespec.TableSpec{
		TopN:    topN,
		Rows:    rows,
		Columns: cols,
	}, nil
}

// parseRowFlag parses "company=aapl_us,filing-type=10-K,date-from=2024-01-01"
// into a SpecRow.
func parseRowFlag(s string) (tablespec.SpecRow, error) {
	kv, err := parseKVString(s)
	if err != nil {
		return tablespec.SpecRow{}, err
	}
	var row tablespec.SpecRow
	for k, v := range kv {
		switch k {
		case "company":
			row.Company = v
		case "custom":
			row.Custom = v
		case "filing-type":
			row.FilingTypes = strings.Split(v, ",")
		case "date-from":
			row.DateFrom = v
		case "date-to":
			row.DateTo = v
		case "exchange":
			row.Exchange = v
		case "sector":
			row.Sector = v
		case "industry":
			row.Industry = v
		default:
			return tablespec.SpecRow{}, fmt.Errorf("unknown row key %q", k)
		}
	}
	return row, nil
}

// parseColFlag parses "name=Revenue,source=filings,query=annual revenue"
// into a SpecColumn.
func parseColFlag(s string) (tablespec.SpecColumn, error) {
	kv, err := parseKVString(s)
	if err != nil {
		return tablespec.SpecColumn{}, err
	}
	var col tablespec.SpecColumn
	for k, v := range kv {
		switch k {
		case "name":
			col.Name = v
		case "source":
			col.Source = v
		case "mode":
			col.Mode = v
		case "query":
			col.Query = v
		case "semantic-query":
			col.SemanticQuery = v
		case "keyword-query":
			col.KeywordQuery = v
		case "web-query":
			col.WebQuery = v
		default:
			return tablespec.SpecColumn{}, fmt.Errorf("unknown column key %q", k)
		}
	}
	return col, nil
}

// parseKVString splits a comma-separated "key=value,key2=value2" string into
// a map. Values may contain commas inside quoted strings (not supported for
// brevity — values with commas require spec files).
func parseKVString(s string) (map[string]string, error) {
	result := map[string]string{}
	parts := strings.Split(s, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.IndexByte(part, '=')
		if idx < 0 {
			return nil, fmt.Errorf("expected key=value, got %q", part)
		}
		k := strings.TrimSpace(part[:idx])
		v := strings.TrimSpace(part[idx+1:])
		result[k] = v
	}
	return result, nil
}

// resolveCompanies runs company name auto-resolution for all rows that contain
// a free-text company name. Modifies spec in place.
func resolveCompanies(cobraCmd *cobra.Command, c resolver.Client, spec *tablespec.TableSpec) error {
	for i := range spec.Rows {
		row := &spec.Rows[i]
		name := row.Company
		if name == "" {
			continue
		}
		// Already an issuer key — bypass.
		if issuerKeyPattern.MatchString(name) {
			continue
		}

		verbHint := `archivist table --row "company=`
		res, err := resolver.AutoResolve(context.Background(), c, name, verbHint)
		if err != nil {
			var ambig *resolver.AmbiguousError
			if errors.As(err, &ambig) {
				switch ambig.Envelope.Error {
				case "ambiguous_match":
					_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
						"ambiguous company name %q — multiple candidates found. Use an explicit issuer_key:\n", name)
					for _, cand := range ambig.Envelope.Candidates {
						_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
							"  %s  %s (%s, %s)  %d filings\n",
							cand.IssuerKey, cand.CompanyName, cand.Symbol, cand.Exchange, cand.FilingCount)
					}
					_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
						"Re-run with: archivist table --row \"company=<issuer_key>,...\"\n")
					return &cmd.ExitError{Code: cmd.ExitAmbiguousMatch}
				case "not_found":
					_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "No company found for '%s'\n", name)
					return &cmd.ExitError{Code: cmd.ExitNotFound}
				}
			}
			_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "resolution error: %v\n", err)
			return &cmd.ExitError{Code: cmd.ExitServerError}
		}

		row.Company = res.IssuerKey
	}
	return nil
}

// validateCascade converts a tablespec.TableSpec to a cascade.Spec (filters
// map form) and calls cascade.ValidateSpec. On *CascadeError, emits the
// canonical exit-8 envelope (markdown to stderr, optional JSON to stdout) and
// returns ExitCascadeViolation.
func validateCascade(
	cobraCmd *cobra.Command,
	spec *tablespec.TableSpec,
	formatFlag string,
	dryRun bool,
) error {
	cs := &cascade.Spec{}
	for _, r := range spec.Rows {
		filters := map[string]interface{}{}
		if r.Company != "" {
			filters["issuer_key"] = r.Company
		}
		if r.Custom != "" {
			filters["custom_entity"] = r.Custom
		}
		if r.DateFrom != "" {
			filters["date_from"] = r.DateFrom
		}
		if r.DateTo != "" {
			filters["date_to"] = r.DateTo
		}
		if r.Exchange != "" {
			filters["exchange"] = r.Exchange
		}
		if len(r.FilingTypes) > 0 {
			filters["formtype"] = r.FilingTypes
		}
		cs.Rows = append(cs.Rows, cascade.SpecRow{Filters: filters})
	}
	for _, c := range spec.Columns {
		cs.Columns = append(cs.Columns, cascade.SpecColumn{Name: c.Name, Source: c.Source})
	}

	if err := cascade.ValidateSpec(cs); err != nil {
		var cerr *cascade.CascadeError
		if !errors.As(err, &cerr) {
			_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "cascade validation: %v\n", err)
			return &cmd.ExitError{Code: cmd.ExitGenericError}
		}
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "cascade rule violation: %s\n", cerr.Message)

		if formatFlag == "json" {
			type envelope struct {
				Error    string `json:"error"`
				Rule     string `json:"rule"`
				Message  string `json:"message"`
				ExitCode int    `json:"exit_code"`
			}
			enc := json.NewEncoder(cobraCmd.OutOrStdout())
			enc.SetIndent("", "  ")
			_ = enc.Encode(envelope{
				Error:    "cascade rule violation",
				Rule:     cerr.Rule,
				Message:  cerr.Message,
				ExitCode: cmd.ExitCascadeViolation,
			})
		}
		return &cmd.ExitError{Code: cmd.ExitCascadeViolation}
	}
	return nil
}

// printDryRun marshals the wire payload to stdout and exits 0.
func printDryRun(cobraCmd *cobra.Command, spec *tablespec.TableSpec) error {
	payload := buildWirePayload(spec)
	enc := json.NewEncoder(cobraCmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "encode dry-run payload: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitGenericError}
	}
	return nil
}

// ─── wire payload ─────────────────────────────────────────────────────────────

// wireFilters is the chat-api filtersSchema shape.
type wireFilters struct {
	IssuerKey      string   `json:"issuer_key,omitempty"`
	FormType       []string `json:"formtype,omitempty"`
	FormDescription string  `json:"formdescription,omitempty"`
	DateFrom       string   `json:"date_from,omitempty"`
	DateTo         string   `json:"date_to,omitempty"`
	Exchange       string   `json:"exchange,omitempty"`
	Sector         string   `json:"sector,omitempty"`
	Industry       string   `json:"industry,omitempty"`
}

// wireRow is the chat-api rowSchema shape.
type wireRow struct {
	Filters     wireFilters `json:"filters"`
	IssuerName  string      `json:"issuer_name,omitempty"`
	IssuerSymbol string     `json:"issuer_symbol,omitempty"`
}

// wireColumn is the chat-api columnSchema shape (discriminated union flattened).
type wireColumn struct {
	Name            string `json:"name"`
	Source          string `json:"source,omitempty"`
	RetrievalMode   string `json:"retrieval_mode,omitempty"`
	Query           string `json:"query,omitempty"`
	SemanticQuery   string `json:"semantic_query,omitempty"`
	KeywordQuery    string `json:"keyword_query,omitempty"`
	WebQuery        string `json:"web_query,omitempty"`
}

// wirePayload is the full searchBodySchema shape.
type wirePayload struct {
	Rows         []wireRow    `json:"rows"`
	Columns      []wireColumn `json:"columns"`
	TopN         int          `json:"top_n"`
	SourceOffset *int         `json:"source_offset,omitempty"`
}

// buildWirePayload translates a TableSpec into the chat-api wire format.
func buildWirePayload(spec *tablespec.TableSpec) wirePayload {
	var rows []wireRow
	for _, r := range spec.Rows {
		var wr wireRow
		if r.Custom != "" {
			wr.Filters = wireFilters{}
			wr.IssuerName = r.Custom
		} else {
			wr.Filters = wireFilters{
				IssuerKey: r.Company,
				FormType:  r.FilingTypes,
				DateFrom:  r.DateFrom,
				DateTo:    r.DateTo,
				Exchange:  r.Exchange,
				Sector:    r.Sector,
				Industry:  r.Industry,
			}
		}
		rows = append(rows, wr)
	}

	var cols []wireColumn
	for _, c := range spec.Columns {
		wc := wireColumn{
			Name:          c.Name,
			Source:        c.Source,
			Query:         c.Query,
			SemanticQuery: c.SemanticQuery,
			KeywordQuery:  c.KeywordQuery,
			WebQuery:      c.WebQuery,
		}
		src := c.Source
		if src == "" {
			src = "filings"
		}
		if src == "web" {
			wc.RetrievalMode = "web"
		} else {
			if c.Mode != "" {
				wc.RetrievalMode = c.Mode
			} else {
				wc.RetrievalMode = "rrf"
			}
		}
		cols = append(cols, wc)
	}

	return wirePayload{
		Rows:         rows,
		Columns:      cols,
		TopN:         spec.TopN,
		SourceOffset: spec.SourceOffset,
	}
}

// ─── HTTP execution ───────────────────────────────────────────────────────────

// executeTable posts to /table/search or /table/search/stream and renders output.
func executeTable(
	cobraCmd *cobra.Command,
	c *client.Client,
	spec *tablespec.TableSpec,
	specName string,
	fmt_ output.Format,
	stream, async, watch bool,
	af flags.AgentFlags,
) error {
	payload := buildWirePayload(spec)
	body, err := json.Marshal(payload)
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "marshal payload: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitGenericError}
	}

	ctx := context.Background()

	if stream {
		return executeTableStream(cobraCmd, c, body, fmt_, af)
	}

	// Blocking POST.
	resp, err := c.Do(ctx, http.MethodPost, "/table/search", bytes.NewReader(body))
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "request error: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}
	defer func() { _ = resp.Body.Close() }()

	if err := handleVersionHeader(cobraCmd, resp); err != nil {
		return err
	}
	if err := handleErrorStatus(cobraCmd, resp); err != nil {
		return err
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "read response: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	var result output.TableResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "decode response: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}

	// Build watch URL.
	if result.TaskID != "" {
		result.WatchURL = watchBaseURL + result.TaskID
	}

	// --watch: print watch URL to stderr before result.
	if watch && result.WatchURL != "" && !af.Quiet {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "Watch URL: %s\n", result.WatchURL)
	}

	// --async: print task_id + watch_url and return.
	if async {
		if result.TaskID == "" {
			_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(),
				"async mode not yet supported by server: response did not include task_id")
			return &cmd.ExitError{Code: cmd.ExitUsageError}
		}
		if fmt_ == output.FormatJSON {
			enc := json.NewEncoder(cobraCmd.OutOrStdout())
			enc.SetIndent("", "  ")
			_ = enc.Encode(map[string]string{
				"task_id":   result.TaskID,
				"watch_url": result.WatchURL,
			})
		} else {
			_, _ = fmt.Fprintf(cobraCmd.OutOrStdout(), "task_id: %s\nwatch_url: %s\n",
				result.TaskID, result.WatchURL)
		}
		return nil
	}

	// applied_defaults echo on stderr (markdown mode only, suppressible with --quiet).
	if !af.Quiet && !stream && len(result.AppliedDefaults) > 0 {
		echoAppliedDefaults(cobraCmd, result.AppliedDefaults)
	}

	// Render output.
	return renderTableResult(cobraCmd, &result, fmt_, specName, af.Compact)
}

// executeTableStream handles /table/search/stream SSE endpoint.
func executeTableStream(
	cobraCmd *cobra.Command,
	c *client.Client,
	body []byte,
	fmt_ output.Format,
	af flags.AgentFlags,
) error {
	resp, err := c.Do(context.Background(), http.MethodPost, "/table/search/stream", bytes.NewReader(body))
	if err != nil {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "stream request error: %v\n", err)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}
	defer func() { _ = resp.Body.Close() }()

	if err := handleVersionHeader(cobraCmd, resp); err != nil {
		return err
	}
	if err := handleErrorStatus(cobraCmd, resp); err != nil {
		return err
	}

	// SSE: read events, print progress to stderr, collect final result.
	scanner := bufio.NewScanner(resp.Body)
	var finalResult output.TableResult

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}

		var event map[string]interface{}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event["type"] {
		case "cell-start":
			if !af.Quiet {
				rowID, _ := event["row_id"].(string)
				colID, _ := event["col_id"].(string)
				_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "  [%s] [%s] ...\n", rowID, colID)
			}
		case "cell-done":
			if !af.Quiet {
				rowID, _ := event["row_id"].(string)
				colID, _ := event["col_id"].(string)
				_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "  [%s] [%s] done\n", rowID, colID)
			}
		case "complete":
			// Final result is in the event payload.
			resultBytes, err := json.Marshal(event["result"])
			if err == nil {
				_ = json.Unmarshal(resultBytes, &finalResult)
			}
		case "task_id":
			taskID, _ := event["task_id"].(string)
			if taskID != "" {
				finalResult.TaskID = taskID
				finalResult.WatchURL = watchBaseURL + taskID
				if !af.Quiet {
					_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "[task_id: %s]\n", taskID)
					_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "[watch: %s]\n", finalResult.WatchURL)
				}
			}
		}
	}

	return renderTableResult(cobraCmd, &finalResult, fmt_, "", af.Compact)
}

// renderTableResult dispatches to the appropriate format writer.
func renderTableResult(
	cobraCmd *cobra.Command,
	result *output.TableResult,
	fmt_ output.Format,
	specName string,
	compact bool,
) error {
	stdout := cobraCmd.OutOrStdout()
	stderr := cobraCmd.ErrOrStderr()

	switch fmt_ {
	case output.FormatJSON:
		return output.WriteJSON(stdout, result)

	case output.FormatCSV:
		return output.WriteCSV(stdout, result)

	case output.FormatXLSX:
		// If stdout is a TTY, write to a named file; otherwise write binary to stdout.
		fi, err := os.Stdout.Stat()
		isTTY := err == nil && (fi.Mode()&os.ModeCharDevice) != 0
		if isTTY {
			fileName := "archivist-table-" + time.Now().Format("20060102-150405") + ".xlsx"
			if specName != "" {
				fileName = specName + ".xlsx"
			}
			f, err := os.Create(fileName)
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "create xlsx file: %v\n", err)
				return &cmd.ExitError{Code: cmd.ExitGenericError}
			}
			defer func() { _ = f.Close() }()
			if err := output.WriteXLSX(f, result); err != nil {
				_, _ = fmt.Fprintf(stderr, "write xlsx: %v\n", err)
				return &cmd.ExitError{Code: cmd.ExitGenericError}
			}
			_, _ = fmt.Fprintf(stderr, "Written to %s\n", fileName)
		} else {
			if err := output.WriteXLSX(stdout, result); err != nil {
				_, _ = fmt.Fprintf(stderr, "write xlsx: %v\n", err)
				return &cmd.ExitError{Code: cmd.ExitGenericError}
			}
		}
		return nil

	default: // markdown
		if err := output.WriteMarkdown(stdout, result, compact); err != nil {
			_, _ = fmt.Fprintf(stderr, "write markdown: %v\n", err)
			return &cmd.ExitError{Code: cmd.ExitGenericError}
		}
		// Markdown footers go to stderr (not stdout) so pipes are clean.
		if result.TaskID != "" {
			_, _ = fmt.Fprintf(stderr, "[task_id: %s]\n", result.TaskID)
		}
		if result.WatchURL != "" {
			_, _ = fmt.Fprintf(stderr, "[watch: %s]\n", result.WatchURL)
		}
		return nil
	}
}

// handleVersionHeader checks X-Archivist-Min-CLI-Version and exits 5 if blocked.
func handleVersionHeader(cobraCmd *cobra.Command, resp *http.Response) error {
	minVer := resp.Header.Get("X-Archivist-Min-CLI-Version")
	if minVer == "" {
		return nil
	}
	// TODO: semver comparison when version package is richer. For now, only block
	// if the server header is non-empty (indicates we should upgrade).
	curVer := resp.Request.Header.Get("X-Archivist-CLI-Version")
	if curVer != "" && minVer > curVer {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
			"Server requires archivist-cli >= %s. You have %s. Run 'archivist update' to upgrade.\n",
			minVer, curVer)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	}
	return nil
}

// handleErrorStatus maps HTTP status codes to typed exit errors.
func handleErrorStatus(cobraCmd *cobra.Command, resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(),
			"auth error: server returned %d. Check your ARCHIVIST_TOKEN.\n", resp.StatusCode)
		return &cmd.ExitError{Code: cmd.ExitAuthError}
	case resp.StatusCode == http.StatusTooManyRequests:
		_, _ = fmt.Fprintln(cobraCmd.ErrOrStderr(), "rate limit exceeded. Wait and retry or check your quota.")
		return &cmd.ExitError{Code: cmd.ExitRateLimit}
	case resp.StatusCode >= 500:
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "server error: %d\n", resp.StatusCode)
		return &cmd.ExitError{Code: cmd.ExitServerError}
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(resp.Body)
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "client error %d: %s\n", resp.StatusCode, string(body))
		return &cmd.ExitError{Code: cmd.ExitUsageError}
	}
	return nil
}

// echoAppliedDefaults prints the server-applied defaults to stderr.
func echoAppliedDefaults(cobraCmd *cobra.Command, defaults map[string]interface{}) {
	var parts []string
	if v, ok := defaults["dateFrom"]; ok {
		parts = append(parts, fmt.Sprintf("dateFrom=%v", v))
	}
	if v, ok := defaults["dateTo"]; ok {
		parts = append(parts, fmt.Sprintf("dateTo=%v", v))
	}
	if v, ok := defaults["filingType"]; ok {
		parts = append(parts, fmt.Sprintf("filingType=%v", v))
	}
	if len(parts) > 0 {
		_, _ = fmt.Fprintf(cobraCmd.ErrOrStderr(), "[defaults applied] %s\n", strings.Join(parts, " "))
	}
}

// truncate clips a string to maxLen characters with an ellipsis.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
