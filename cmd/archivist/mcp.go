// mcp.go implements `archivist mcp serve` (Story 39.7) — the cobratree MCP
// server. A tree-walker derives one MCP tool per Cobra verb at runtime; every
// tools/call dispatches through a FRESH root command via root.Execute(), the
// same code path a shell user exercises. The 1:1 verb↔tool mapping is the
// architecture (E39 §2.2): compound tools mean new Cobra verbs first.
//
// This file lives in package main because only package main can assemble the
// full command tree — `table` (cmd/archivist/table.go) registers here, not in
// internal/cmd.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/cmd"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// mcpFlagDenylist names flags that never appear in any tool schema (AC8):
// token (credential injection), stdin (protocol-stdin collision under MCP),
// stream/quiet/no-color (TTY-presentation; meaningless for agents).
var mcpFlagDenylist = map[string]bool{
	"token":    true,
	"stdin":    true,
	"stream":   true,
	"quiet":    true,
	"no-color": true,
}

// mcpSkipCommands names auto-generated Cobra commands the walker never maps.
var mcpSkipCommands = map[string]bool{
	"help":       true,
	"completion": true,
}

// toolSpec carries everything the MCP layer needs for one tool: registration
// metadata (Name/Description/ReadOnly/Schema) plus dispatch wiring (Path,
// Positionals, FlagFor, StdinProp).
type toolSpec struct {
	Name        string
	Description string
	ReadOnly    bool
	// Path is the argv prefix, e.g. ["companies", "search"].
	Path []string
	// Positionals are required string property names, in Use-string order.
	Positionals []string
	// FlagFor maps schema property names to pflag names (filing_type -> filing-type).
	FlagFor map[string]string
	// StdinProp names the property whose value feeds the command's stdin via
	// SetIn (+ implicit --stdin argv). Empty for all tools except table_run.
	StdinProp string
	Schema    *jsonschema.Schema
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
		Name: strings.Join(path, "_"),
		// Literal comparison on purpose: table carries mcp:read-only "false",
		// which must NOT read as true.
		ReadOnly: c.Annotations["mcp:read-only"] == "true",
		Path:     path,
		FlagFor:  map[string]string{},
	}
	spec.Description = toolDescription(c)

	props := map[string]*jsonschema.Schema{}
	var required []string

	// Positionals first (Use-string order). Special case table run: the
	// <spec.yaml|spec.json> positional is replaced by a required spec_yaml
	// string fed via stdin at dispatch (architecture E39 §2.2).
	if spec.Name == "table_run" {
		spec.StdinProp = "spec_yaml"
		props["spec_yaml"] = &jsonschema.Schema{
			Type: "string",
			Description: "Table spec as YAML — the contents of the file you would pass to " +
				"'archivist table run'. Top-level keys: top_n, rows, columns.",
		}
		required = append(required, "spec_yaml")
	} else {
		for _, pos := range positionalNames(c.Use) {
			props[pos] = &jsonschema.Schema{
				Type:        "string",
				Description: "Required positional argument <" + pos + ">.",
			}
			required = append(required, pos)
			spec.Positionals = append(spec.Positionals, pos)
		}
	}

	// Local flags only — VisitAll on Flags() (NOT InheritedFlags) keeps the
	// root --token out naturally; the denylist catches locals like chat's
	// --token as belt-and-braces.
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

// mcpInstructions is sent to hosts in the initialize result (T4.5).
const mcpInstructions = "Tools mirror the archivist CLI verbs 1:1 and return the same JSON " +
	"envelopes the CLI emits when piped. Results carry [cite:N] citations and watch URLs " +
	"that resolve on mosaic-finance.com (the audit surface for every call). Use " +
	"companies_search to resolve free-text company names to issuer keys before filtering; " +
	"explain_cascade and explain_defaults document the filter rules the server enforces. " +
	"Long table runs can exceed host tool timeouts: pass async=true and poll with table_watch."

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
// newRoot builds a PRISTINE full command tree (including table, excluding
// mcp itself — the factory closure predates mcp registration in main.go, so
// recursion is structurally impossible). The walker reads one fresh tree at
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
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve every archivist verb as an MCP tool over stdio",
		Long: `Serve every archivist verb as an MCP tool over stdio.

The server speaks JSON-RPC on stdin/stdout (stderr is the log channel) and
exposes one tool per CLI verb with the same auth flow, exit-code semantics,
and web-UI audit surface. Requires ARCHIVIST_TOKEN (or --token).

Claude Desktop config:
  {"mcpServers":{"archivist":{"command":"archivist","args":["mcp","serve"],
   "env":{"ARCHIVIST_TOKEN":"ak_..."}}}}`,
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,4",
			"mcp:hidden":          "true",
		},
		RunE: func(c *cobra.Command, args []string) error {
			// AC1: validate the token BEFORE serving — fail fast with the
			// CLI's canonical guidance and exit 4.
			tokenFlag, _ := c.Root().PersistentFlags().GetString("token")
			if _, err := auth.ResolveToken(tokenFlag); err != nil {
				if errors.Is(err, auth.ErrNoToken) {
					_, _ = fmt.Fprintln(c.ErrOrStderr(),
						"archivist mcp serve: no credentials found. Run 'archivist auth login' for setup instructions.")
				} else {
					_, _ = fmt.Fprintln(c.ErrOrStderr(), err.Error())
				}
				return &cmd.ExitError{Code: cmd.ExitAuthError}
			}

			server, count := buildMCPServer(newRoot, version, tokenFlag)
			// Stderr only — process stdout belongs to the SDK transport (AC9).
			_, _ = fmt.Fprintf(c.ErrOrStderr(),
				"archivist mcp serve: %d tools registered (v%s)\n", count, version)

			err := server.Run(c.Context(), &mcp.StdioTransport{})
			if err == nil || errors.Is(err, context.Canceled) {
				// Clean EOF / ctx-cancel — exit 0 (AC1).
				return nil
			}
			_, _ = fmt.Fprintf(c.ErrOrStderr(), "archivist mcp serve: %v\n", err)
			return &cmd.ExitError{Code: cmd.ExitGenericError}
		},
	}
}

// buildMCPServer assembles the MCP server with one tool per walked verb.
// tokenOverride carries a --token passed to `mcp serve` into every dispatch.
// Returns the server and the registered tool count.
func buildMCPServer(newRoot func() *cobra.Command, version, tokenOverride string) (*mcp.Server, int) {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "archivist", Version: version},
		&mcp.ServerOptions{Instructions: mcpInstructions},
	)
	specs := collectTools(newRoot())
	for _, spec := range specs {
		// Untyped AddTool on purpose: schemas are walker-built at runtime;
		// the generic mcp.AddTool[In,Out] infers schemas from Go structs.
		server.AddTool(&mcp.Tool{
			Name:        spec.Name,
			Description: spec.Description,
			InputSchema: spec.Schema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: spec.ReadOnly},
		}, newToolHandler(newRoot, spec, tokenOverride))
	}
	return server, len(specs)
}

// newToolHandler returns the dispatch handler for one tool. Each call decodes
// the raw arguments, builds argv, and executes a FRESH root command with
// buffered stdout/stderr — concurrency-safe, no flag-state bleed (AC9).
//
// Accepted v1 limit: verbs build context.Background() internally, so MCP-side
// cancellation does not abort in-flight HTTP calls. Long table fan-outs may
// exceed host tool timeouts — agents should use async + table_watch.
func newToolHandler(newRoot func() *cobra.Command, spec toolSpec, tokenOverride string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		argv, stdinPayload, usageErr := buildArgv(spec, req.Params.Arguments, tokenOverride)
		if usageErr != "" {
			return errorResult(cmd.ExitUsageError, usageErr, ""), nil
		}

		r := newRoot()
		var stdout, stderr bytes.Buffer
		r.SetOut(&stdout)
		r.SetErr(&stderr)
		if stdinPayload != "" {
			r.SetIn(strings.NewReader(stdinPayload))
		}
		r.SetArgs(argv)

		if err := r.Execute(); err != nil {
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
// argv, the stdin payload (table_run's spec_yaml), and a usage-error message
// ("" when valid). The untyped AddTool path does NOT schema-validate inputs,
// so required/unknown/type checks happen here.
func buildArgv(spec toolSpec, rawArgs json.RawMessage, tokenOverride string) (argv []string, stdinPayload, usageErr string) {
	args := map[string]any{}
	if len(rawArgs) > 0 {
		dec := json.NewDecoder(bytes.NewReader(rawArgs))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return nil, "", fmt.Sprintf("invalid tool arguments JSON: %v", err)
		}
	}

	argv = append(argv, spec.Path...)
	consumed := map[string]bool{}

	// Positionals in Use-string order.
	for _, pos := range spec.Positionals {
		v, ok := args[pos]
		if !ok {
			return nil, "", fmt.Sprintf("missing required argument %q", pos)
		}
		s, ok := v.(string)
		if !ok {
			return nil, "", fmt.Sprintf("argument %q must be a string", pos)
		}
		argv = append(argv, s)
		consumed[pos] = true
	}

	// table_run: spec_yaml feeds stdin; the verb reads it via InOrStdin (T2).
	if spec.StdinProp != "" {
		v, ok := args[spec.StdinProp]
		if !ok {
			return nil, "", fmt.Sprintf("missing required argument %q", spec.StdinProp)
		}
		s, ok := v.(string)
		if !ok {
			return nil, "", fmt.Sprintf("argument %q must be a string", spec.StdinProp)
		}
		stdinPayload = s
		argv = append(argv, "--stdin")
		consumed[spec.StdinProp] = true
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
			return nil, "", fmt.Sprintf("unknown argument %q for tool %s", k, spec.Name)
		}
		switch v := args[k].(type) {
		case []any:
			for _, elem := range v {
				s, ok := jsonScalarString(elem)
				if !ok {
					return nil, "", fmt.Sprintf("argument %q: array elements must be scalars", k)
				}
				argv = append(argv, "--"+flagName+"="+s)
			}
		default:
			s, ok := jsonScalarString(v)
			if !ok {
				return nil, "", fmt.Sprintf("argument %q must be a scalar or array of scalars", k)
			}
			argv = append(argv, "--"+flagName+"="+s)
		}
	}

	if tokenOverride != "" {
		argv = append(argv, "--token", tokenOverride)
	}
	return argv, stdinPayload, ""
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
// stdout matters because exit-6 ambiguous-company JSON lands there.
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
