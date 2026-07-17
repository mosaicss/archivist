package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/cmd/flags"
	"github.com/mosaicss/archivist/internal/resolver"
	"github.com/spf13/cobra"
)

// cliChatModels mirrors chat-api CLI_CHAT_MODELS (vertex.ts) — the cheap suite,
// i.e. the registry minus the web-chat-only premium. The server allowlist
// (Story 41.8) is the authoritative gate; this client guard is defense in depth
// plus a faster, clearer error. Keep in sync on every lineup change.
// 2026-06-23: gemini-2.5-flash-lite dropped — the chat path sends
// thinkingConfig.thinkingLevel, which Gemini 2.5 rejects (400); it is no longer
// a registered model server-side. gemini-3.1-flash-lite is the cheap option.
// 2026-07-17 (mosaic Story 52.5): gemini-2.5-flash dropped — the Gemini 2.5
// line retires 2026-10-16 and the web-search engine repointed to
// gemini-3-flash; the registry is Gemini-3-only now.
var cliChatModels = []string{
	"gemini-3.1-flash-lite",
	"gemini-3-flash",
}

func isCLIChatModelAllowed(model string) bool {
	for _, m := range cliChatModels {
		if m == model {
			return true
		}
	}
	return false
}

// NewChatCmd returns the full `archivist chat` command (replaces the stub from 36.1).
func NewChatCmd(version string) *cobra.Command {
	var (
		company        string
		filingType     string
		dateFrom       string
		dateTo         string
		attachFilings  []string
		model          string
		conversationID string
		stream         bool
		format         string
		token          string
	)

	cmd := &cobra.Command{
		Use:   "chat <question>",
		Short: "Run a research question against Mosaic filings",
		Args:  cobra.ExactArgs(1),
		Annotations: map[string]string{
			"pp:typed-exit-codes": "0,2,3,4,5,6,7",
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			question := args[0]
			af := flags.ReadAgentFlags(cmd)

			// --dry-run + --stream is invalid
			if af.DryRun && stream {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "--dry-run and --stream cannot be combined")
				return &ExitError{Code: ExitUsageError}
			}

			// Story 41.8 — restrict --model to the cheap suite. The premium model
			// is web-chat-only; the server allowlist rejects it on the CLI surface
			// (400), but failing fast here is a clearer UX. `table` is unaffected.
			if model != "" && !isCLIChatModelAllowed(model) {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"archivist chat: model %q is not available on the CLI. Choose one of: %s.\n",
					model, strings.Join(cliChatModels, ", "))
				return &ExitError{Code: ExitUsageError}
			}

			// Resolve token: --token flag > ARCHIVIST_TOKEN env
			tokenFlag, _ := cmd.Root().PersistentFlags().GetString("token")
			if token != "" {
				tokenFlag = token
			}
			resolvedToken, err := auth.ResolveToken(tokenFlag)
			if err != nil {
				if errors.Is(err, auth.ErrNoToken) {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
						"archivist chat: no credentials found. Run 'archivist auth login --token ak_...' to save a credential, or set ARCHIVIST_TOKEN.")
				} else {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
				}
				return &ExitError{Code: ExitAuthError}
			}

			// Detect TTY; auto-JSON when stdout is not a TTY
			isTTY := isTerminal(cmd.OutOrStdout())
			if !isTTY && format == "" {
				format = "json"
			}
			if format == "" {
				format = "markdown"
			}

			// --no-color: auto-enable when not TTY
			noColor := af.NoColor || !isTTY

			// Build structured params from flags
			sp := &structuredParams{}
			resolvedCompany := company
			filingTypes := parseCSV(filingType)

			// --stdin: read and merge JSON params from stdin
			if af.Stdin {
				stdinParams, err := readStdinParams(cmd)
				if err != nil {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "archivist chat:", err.Error())
					return &ExitError{Code: ExitUsageError}
				}
				// stdin filter params win over explicit flags
				if stdinParams.Company != "" {
					resolvedCompany = stdinParams.Company
				}
				if stdinParams.DateFrom != "" {
					dateFrom = stdinParams.DateFrom
				}
				if stdinParams.DateTo != "" {
					dateTo = stdinParams.DateTo
				}
				if len(stdinParams.FilingType) > 0 {
					filingTypes = stdinParams.FilingType
				}
			}

			// Build client
			c := client.New(resolvedToken, version)
			if af.Quiet {
				c.SetQuiet(true)
			}
			c.SetStderr(cmd.ErrOrStderr())

			// --company auto-resolution
			if resolvedCompany != "" {
				// Literal issuer_key (cik:NNN, uuid:UUID, sym_us/ca) — bypass AutoResolve
				// so the typed id passes through unchanged. Bug fixed for the table verb
				// in 37.5; this is the chat-verb sibling.
				if !resolver.IsLiteralIssuerKey(resolvedCompany) {
					// Free-text: resolve via /companies/search
					issuerKey, resolveErr := resolveCompany(cmd, c, resolvedCompany, format)
					if resolveErr != nil {
						return resolveErr
					}
					resolvedCompany = issuerKey
				}
				sp.Company = resolvedCompany
			}

			if len(filingTypes) > 0 {
				sp.FilingType = filingTypes
			}
			if dateFrom != "" {
				sp.DateFrom = dateFrom
			}
			if dateTo != "" {
				sp.DateTo = dateTo
			}

			// Build request body
			msgID, err := newUUID()
			if err != nil {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: failed to generate request ID: %v\n", err)
				return &ExitError{Code: ExitGenericError}
			}

			reqBody := chatRequest{
				Messages: []chatMessage{
					{
						ID:    msgID,
						Role:  "user",
						Parts: []interface{}{map[string]string{"type": "text", "text": question}},
					},
				},
			}
			if conversationID != "" {
				reqBody.ConversationID = conversationID
			}
			if model != "" {
				reqBody.Model = model
			}
			if len(attachFilings) > 0 {
				reqBody.AttachedFilingIDs = attachFilings
			}
			if sp.Company != "" || len(sp.FilingType) > 0 || sp.DateFrom != "" || sp.DateTo != "" {
				reqBody.StructuredParams = sp
			}

			// --dry-run: print payload and exit
			if af.DryRun {
				return dryRunOutput(cmd, c, reqBody, stream)
			}

			// Execute the request (always SSE — chat-api always streams)
			return executeChatRequest(cmd, c, reqBody, stream, format, noColor, af)
		},
	}

	cmd.Flags().StringVar(&company, "company", "", "Filter to a single company. Free-text triggers auto-resolution.")
	cmd.Flags().StringVar(&filingType, "filing-type", "", "Comma-separated filing type(s): 10-K, 10-Q, 8-K, etc.")
	cmd.Flags().StringVar(&dateFrom, "date-from", "", "Start of date range (inclusive), YYYY-MM-DD")
	cmd.Flags().StringVar(&dateTo, "date-to", "", "End of date range (inclusive), YYYY-MM-DD")
	cmd.Flags().StringArrayVar(&attachFilings, "attach-filing", nil, "Attach one or more specific filing IDs (repeatable)")
	cmd.Flags().StringVar(&model, "model", "", "Override model (default: user saved preference). CLI allows the cheap suite only; premium is web chat only.")
	cmd.Flags().StringVar(&conversationID, "conversation", "", "Resume an existing conversation thread")
	cmd.Flags().BoolVar(&stream, "stream", false, "Use SSE streaming; progress on stderr, final answer on stdout")
	cmd.Flags().StringVar(&format, "format", "", "Output format: markdown or json (default: markdown; auto-json when piped)")
	cmd.Flags().StringVar(&token, "token", "", "Per-call token override (overrides env var and credentials file)")

	flags.RegisterAgentFlags(cmd)

	return cmd
}

// resolveCompany calls GET /companies/search and returns an issuer_key or an ExitError.
func resolveCompany(cmd *cobra.Command, c *client.Client, query, format string) (string, error) {
	path := "/companies/search?q=" + url.QueryEscape(query) + "&limit=5"
	resp, err := c.Do(context.Background(), http.MethodGet, path, nil)
	if err != nil {
		var exitErr *client.ExitCodeError
		if errors.As(err, &exitErr) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: server error resolving company: %v\n", err)
			return "", &ExitError{Code: exitErr.Code}
		}
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: network error resolving company: %v\n", err)
		return "", &ExitError{Code: ExitServerError}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"archivist chat: authentication failed. Run 'archivist auth status' to check your token.")
		return "", &ExitError{Code: ExitAuthError}
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: server error (HTTP %d) resolving company.\n", resp.StatusCode)
		return "", &ExitError{Code: ExitServerError}
	}

	var candidates []companyCandidate
	if err := json.NewDecoder(resp.Body).Decode(&candidates); err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: could not parse company search response: %v\n", err)
		return "", &ExitError{Code: ExitServerError}
	}

	switch len(candidates) {
	case 0:
		msg := fmt.Sprintf("archivist chat: company %q not found. Try a ticker symbol or different name.", query)
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), msg)
		return "", &ExitError{Code: ExitNotFound}

	case 1:
		return candidates[0].IssuerKey, nil

	default:
		if format == "json" || !isTerminal(cmd.OutOrStdout()) {
			out := map[string]interface{}{
				"error":      "AMBIGUOUS_COMPANY",
				"candidates": candidates,
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			_ = enc.Encode(out)
		} else {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"archivist chat: company %q is ambiguous. Matches:\n", query)
			for i, cand := range candidates {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"  %d. %s (%s) -- %s\n", i+1, cand.CompanyName, cand.IssuerKey, cand.Exchange)
			}
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Re-run with --company <issuer_key> to select one.")
		}
		return "", &ExitError{Code: ExitAmbiguousMatch}
	}
}

// executeChatRequest sends the chat request and handles the SSE response.
func executeChatRequest(
	cmd *cobra.Command,
	c *client.Client,
	reqBody chatRequest,
	stream bool,
	format string,
	noColor bool,
	af flags.AgentFlags,
) error {
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: failed to serialize request: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.BaseURL+"/chat", bytes.NewReader(bodyBytes))
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: failed to build request: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Archivist-CLI-Version", c.Version)
	req.Header.Set("X-Archivist-Origin", c.Origin)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("archivist-cli/%s (%s/%s)", c.Version, c.OS, c.Arch))
	// Always request SSE; chat-api always streams
	req.Header.Set("Accept", "text/event-stream")

	httpClient := &http.Client{Timeout: 120 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: network error: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}
	defer func() { _ = resp.Body.Close() }()

	// Handle version + quota headers
	if min := resp.Header.Get("X-Archivist-Min-CLI-Version"); min != "" {
		if isOlderSemver(c.Version, min) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"Server requires archivist-cli >= %s. You have %s. Run 'archivist update' to upgrade.\n",
				min, c.Version)
			return &ExitError{Code: ExitServerError}
		}
	}
	if remaining := resp.Header.Get("X-Queries-Remaining"); remaining != "" && !af.Quiet {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[quota] %s queries remaining\n", remaining)
	}

	// Error status codes
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"archivist chat: authentication failed. Run 'archivist auth status' to check your token.")
		if format == "json" {
			writeJSONError(cmd.OutOrStdout(), "AUTH_FAILED",
				"authentication failed. Run 'archivist auth status' to check your token.", ExitAuthError)
		}
		return &ExitError{Code: ExitAuthError}
	case http.StatusPaymentRequired:
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"archivist chat: quota exceeded. Run 'archivist usage' to check remaining queries.")
		if format == "json" {
			writeJSONError(cmd.OutOrStdout(), "QUOTA_EXCEEDED",
				"quota exceeded. Run 'archivist usage' to check remaining queries.", ExitRateLimit)
		}
		return &ExitError{Code: ExitRateLimit}
	}
	if resp.StatusCode >= 500 {
		msg := fmt.Sprintf("archivist chat: server error (HTTP %d). Try again later.", resp.StatusCode)
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), msg)
		if format == "json" {
			writeJSONError(cmd.OutOrStdout(), "SERVER_ERROR", msg, ExitServerError)
		}
		return &ExitError{Code: ExitServerError}
	}

	// Any other non-2xx (mosaic Story 52.5): 400 MODEL_NOT_ALLOWED from the
	// authoritative server allowlist, 404, 429 from the CLI rate limiter, etc.
	// Surface the standardized {error, code, suggestion} body. Pre-52.5 these
	// fell through to the SSE consumer, which found no "data:" frames in a
	// JSON error body and rendered an empty result with exit 0.
	if resp.StatusCode >= 300 {
		return surfaceChatHTTPError(cmd, resp, format)
	}

	// Parse SSE stream
	result, err := consumeSSEStream(resp.Body, stream, cmd.ErrOrStderr(), af.Quiet, noColor)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: stream error: %v\n", err)
		return &ExitError{Code: ExitServerError}
	}

	return renderChatResult(cmd, result, format, noColor, af)
}

// consumeSSEStream reads the SSE response, optionally printing progress to stderr.
// Returns a chatResult with the assembled text, citations, and conversation ID.
func consumeSSEStream(body io.Reader, showProgress bool, stderr io.Writer, quiet bool, noColor bool) (*chatResult, error) {
	result := &chatResult{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}

		var event sseEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			// Not all data lines are JSON; skip non-JSON lines gracefully
			continue
		}

		parseSSEEvent(&event, result, showProgress, stderr, quiet)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading stream: %w", err)
	}

	return result, nil
}

// parseSSEEvent handles a single parsed SSE event, updating result and printing progress.
func parseSSEEvent(event *sseEvent, result *chatResult, showProgress bool, stderr io.Writer, quiet bool) {
	switch event.Type {
	case "text-delta":
		result.Markdown += event.TextDelta

	case "tool-output-available":
		// AI SDK v6 emits tool results as "tool-output-available" with the
		// payload under `output`. The earlier event "tool-input-available"
		// (handled below) carries the toolName for the same toolCallId.
		toolName := result.toolNameByCallID[event.ToolCallID]
		if toolName == "search_filings" && event.Output != nil {
			parseCitationsFromToolResult(event.Output, result)
		}

	case "tool-input-available":
		// Track toolCallId → toolName so the matching tool-output-available
		// later can be routed to the right citation handler.
		if result.toolNameByCallID == nil {
			result.toolNameByCallID = map[string]string{}
		}
		if event.ToolCallID != "" && event.ToolName != "" {
			result.toolNameByCallID[event.ToolCallID] = event.ToolName
		}
		// Progress: show tool invocations when streaming
		if showProgress && !quiet && event.ToolName != "" {
			_, _ = fmt.Fprintf(stderr, "  %s...\n", toolProgressMessage(event.ToolName, event.ToolInput))
		}

	case "data-status":
		// Transient progress (tool_start, tool_complete) — print summary if streaming
		if showProgress && !quiet && event.Data != nil {
			if summary, ok := event.Data["summary"].(string); ok && summary != "" {
				_, _ = fmt.Fprintf(stderr, "  %s\n", summary)
			}
		}

	case "finish":
		// Final event — may carry conversationId and taskId
		if event.ConversationID != "" {
			result.ConversationID = event.ConversationID
		}
		if event.TaskID != "" {
			result.TaskID = event.TaskID
		}
		if event.AppliedDefaults != nil {
			result.AppliedDefaults = event.AppliedDefaults
		}

	case "conversation-id":
		if event.ConversationID != "" {
			result.ConversationID = event.ConversationID
		}

	case "step-finish":
		if event.ConversationID != "" {
			result.ConversationID = event.ConversationID
		}
	}
}

// parseCitationsFromToolResult extracts citations from a search_filings
// tool result. The chat-api wraps the source list under `sources` in the
// tool-output-available payload, but the pre-36.3 test fixtures used a
// top-level array. Try both shapes so existing tests keep passing.
func parseCitationsFromToolResult(result interface{}, r *chatResult) {
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	// New (production) shape: { sources: [...], ... }
	var wrapped struct {
		Sources []citationSource `json:"sources"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.Sources) > 0 {
		appendCitations(r, wrapped.Sources)
		return
	}
	// Legacy / test-fixture shape: top-level array.
	var sources []citationSource
	if err := json.Unmarshal(data, &sources); err == nil {
		appendCitations(r, sources)
	}
}

func appendCitations(r *chatResult, sources []citationSource) {
	for _, s := range sources {
		if s.SourceNumber > 0 {
			r.Citations = append(r.Citations, chatCitation{
				Number:    s.SourceNumber,
				Ticker:    s.Ticker,
				FormType:  s.FormType,
				DateFiled: s.DateFiled,
				Section:   s.Section,
				Page:      s.Page,
				URL:       s.URL,
			})
		}
	}
}

// renderChatResult writes the final output to stdout/stderr.
func renderChatResult(cmd *cobra.Command, result *chatResult, format string, noColor bool, af flags.AgentFlags) error {
	watchURL := ""
	if result.ConversationID != "" {
		watchURL = "https://mosaic-finance.com/chat/" + result.ConversationID
	}

	// Applied defaults: print to stderr unless quiet
	if result.AppliedDefaults != nil && !af.Quiet {
		var parts []string
		if v, ok := result.AppliedDefaults["dateFrom"]; ok {
			parts = append(parts, "dateFrom="+fmt.Sprint(v))
		}
		if v, ok := result.AppliedDefaults["dateTo"]; ok {
			parts = append(parts, "dateTo="+fmt.Sprint(v))
		}
		if v, ok := result.AppliedDefaults["filingType"]; ok {
			parts = append(parts, "filingType="+fmt.Sprint(v))
		}
		if len(parts) > 0 {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[defaults applied] %s\n", strings.Join(parts, " "))
		}
	}

	if format == "json" {
		return renderJSON(cmd.OutOrStdout(), cmd.ErrOrStderr(), result, watchURL, af)
	}
	return renderMarkdown(cmd.OutOrStdout(), cmd.ErrOrStderr(), result, watchURL, af, noColor)
}

// markupBlockRegex strips <highlights>...</highlights> and
// <follow-ups>...</follow-ups> blocks that chat-api emits inline for the
// web UI's hover/preview features. CLI markdown output drops them; the
// JSON path preserves them (raw markdown is available to programmatic
// consumers as result.Markdown).
var markupBlockRegex = regexp.MustCompile(`(?s)\s*<(?:highlights|follow-ups)>.*?</(?:highlights|follow-ups)>\s*`)

func stripChatMarkupBlocks(s string) string {
	out := markupBlockRegex.ReplaceAllString(s, "")
	return strings.TrimRight(out, "\n") + "\n"
}

func renderMarkdown(stdout, stderr io.Writer, result *chatResult, watchURL string, af flags.AgentFlags, noColor bool) error {
	// Main markdown answer to stdout (with web-UI-only markup stripped)
	_, _ = fmt.Fprintln(stdout, stripChatMarkupBlocks(result.Markdown))

	if !af.Compact {
		// Citations to stdout
		if len(result.Citations) > 0 {
			_, _ = fmt.Fprintln(stdout)
			_, _ = fmt.Fprintln(stdout, "---citations---")
			for _, c := range result.Citations {
				line := fmt.Sprintf("%d. %s %s %s", c.Number, c.Ticker, c.FormType, c.DateFiled)
				if c.Section != "" {
					line += " -- " + c.Section
				}
				if c.Page > 0 {
					line += fmt.Sprintf(", p.%d", c.Page)
				}
				if c.URL != "" {
					line += ". " + c.URL
				}
				_, _ = fmt.Fprintln(stdout, line)
			}
		}

		// Watch/conversation footer to stderr (not stdout — clean pipes)
		if result.ConversationID != "" {
			_, _ = fmt.Fprintf(stderr, "[conversation: %s]\n", result.ConversationID)
		}
		if watchURL != "" {
			_, _ = fmt.Fprintf(stderr, "[watch: %s]\n", watchURL)
		}
	}
	return nil
}

func renderJSON(stdout, stderr io.Writer, result *chatResult, watchURL string, af flags.AgentFlags) error {
	out := map[string]interface{}{
		"markdown":        result.Markdown,
		"conversation_id": result.ConversationID,
		"applied_defaults": func() interface{} {
			if result.AppliedDefaults != nil {
				return result.AppliedDefaults
			}
			return map[string]interface{}{}
		}(),
	}

	if !af.Compact {
		out["citations"] = result.Citations
		if result.TaskID != "" {
			out["task_id"] = result.TaskID
		}
		out["watch_url"] = watchURL
	} else {
		out["citations"] = []interface{}{}
	}

	// Watch footer to stderr even in JSON mode
	if result.ConversationID != "" && !af.Compact {
		_, _ = fmt.Fprintf(stderr, "[conversation: %s]\n", result.ConversationID)
		if watchURL != "" {
			_, _ = fmt.Fprintf(stderr, "[watch: %s]\n", watchURL)
		}
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// dryRunOutput prints the resolved request payload to stdout without making the /chat call.
func dryRunOutput(cmd *cobra.Command, c *client.Client, reqBody chatRequest, stream bool) error {
	if stream {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "--dry-run and --stream cannot be combined")
		return &ExitError{Code: ExitUsageError}
	}

	out := map[string]interface{}{
		"endpoint": "POST " + c.BaseURL + "/chat",
		"headers": map[string]string{
			"X-Archivist-CLI-Version": c.Version,
			"X-Archivist-Origin":      c.Origin,
		},
		"body": reqBody,
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// readStdinParams reads a JSON object from stdin with a 5-second timeout.
func readStdinParams(cmd *cobra.Command) (*stdinParamsPayload, error) {
	type result struct {
		payload *stdinParamsPayload
		err     error
	}
	ch := make(chan result, 1)
	go func() {
		var p stdinParamsPayload
		dec := json.NewDecoder(os.Stdin)
		if err := dec.Decode(&p); err != nil {
			ch <- result{nil, fmt.Errorf("could not decode stdin JSON: %w", err)}
			return
		}
		ch <- result{&p, nil}
	}()

	select {
	case r := <-ch:
		return r.payload, r.err
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("--stdin set but no data on stdin (timed out after 5s)")
	}
}

// writeJSONError writes a JSON error envelope to w.
// surfaceChatHTTPError reads a non-2xx /chat response carrying the
// standardized {error, code, suggestion} envelope and maps it to the CLI's
// typed exit codes: 400/422 usage (2), 404 not-found (3), 429 rate-limit (7),
// anything else generic (1).
func surfaceChatHTTPError(cmd *cobra.Command, resp *http.Response, format string) error {
	var envelope struct {
		Error      string `json:"error"`
		Code       string `json:"code"`
		Suggestion string `json:"suggestion"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	_ = json.Unmarshal(body, &envelope)

	msg := envelope.Error
	if msg == "" {
		msg = fmt.Sprintf("server returned HTTP %d", resp.StatusCode)
	}
	code := envelope.Code
	if code == "" {
		code = "HTTP_ERROR"
	}

	exitCode := ExitGenericError
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		exitCode = ExitUsageError
	case http.StatusNotFound:
		exitCode = ExitNotFound
	case http.StatusTooManyRequests:
		exitCode = ExitRateLimit
	}

	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "archivist chat: %s [%s]\n", msg, code)
	if envelope.Suggestion != "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), envelope.Suggestion)
	}
	if format == "json" {
		writeJSONError(cmd.OutOrStdout(), code, msg, exitCode)
	}
	return &ExitError{Code: exitCode}
}

func writeJSONError(w io.Writer, code, message string, exitCode int) {
	out := map[string]interface{}{
		"error":     code,
		"message":   message,
		"exit_code": exitCode,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// newUUID generates a random UUID v4 hex string.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:]),
	), nil
}

// parseCSV splits a comma-separated string into a non-empty string slice.
func parseCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			result = append(result, t)
		}
	}
	return result
}

// isTerminal returns true if w is an *os.File wrapping a real TTY.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// toolProgressMessage returns a human-friendly progress line for a tool call.
func toolProgressMessage(name string, input map[string]interface{}) string {
	switch name {
	case "search_filings":
		if q, ok := input["query"].(string); ok && q != "" {
			return "Searching filings: " + q
		}
		return "Searching filings"
	case "read_filing":
		if id, ok := input["filing_id"].(string); ok && id != "" {
			return "Reading filing " + id
		}
		return "Reading filing"
	default:
		return name
	}
}

// isOlderSemver returns true if current < minimum (simple semver comparison).
func isOlderSemver(current, minimum string) bool {
	cur := parseSemver(current)
	min := parseSemver(minimum)
	if cur == nil || min == nil {
		return false
	}
	for i := range cur {
		if cur[i] < min[i] {
			return true
		}
		if cur[i] > min[i] {
			return false
		}
	}
	return false
}

// parseSemver parses vX.Y.Z or X.Y.Z into [3]int. Returns nil on failure.
func parseSemver(v string) []int {
	if len(v) > 0 && v[0] == 'v' {
		v = v[1:]
	}
	var major, minor, patch int
	_, err := fmt.Sscanf(v, "%d.%d.%d", &major, &minor, &patch)
	if err != nil {
		return nil
	}
	return []int{major, minor, patch}
}

// --- Request/response types ---

type chatRequest struct {
	Messages          []chatMessage     `json:"messages"`
	ConversationID    string            `json:"conversationId,omitempty"`
	Model             string            `json:"model,omitempty"`
	AttachedFilingIDs []string          `json:"attachedFilingIds,omitempty"`
	StructuredParams  *structuredParams `json:"structuredParams,omitempty"`
}

type chatMessage struct {
	ID    string        `json:"id"`
	Role  string        `json:"role"`
	Parts []interface{} `json:"parts"`
}

type structuredParams struct {
	Company       string   `json:"company,omitempty"`
	CompanySymbol string   `json:"companySymbol,omitempty"`
	FilingType    []string `json:"filingType,omitempty"`
	DateFrom      string   `json:"dateFrom,omitempty"`
	DateTo        string   `json:"dateTo,omitempty"`
}

type companyCandidate struct {
	IssuerKey   string `json:"issuer_key"`
	CompanyName string `json:"company_name"`
	Ticker      string `json:"ticker,omitempty"`
	Exchange    string `json:"exchange,omitempty"`
	Country     string `json:"country,omitempty"`
}

type chatResult struct {
	Markdown        string
	Citations       []chatCitation
	ConversationID  string
	TaskID          string
	AppliedDefaults map[string]interface{}
	// toolNameByCallID maps SSE toolCallId → toolName so tool-output-available
	// events (which carry only the callId) can be routed back to the matching
	// toolName-aware handler (e.g. citation parsing for search_filings).
	toolNameByCallID map[string]string
}

type chatCitation struct {
	Number    int    `json:"number"`
	Ticker    string `json:"ticker"`
	FormType  string `json:"formtype"`
	DateFiled string `json:"datefiled"`
	Section   string `json:"section,omitempty"`
	Page      int    `json:"page,omitempty"`
	URL       string `json:"url,omitempty"`
}

// citationSource matches the per-source shape emitted by chat-api's
// search_filings tool. Field tags align with the production wire format
// (symbol/formtype/datefiled — no underscores).
type citationSource struct {
	SourceNumber int    `json:"source_number"`
	Ticker       string `json:"symbol"`
	FormType     string `json:"formtype"`
	DateFiled    string `json:"datefiled"`
	Section      string `json:"section_header"`
	Page         int    `json:"page"`
	URL          string `json:"url"`
}

// sseEvent is a generic SSE event payload from the Vercel AI SDK v6
// UI Message Stream (chat-api sets header `x-vercel-ai-ui-message-stream: v1`).
//
// Key types seen on the wire:
//   - text-start / text-delta / text-end (delta carries the chunk text)
//   - tool-input-start / tool-input-delta / tool-input-available
//   - tool-output-available (output carries the result payload)
//   - data-status (transient progress messages with {event, tool, summary})
//   - start-step / finish-step
//
// ConversationID / AppliedDefaults are not currently emitted by chat-api;
// the structs are kept here for forward-compat if/when the server adds them.
type sseEvent struct {
	Type            string                 `json:"type"`
	TextDelta       string                 `json:"delta"`
	ToolCallID      string                 `json:"toolCallId"`
	ToolName        string                 `json:"toolName"`
	ToolInput       map[string]interface{} `json:"input"`
	Output          interface{}            `json:"output"`
	Data            map[string]interface{} `json:"data"`
	ConversationID  string                 `json:"conversationId"`
	TaskID          string                 `json:"taskId"`
	AppliedDefaults map[string]interface{} `json:"appliedDefaults"`
}

// stdinParamsPayload is the JSON structure read from stdin when --stdin is set.
type stdinParamsPayload struct {
	Company    string   `json:"company"`
	DateFrom   string   `json:"dateFrom"`
	DateTo     string   `json:"dateTo"`
	FilingType []string `json:"filingType"`
}
