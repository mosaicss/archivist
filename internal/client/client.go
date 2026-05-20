package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const defaultBaseURL = "https://chat-api-685186721186.us-central1.run.app"

// Client is the shared HTTP client for chat-api calls.
type Client struct {
	baseURL    string
	token      string
	version    string
	origin     string
	httpClient *http.Client
}

// New returns a Client configured from the environment and provided token.
// version is the binary version string (from ldflags).
func New(token, version string) *Client {
	baseURL := os.Getenv("ARCHIVIST_BASE_URL")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	origin := os.Getenv("ARCHIVIST_ORIGIN")
	if origin == "" {
		origin = "manual"
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		version: version,
		origin:  origin,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Do executes an authenticated request to the chat-api.
// It injects Authorization, X-Archivist-CLI-Version, and X-Archivist-Origin headers.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Archivist-CLI-Version", c.version)
	req.Header.Set("X-Archivist-Origin", c.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.httpClient.Do(req)
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
