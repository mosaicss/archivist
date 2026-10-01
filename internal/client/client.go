package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"
)

// DefaultBaseURL is the production chat-api endpoint. Overrideable via ARCHIVIST_BASE_URL.
const DefaultBaseURL = "https://chat-api-685186721186.us-central1.run.app"

// AccountURL is the one Mosaic web page the binary prints itself: plans and
// account access. Every other Mosaic URL it prints is a server-supplied
// permalink.
const AccountURL = "https://mosaic-finance.com/en/pricing/"

// maxErrorBody caps how much of an error response body is read.
const maxErrorBody = 64 * 1024

// ResolveBaseURL returns ARCHIVIST_BASE_URL when set, else DefaultBaseURL.
func ResolveBaseURL() string {
	if baseURL := os.Getenv("ARCHIVIST_BASE_URL"); baseURL != "" {
		return baseURL
	}
	return DefaultBaseURL
}

// Client is the shared HTTP client for chat-api calls.
type Client struct {
	BaseURL    string
	Token      string
	Version    string
	Origin     string
	OS         string
	Arch       string
	httpClient *http.Client
	stderr     io.Writer
	quiet      bool
}

// New returns a Client configured from the environment and provided token.
// version is the binary version string (from ldflags).
func New(token, version string) *Client {
	baseURL := ResolveBaseURL()
	origin := os.Getenv("ARCHIVIST_ORIGIN")
	if origin == "" {
		origin = "manual"
	}
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		Version: version,
		Origin:  origin,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		stderr: os.Stderr,
	}
}

// SetQuiet controls whether quota/progress lines are suppressed.
func (c *Client) SetQuiet(q bool) {
	c.quiet = q
}

// SetStderr redirects diagnostic output (for testing).
func (c *Client) SetStderr(w io.Writer) {
	c.stderr = w
}

// Do executes an authenticated request to the chat-api, applying retry logic.
// method controls whether GET (3 retries on 5xx/429/network) or POST (1 retry on 503/network) policy applies.
//
// A 429 whose body code is CLI_QUOTA (the monthly fair use ceiling) is never
// retried: Do returns that response with its body intact so the caller maps
// it. Any other 429 is retried after Retry-After (clamped to 30 s) and ends in
// an *ExitCodeError with code 7. A non-2xx response other than 429 and 5xx is
// returned to the caller unread.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	url := c.BaseURL + path

	isGet := method == http.MethodGet
	maxRetries := 1
	if isGet {
		maxRetries = 3
	}

	backoffs := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, 1000 * time.Millisecond}

	var lastErr error
	var lastResp *http.Response

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffWithJitter(backoffs[attempt-1])
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, url, body)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		c.injectHeaders(req)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("archivist: network error: %w", err)
			// POST: only 1 retry on network error; GET: 3 retries
			continue
		}

		// Handle version block — applies regardless of retry policy
		if min := resp.Header.Get("X-Archivist-Min-CLI-Version"); min != "" {
			if isOlderVersion(c.Version, min) {
				_, _ = fmt.Fprintf(c.stderr,
					"Server requires archivist-cli >= %s. You have %s. Run 'archivist update' to upgrade.\n",
					min, c.Version)
				_ = resp.Body.Close()
				return nil, &ExitCodeError{
					Code:     5,
					Message:  "server requires newer CLI version",
					APICode:  "MIN_CLI_VERSION",
					Reported: true,
				}
			}
		}

		// Quota info for positive values. X-Queries-Remaining: 0 says nothing
		// on its own: a successful last allowed call carries it, and so does
		// the 429 CLI_QUOTA refusal, which is mapped from its body below.
		if remaining := resp.Header.Get("X-Queries-Remaining"); remaining != "" {
			n, parseErr := strconv.Atoi(remaining)
			if parseErr == nil && n > 0 && !c.quiet {
				if n < 5 {
					_, _ = fmt.Fprintf(c.stderr,
						"[archivist] Warning: %d queries remaining this month. Run 'archivist usage' for details.\n", n)
				} else {
					_, _ = fmt.Fprintf(c.stderr, "[quota] %s queries remaining\n", remaining)
				}
			}
		}

		status := resp.StatusCode

		// 401 — do not retry
		if status == http.StatusUnauthorized {
			return resp, nil
		}

		// 429 — CLI_QUOTA returns at once; any other 429 honors Retry-After,
		// then retries (GET: up to 3, POST: up to 1).
		if status == http.StatusTooManyRequests {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			_ = resp.Body.Close()
			apiErr := parseAPIErrorBody(status, errBody)
			if apiErr.Code == "CLI_QUOTA" {
				resp.Body = io.NopCloser(bytes.NewReader(errBody))
				return resp, nil
			}
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			if attempt < maxRetries {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(retryAfter):
				}
				lastResp = nil
				lastErr = fmt.Errorf("rate limited (429)")
				continue
			}
			code := apiErr.Code
			if code == "" {
				code = "RATE_LIMITED"
			}
			return nil, &ExitCodeError{
				Code:       7,
				Message:    fmt.Sprintf("rate limit exceeded. Try again in %v", retryAfter),
				APICode:    code,
				Suggestion: apiErr.Suggestion,
			}
		}

		// 5xx — for POST only retry 503; for GET retry any 5xx
		if status >= 500 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			_ = resp.Body.Close()
			if attempt < maxRetries && (isGet || status == http.StatusServiceUnavailable) {
				lastResp = nil
				lastErr = fmt.Errorf("server error %d", status)
				continue
			}
			return nil, serverError(parseAPIErrorBody(status, errBody))
		}

		// Success or 4xx (don't retry 4xx other than 429)
		lastResp = resp
		lastErr = nil
		break
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return lastResp, nil
}

// injectHeaders sets all required headers on the request.
func (c *Client) injectHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("X-Archivist-CLI-Version", c.Version)
	req.Header.Set("X-Archivist-Origin", c.Origin)
	req.Header.Set("User-Agent", fmt.Sprintf("archivist-cli/%s (%s/%s)", c.Version, c.OS, c.Arch))
	if req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
}

// GetCLITokens calls GET /account/cli-tokens and returns the parsed response.
// A 401 is ErrUnauthorized; any other non-200 is a *APIError; a Do error is
// wrapped with %w.
func (c *Client) GetCLITokens(ctx context.Context) (*CLITokensResponse, error) {
	resp, err := c.Do(ctx, http.MethodGet, "/account/cli-tokens", nil)
	if err != nil {
		return nil, fmt.Errorf("get cli-tokens: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}

	var result CLITokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// backoffWithJitter adds ±25% jitter to the given base duration.
func backoffWithJitter(base time.Duration) time.Duration {
	jitter := float64(base) * 0.25
	delta := time.Duration(rand.Int63n(int64(jitter*2+1)) - int64(jitter))
	return base + delta
}

// parseRetryAfter parses a Retry-After header value (seconds integer).
// Returns clamped value ≤30s. Falls back to 5s if unparseable.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 5 * time.Second
	}
	secs, err := strconv.Atoi(header)
	if err != nil || secs <= 0 {
		return 5 * time.Second
	}
	d := time.Duration(secs) * time.Second
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

// isOlderVersion returns true if current < minimum using simple string semver prefix.
// For proper comparison we compare major.minor.patch as integers.
// Returns false (don't block) if either version is unparseable.
func isOlderVersion(current, minimum string) bool {
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

// ExitCodeError is an error that carries a desired exit code.
type ExitCodeError struct {
	Code    int
	Message string
	// APICode is the server error code, or a client-side one such as
	// MIN_CLI_VERSION or RATE_LIMITED, for the JSON error envelope.
	APICode string
	// Suggestion is the server's suggestion, when the response carried one.
	Suggestion string
	// Reported is true when Do already wrote the message to stderr.
	Reported bool
}

// serverError shapes a final 5xx as an exit 5 failure carrying the server's
// own error text when it sent one.
func serverError(apiErr *APIError) *ExitCodeError {
	msg := fmt.Sprintf("server error (HTTP %d). Try again later.", apiErr.Status)
	if apiErr.hasBody {
		msg = fmt.Sprintf("%s (HTTP %d). Try again later.", apiErr.Message, apiErr.Status)
	}
	code := apiErr.Code
	if code == "" {
		code = "SERVER_ERROR"
	}
	return &ExitCodeError{Code: 5, Message: msg, APICode: code, Suggestion: apiErr.Suggestion}
}

// APIError is a non-2xx chat-api response decoded from its JSON error body
// ({error, code, suggestion, account_url, reset_date}). Fields the body did
// not carry are empty; Message falls back to the HTTP status.
type APIError struct {
	Status     int
	Code       string
	Message    string
	Suggestion string
	AccountURL string
	ResetDate  string
	// hasBody is true when the body carried an error message.
	hasBody bool
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s [%s]", e.Message, e.Code)
	}
	return e.Message
}

// ParseAPIError reads at most 64 KiB of resp's body and decodes the error
// envelope. A body that is not JSON leaves the fields empty. The caller
// still closes resp.Body.
func ParseAPIError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return parseAPIErrorBody(resp.StatusCode, body)
}

func parseAPIErrorBody(status int, body []byte) *APIError {
	var envelope struct {
		Error      any    `json:"error"`
		Code       string `json:"code"`
		Suggestion string `json:"suggestion"`
		AccountURL string `json:"account_url"`
		ResetDate  string `json:"reset_date"`
	}
	_ = json.Unmarshal(body, &envelope)
	e := &APIError{
		Status:     status,
		Code:       envelope.Code,
		Suggestion: envelope.Suggestion,
		AccountURL: envelope.AccountURL,
		ResetDate:  envelope.ResetDate,
	}
	if msg, ok := envelope.Error.(string); ok && msg != "" {
		e.Message = msg
		e.hasBody = true
	} else {
		e.Message = fmt.Sprintf("server returned HTTP %d", status)
	}
	return e
}

func (e *ExitCodeError) Error() string {
	return e.Message
}

// ErrUnauthorized is returned when the server rejects the credential.
var ErrUnauthorized = fmt.Errorf("token invalid or revoked. Create a new key via the avatar menu (Manage account → API keys) at %s", AccountURL)

// CLITokensResponse is the shape returned by GET /account/cli-tokens.
type CLITokensResponse struct {
	UserEmail string     `json:"user_email"`
	Tier      string     `json:"tier"`
	Tokens    []APIToken `json:"tokens"`
}

// APIToken represents a single Clerk API Key entry.
type APIToken struct {
	KeyID      string  `json:"key_id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}
