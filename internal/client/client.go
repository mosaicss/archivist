package client

import (
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
	baseURL := os.Getenv("ARCHIVIST_BASE_URL")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
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
				return nil, &ExitCodeError{Code: 5, Message: "server requires newer CLI version"}
			}
		}

		// Emit quota info per AC7 spec.
		if remaining := resp.Header.Get("X-Queries-Remaining"); remaining != "" {
			n, parseErr := strconv.Atoi(remaining)
			if parseErr == nil {
				switch {
				case n == 0:
					_, _ = fmt.Fprintln(c.stderr, "[archivist] Error: monthly query limit reached. Run 'archivist usage' for details.")
					_ = resp.Body.Close()
					return nil, &ExitCodeError{Code: 7, Message: "monthly query limit reached"}
				case n < 5 && !c.quiet:
					_, _ = fmt.Fprintf(c.stderr,
						"[archivist] Warning: %d queries remaining this month. Run 'archivist usage' for details.\n", n)
				default:
					if !c.quiet {
						_, _ = fmt.Fprintf(c.stderr, "[quota] %s queries remaining\n", remaining)
					}
				}
			}
		}

		status := resp.StatusCode

		// 401 — do not retry
		if status == http.StatusUnauthorized {
			return resp, nil
		}

		// 429 — honor Retry-After, then retry (GET: up to 3, POST: up to 1)
		if status == http.StatusTooManyRequests {
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			_ = resp.Body.Close()
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
			return nil, &ExitCodeError{Code: 7, Message: fmt.Sprintf("rate limit exceeded. Try again in %v", retryAfter)}
		}

		// 5xx — for POST only retry 503; for GET retry any 5xx
		if status >= 500 {
			_ = resp.Body.Close()
			if attempt < maxRetries {
				if !isGet && status != http.StatusServiceUnavailable {
					// POST: only retry 503
					return nil, &ExitCodeError{Code: 5, Message: fmt.Sprintf("server error (HTTP %d). Try again later.", status)}
				}
				lastResp = nil
				lastErr = fmt.Errorf("server error %d", status)
				continue
			}
			return nil, &ExitCodeError{Code: 5, Message: fmt.Sprintf("server error (HTTP %d). Try again later.", status)}
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
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
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
}

func (e *ExitCodeError) Error() string {
	return e.Message
}

// ErrUnauthorized is returned when the server rejects the credential.
var ErrUnauthorized = fmt.Errorf("token invalid or revoked. Re-issue at https://mosaic-finance.com/account/cli-tokens")

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
