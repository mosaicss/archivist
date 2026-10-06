// mcp.go implements `archivist mcp serve` (Story 39.7) — the cobratree MCP
// server. A tree-walker derives one MCP tool per Cobra verb at runtime; every
// tools/call dispatches through a FRESH root command via root.Execute(), the
// same code path a shell user exercises. The 1:1 verb↔tool mapping is the
// architecture (E39 §2.2): compound tools mean new Cobra verbs first.
//
// The mcp command lives in package main: it needs the root factory, and the
// dispatch roots it builds must not contain mcp itself.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/mosaicss/archivist/internal/fsutil"
	"github.com/mosaicss/archivist/internal/guidance"
	"github.com/mosaicss/archivist/internal/taskscope"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// mcpFlagDenylist names flags that never appear in any tool schema (AC8):
// token (credential injection), stdin (protocol-stdin collision under MCP),
// stream/quiet/no-color (TTY-presentation; meaningless for agents), dry-run
// (a shell check that prints the request; Story 81.4).
var mcpFlagDenylist = map[string]bool{
	"token":    true,
	"stdin":    true,
	"stream":   true,
	"quiet":    true,
	"no-color": true,
	"dry-run":  true,
}

// mcpSkipCommands names auto-generated Cobra commands the walker never maps.
var mcpSkipCommands = map[string]bool{
	"help":       true,
	"completion": true,
}

// toolSpec carries everything the MCP layer needs for one tool: registration
// metadata (Name/Title/Description/ReadOnly/Schema) plus dispatch wiring
// (Path, Positionals, FlagFor).
type toolSpec struct {
	Name        string
	Title       string
	Description string
	ReadOnly    bool
	// Path is the argv prefix, e.g. ["companies", "search"].
	Path []string
	// Positionals are required string property names, in Use-string order.
	Positionals []string
	// FlagFor maps schema property names to pflag names (date_from -> date-from).
	FlagFor map[string]string
	Schema  *jsonschema.Schema
	// PinFormat drops the format argument and always dispatches
	// --format=<PinFormat>. Task mode (Story 78.31) pins json, the server
	// body unchanged, so each passage's cite_as reaches the agent and the
	// workspace parses the records; any other token pins compact (Story
	// 81.4), the minified projection fitted to a page budget.
	PinFormat string
}

// collectTools walks the Cobra tree depth-first and returns one toolSpec per
// runnable, non-hidden command. A command maps to a tool iff it has RunE, is
// not annotated mcp:hidden, and is not an auto-generated command. An
// mcp:hidden annotation on a parent hides its whole subtree.
func collectTools(root *cobra.Command) []toolSpec {
	var specs []toolSpec
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, child := range c.Commands() {
			name := child.Name()
			if mcpSkipCommands[name] {
				continue
			}
			if child.Annotations["mcp:hidden"] == "true" {
				continue
			}
			childPath := append(append([]string{}, path...), name)
			if child.RunE != nil {
				specs = append(specs, buildToolSpec(child, childPath))
			}
			walk(child, childPath)
		}
	}
	walk(root, nil)
	return specs
}

// buildToolSpec derives the MCP tool definition for one Cobra command.
func buildToolSpec(c *cobra.Command, path []string) toolSpec {
	spec := toolSpec{
		Name:  strings.Join(path, "_"),
		Title: c.Annotations["mcp:title"],
		// Literal comparison on purpose: only the string "true" reads as true.
		ReadOnly: c.Annotations["mcp:read-only"] == "true",
		Path:     path,
		FlagFor:  map[string]string{},
	}
	spec.Description = toolDescription(c)

	props := map[string]*jsonschema.Schema{}
	var required []string

	// Positionals first (Use-string order).
	for _, pos := range positionalNames(c.Use) {
		props[pos] = &jsonschema.Schema{
			Type:        "string",
			Description: "Required positional argument <" + pos + ">.",
		}
		required = append(required, pos)
		spec.Positionals = append(spec.Positionals, pos)
	}

	// Local flags only — VisitAll on Flags() (NOT InheritedFlags) keeps the
	// root --token out naturally; the denylist catches a local --token as
	// belt-and-braces.
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if mcpFlagDenylist[f.Name] || f.Hidden {
			return
		}
		prop := strings.ReplaceAll(f.Name, "-", "_")
		s := &jsonschema.Schema{Description: f.Usage}
		switch f.Value.Type() {
		case "bool":
			s.Type = "boolean"
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "count":
			s.Type = "integer"
		case "float32", "float64":
			s.Type = "number"
		case "stringArray", "stringSlice":
			s.Type = "array"
			s.Items = &jsonschema.Schema{Type: "string"}
		default:
			s.Type = "string"
		}
		props[prop] = s
		spec.FlagFor[prop] = f.Name
	})

	sort.Strings(required)
	spec.Schema = &jsonschema.Schema{
		Type:       "object",
		Properties: props,
		Required:   required,
		// falseSchema: marshals to "additionalProperties": false (AC2).
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
	return spec
}

// toolDescription builds the tool description from Short (+ Long, truncated
// sanely) and appends the typed exit codes so agents can self-correct (T3.4).
func toolDescription(c *cobra.Command) string {
	const maxBody = 900
	body := strings.TrimSpace(c.Short)
	if long := strings.TrimSpace(c.Long); long != "" {
		body += "\n\n" + long
	}
	if len(body) > maxBody {
		cut := strings.LastIndexByte(body[:maxBody], ' ')
		if cut < maxBody/2 {
			cut = maxBody
		}
		body = body[:cut] + " …"
	}
	if codes := c.Annotations["pp:typed-exit-codes"]; codes != "" {
		body += "\n\nExit codes: " + codes
	}
	return body
}

var placeholderRe = regexp.MustCompile(`<([^>]+)>`)

// positionalNames extracts <placeholder> names from a Cobra Use string and
// sanitizes each to a schema property identifier (<session_id> -> session_id).
func positionalNames(use string) []string {
	var names []string
	for _, m := range placeholderRe.FindAllStringSubmatch(use, -1) {
		names = append(names, sanitizeIdent(m[1]))
	}
	return names
}

// sanitizeIdent lowercases and maps every non [a-z0-9_] run to a single "_".
func sanitizeIdent(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// ─── mcp serve command ───────────────────────────────────────────────────────

// Server instructions (T4.5, Story 78.31) are Mosaic's compact research
// guidance, the text chat-api serves at GET /agent-guidance (fetched at
// startup, embedded copy otherwise), plus these exit code notes. A task token
// (Mosaic is the UI: cite_as citations) takes the mosaic-ui guidance, any other
// token the agent-ui guidance (cite each passage url as a markdown link).
const mcpExitNotes = "Tool errors name an archivist exit code: 2 bad arguments, 3 nothing found, " +
	"4 no credential or no Mosaic Pro account, 6 ambiguous symbol, 7 monthly fair use limit reached."

// mcpLocalSuffix (Story 81.4) maps the guidance's hosted flow onto this
// server's tool names, between the guidance and the exit code notes.
const mcpLocalSuffix = "This server's tools: search (latest_only for the newest filing of a form), " +
	"read_passage, toc and read_section; filing_id feeds the last two. filings lists a company's " +
	"filings newest first; find says whether one filing mentions a term."

// Hosts cut MCP instructions at 2048 characters (Claude Code); the exit code
// notes get at most 200 of them.
const (
	mcpInstructionsMax = 2048
	mcpExitNotesMax    = 200
)

// guidanceSurface is the guidance surface for a token mode.
func guidanceSurface(taskMode bool) string {
	if taskMode {
		return guidance.SurfaceMosaicUI
	}
	return guidance.SurfaceAgentUI
}

// jsLen is a string's length in UTF-16 code units, the unit hosts count.
func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

// mcpInstructionsFrom joins the compact guidance, the local suffix and the
// exit code notes. When that is too long the suffix goes first, then a live
// text is replaced by the embedded one (with the suffix when it fits), which
// always fits.
func mcpInstructionsFrom(g guidance.Text, surface string) string {
	join := func(body string, suffix bool) string {
		text := strings.TrimRight(body, "\n") + "\n"
		if suffix {
			text += mcpLocalSuffix + "\n"
		}
		return text + mcpExitNotes
	}
	embedded := guidance.Embedded(surface, guidance.FormCompact).Body
	for _, text := range []string{join(g.Body, true), join(g.Body, false), join(embedded, true)} {
		if jsLen(text) <= mcpInstructionsMax {
			return text
		}
	}
	return join(embedded, false)
}

// embeddedMCPInstructions is the instructions with no fetch (the builders
// tests use; mcp serve itself fetches).
func embeddedMCPInstructions(taskMode bool) string {
	surface := guidanceSurface(taskMode)
	return mcpInstructionsFrom(guidance.Embedded(surface, guidance.FormCompact), surface)
}

// fetchMCPGuidance fetches the compact guidance (a variable for tests).
var fetchMCPGuidance = func(ctx context.Context, baseURL, surface string) guidance.Text {
	return guidance.Fetch(ctx, baseURL, surface, guidance.FormCompact)
}

// exitCodeNames mirrors internal/cmd/exitcodes.go (architecture E36 §11.4).
// Dispatch stamps these on MCP error results so agents can self-correct.
var exitCodeNames = map[int]string{
	cmd.ExitOK:               "ok",
	cmd.ExitGenericError:     "generic error",
	cmd.ExitUsageError:       "usage error",
	cmd.ExitNotFound:         "not found",
	cmd.ExitAuthError:        "auth error",
	cmd.ExitServerError:      "server error",
	cmd.ExitAmbiguousMatch:   "ambiguous match",
	cmd.ExitRateLimit:        "rate limit",
	cmd.ExitCascadeViolation: "cascade violation",
	cmd.ExitNotImplemented:   "not implemented",
}

// newMCPCmd returns the hidden `mcp` parent with its `serve` subcommand.
// newRoot builds a PRISTINE full command tree (excluding mcp itself — the
// factory closure predates mcp registration in main.go, so recursion is
// structurally impossible). The walker reads one fresh tree at
// startup; every dispatch executes another. Fresh-root-per-call is
// non-negotiable: Cobra flag values live in closures captured at
// construction, so a reused root bleeds flag state across concurrent calls.
func newMCPCmd(newRoot func() *cobra.Command, version string) *cobra.Command {
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run archivist as an MCP server",
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2",
			"mcp:hidden":          "true",
		},
	}
	mcpCmd.AddCommand(newMCPServeCmd(newRoot, version))
	return mcpCmd
}

func newMCPServeCmd(newRoot func() *cobra.Command, version string) *cobra.Command {
	var tokenFile, publishDir, publishSession string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve every archivist verb as an MCP tool over stdio",
		Long: `Serve every archivist verb as an MCP tool over stdio.

The server speaks JSON-RPC on stdin/stdout (stderr is the log channel) and
exposes one tool per CLI verb with the same auth flow, exit-code semantics,
and web-UI audit surface. Requires ARCHIVIST_TOKEN (or --token).

--token-file names a file holding the token; it is read again on every tool
call, so a supervisor can rotate the token without restarting the server.
A session task token (mst_..., minted by 'archivist connect') switches the
server to task mode: only the search and read tools whose chat-api routes a
task token may call are exposed.

--publish-session and --publish-dir (set together, task mode only) add the
publish_artifact tool for that 'archivist connect' session: it publishes a
file from the session's working directory to the Mosaic workspace.

Claude Desktop config:
  {"mcpServers":{"archivist":{"command":"archivist","args":["mcp","serve"],
   "env":{"ARCHIVIST_TOKEN":"ak_..."}}}}`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,4",
			"mcp:hidden":          "true",
		},
		RunE: func(c *cobra.Command, args []string) error {
			tokenFlag, _ := c.Root().PersistentFlags().GetString("token")
			if tokenFlag != "" && tokenFile != "" {
				_, _ = fmt.Fprintln(c.ErrOrStderr(), "archivist mcp serve: use --token or --token-file, not both.")
				return &cmd.ExitError{Code: cmd.ExitUsageError}
			}
			// AC1: validate the token BEFORE serving — fail fast with the
			// CLI's canonical guidance and exit 4.
			var token string
			var err error
			src := staticToken(tokenFlag)
			if tokenFile != "" {
				src = fileToken(tokenFile)
				token, err = src()
			} else {
				token, err = auth.ResolveToken(tokenFlag)
			}
			if err != nil {
				if errors.Is(err, auth.ErrNoToken) {
					_, _ = fmt.Fprintln(c.ErrOrStderr(),
						"archivist mcp serve: no credentials found. Run 'archivist auth login --token ak_...' to save a credential, or set ARCHIVIST_TOKEN.")
				} else {
					_, _ = fmt.Fprintln(c.ErrOrStderr(), "archivist mcp serve: "+err.Error())
				}
				return &cmd.ExitError{Code: cmd.ExitAuthError}
			}

			taskMode := auth.IsTaskToken(token)
			var pub *publishConfig
			if publishDir != "" || publishSession != "" {
				if !taskMode || publishDir == "" || !publishSessionRe.MatchString(publishSession) || !filepath.IsAbs(publishDir) {
					_, _ = fmt.Fprintln(c.ErrOrStderr(), "archivist mcp serve: --publish-session (a session id) and an absolute --publish-dir go together, with a task token only.")
					return &cmd.ExitError{Code: cmd.ExitUsageError}
				}
				pub = &publishConfig{SessionID: publishSession, Dir: publishDir}
			}
			surface := guidanceSurface(taskMode)
			ctx := c.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			g := fetchMCPGuidance(ctx, client.ResolveBaseURL(), surface)
			if g.Source == guidance.SourceLive {
				_, _ = fmt.Fprintf(c.ErrOrStderr(), "archivist mcp serve: research guidance %s %s (%s)\n", surface, g.Source, g.Digest)
			} else {
				_, _ = fmt.Fprintf(c.ErrOrStderr(), "archivist mcp serve: research guidance %s %s (%s; live fetch: %s)\n", surface, g.Source, g.Digest, g.Reason)
			}
			server, count := buildMCPServerFull(newRoot, version, src, taskMode, pub, mcpInstructionsFrom(g, surface))
			mode := ""
			if taskMode {
				mode = ", task mode"
			}
			// Stderr only — process stdout belongs to the SDK transport (AC9).
			_, _ = fmt.Fprintf(c.ErrOrStderr(),
				"archivist mcp serve: %d tools registered (v%s%s)\n", count, version, mode)

			err = server.Run(c.Context(), &mcp.StdioTransport{})
			if err == nil || errors.Is(err, context.Canceled) {
				// Clean EOF / ctx-cancel — exit 0 (AC1).
				return nil
			}
			_, _ = fmt.Fprintf(c.ErrOrStderr(), "archivist mcp serve: %v\n", err)
			return &cmd.ExitError{Code: cmd.ExitGenericError}
		},
	}
	serve.Flags().StringVar(&tokenFile, "token-file", "",
		"Read the token from this file on every tool call (rotation without restart)")
	serve.Flags().StringVar(&publishSession, "publish-session", "",
		"Task mode: the archivist connect session publish_artifact publishes for")
	serve.Flags().StringVar(&publishDir, "publish-dir", "",
		"Task mode: the session working directory publish_artifact may read from")
	return serve
}

// tokenSource yields the token appended to each dispatched argv. An empty
// token leaves resolution to the verb (ARCHIVIST_TOKEN, credentials file).
type tokenSource func() (string, error)

func staticToken(token string) tokenSource {
	return func() (string, error) { return token, nil }
}

// fileToken re-reads path on every call. The file holds exactly one token
// (surrounding whitespace ignored) in a valid format.
func fileToken(path string) tokenSource {
	return func() (string, error) {
		data, err := fsutil.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("token file %s is empty", path)
		}
		if err := auth.ValidateTokenFormat(token); err != nil {
			return "", fmt.Errorf("token file %s: %w", path, err)
		}
		return token, nil
	}
}

// buildMCPServer assembles the MCP server with one tool per walked verb.
// tokenOverride carries a --token passed to `mcp serve` into every dispatch.
// Returns the server and the registered tool count.
func buildMCPServer(newRoot func() *cobra.Command, version, tokenOverride string) (*mcp.Server, int) {
	return buildMCPServerWith(newRoot, version, staticToken(tokenOverride), auth.IsTaskToken(tokenOverride))
}

// buildMCPServerWith is buildMCPServer with a token source; taskMode limits
// the tools to the task allowlist.
func buildMCPServerWith(newRoot func() *cobra.Command, version string, src tokenSource, taskMode bool) (*mcp.Server, int) {
	return buildMCPServerTask(newRoot, version, src, taskMode, nil)
}

// buildMCPServerTask is buildMCPServerWith plus, in task mode with a
// session, the publish_artifact tool (Story 78.18).
func buildMCPServerTask(newRoot func() *cobra.Command, version string, src tokenSource, taskMode bool, pub *publishConfig) (*mcp.Server, int) {
	return buildMCPServerFull(newRoot, version, src, taskMode, pub, embeddedMCPInstructions(taskMode))
}

// buildMCPServerFull is buildMCPServerTask with the server instructions given.
func buildMCPServerFull(newRoot func() *cobra.Command, version string, src tokenSource, taskMode bool, pub *publishConfig, instructions string) (*mcp.Server, int) {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "archivist", Version: version},
		&mcp.ServerOptions{Instructions: instructions},
	)
	count := 0
	for _, spec := range collectTools(newRoot()) {
		if taskMode && !taskscope.ToolAllowed(spec.Name) {
			continue
		}
		if taskMode {
			spec = pinFormat(spec, "json")
		} else {
			spec = pinFormat(spec, "compact")
		}
		count++
		// Untyped AddTool on purpose: schemas are walker-built at runtime;
		// the generic mcp.AddTool[In,Out] infers schemas from Go structs.
		server.AddTool(&mcp.Tool{
			Name:        spec.Name,
			Title:       spec.Title,
			Description: spec.Description,
			InputSchema: spec.Schema,
			Annotations: &mcp.ToolAnnotations{
				Title:           spec.Title,
				ReadOnlyHint:    spec.ReadOnly,
				DestructiveHint: boolPtr(false),
				OpenWorldHint:   boolPtr(false),
			},
		}, newToolHandler(newRoot, spec, src))
	}
	if taskMode && pub != nil && taskscope.ToolAllowed(taskscope.PublishTool) {
		addPublishTool(server, version, src, *pub)
		count++
	}
	return server, count
}

func boolPtr(b bool) *bool { return &b }

// pinFormat removes a tool's format argument and marks it to dispatch
// --format=<format>: json in task mode, compact otherwise. A tool without a
// format flag is unchanged.
func pinFormat(spec toolSpec, format string) toolSpec {
	if _, ok := spec.FlagFor["format"]; !ok {
		return spec
	}
	flagFor := make(map[string]string, len(spec.FlagFor))
	for k, v := range spec.FlagFor {
		if k != "format" {
			flagFor[k] = v
		}
	}
	schema := *spec.Schema
	props := make(map[string]*jsonschema.Schema, len(schema.Properties))
	for k, v := range schema.Properties {
		if k != "format" {
			props[k] = v
		}
	}
	schema.Properties = props
	spec.FlagFor, spec.Schema, spec.PinFormat = flagFor, &schema, format
	return spec
}

// newToolHandler returns the dispatch handler for one tool. Each call decodes
// the raw arguments, builds argv, and executes a FRESH root command with
// buffered stdout/stderr — concurrency-safe, no flag-state bleed (AC9).
//
// The dispatched root runs under the request context (ExecuteContext), so a
// host cancellation reaches the verb's Client.Do and aborts its HTTP call.
func newToolHandler(newRoot func() *cobra.Command, spec toolSpec, src tokenSource) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		tokenOverride, err := src()
		if err != nil {
			return errorResult(cmd.ExitAuthError, err.Error(), ""), nil
		}
		argv, usageErr := buildArgv(spec, req.Params.Arguments, tokenOverride)
		if usageErr != "" {
			return errorResult(cmd.ExitUsageError, usageErr, ""), nil
		}

		r := newRoot()
		var stdout, stderr bytes.Buffer
		r.SetOut(&stdout)
		r.SetErr(&stderr)
		r.SetIn(strings.NewReader(""))
		r.SetArgs(argv)

		if err := r.ExecuteContext(ctx); err != nil {
			code := cmd.ExitUsageError // non-ExitError errors map to usage semantics (main.go parity)
			var exitErr *cmd.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.Code
			} else {
				_, _ = fmt.Fprintln(&stderr, err.Error())
			}
			return errorResult(code, stderr.String(), stdout.String()), nil
		}

		text := stdout.String()
		if text == "" {
			text = stderr.String()
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil
	}
}

// buildArgv translates decoded tool arguments into a CLI argv. Returns the
// argv and a usage-error message ("" when valid). The untyped AddTool path
// does NOT schema-validate inputs, so required/unknown/type checks happen
// here.
func buildArgv(spec toolSpec, rawArgs json.RawMessage, tokenOverride string) (argv []string, usageErr string) {
	args := map[string]any{}
	if len(rawArgs) > 0 {
		dec := json.NewDecoder(bytes.NewReader(rawArgs))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return nil, fmt.Sprintf("invalid tool arguments JSON: %v", err)
		}
	}

	argv = append(argv, spec.Path...)
	consumed := map[string]bool{}

	// Positionals in Use-string order. They are appended after a "--"
	// terminator below, so a value such as "-10% revenue" or "--stdin" stays
	// an argument and is never parsed as a flag.
	var positionals []string
	for _, pos := range spec.Positionals {
		v, ok := args[pos]
		if !ok {
			return nil, fmt.Sprintf("missing required argument %q", pos)
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Sprintf("argument %q must be a string", pos)
		}
		positionals = append(positionals, s)
		consumed[pos] = true
	}

	// Flags in sorted property order for deterministic argv.
	keys := make([]string, 0, len(args))
	for k := range args {
		if !consumed[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		flagName, ok := spec.FlagFor[k]
		if !ok {
			return nil, fmt.Sprintf("unknown argument %q for tool %s", k, spec.Name)
		}
		switch v := args[k].(type) {
		case []any:
			for _, elem := range v {
				s, ok := jsonScalarString(elem)
				if !ok {
					return nil, fmt.Sprintf("argument %q: array elements must be scalars", k)
				}
				argv = append(argv, "--"+flagName+"="+s)
			}
		default:
			s, ok := jsonScalarString(v)
			if !ok {
				return nil, fmt.Sprintf("argument %q must be a scalar or array of scalars", k)
			}
			argv = append(argv, "--"+flagName+"="+s)
		}
	}

	if spec.PinFormat != "" {
		argv = append(argv, "--format="+spec.PinFormat)
	}
	if tokenOverride != "" {
		argv = append(argv, "--token", tokenOverride)
	}
	if len(positionals) > 0 {
		argv = append(argv, "--")
		argv = append(argv, positionals...)
	}
	return argv, ""
}

// jsonScalarString renders a decoded JSON scalar as a flag value. Numbers
// arrive as json.Number (UseNumber), so 20 never becomes "2e+01".
func jsonScalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case json.Number:
		return x.String(), true
	default:
		return "", false
	}
}

// errorResult shapes a typed CLI failure as an MCP tool error (AC6): the text
// names the exit code, then carries captured stderr and stdout sections —
// stdout matters because the JSON error envelope and a zero-result search
// body land there.
func errorResult(code int, stderrText, stdoutText string) *mcp.CallToolResult {
	name := exitCodeNames[code]
	if name == "" {
		name = "unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "archivist exited with exit code %d (%s)", code, name)
	if s := strings.TrimSpace(stderrText); s != "" {
		b.WriteString("\n\n--- stderr ---\n")
		b.WriteString(s)
	}
	if s := strings.TrimSpace(stdoutText); s != "" {
		b.WriteString("\n\n--- stdout ---\n")
		b.WriteString(s)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
		IsError: true,
	}
}
