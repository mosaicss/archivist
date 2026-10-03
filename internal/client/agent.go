package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
)

// Story 78.15 chat-api agent session routes. Every mint call needs the
// owner's verified ak_ parent key; chat-api refuses mc_pat_ and task tokens.

// RelayTicket is POST /relay-tickets' 201 body. ExpiresAt is in milliseconds.
type RelayTicket struct {
	Ticket    string `json:"ticket"`
	ExpiresAt int64  `json:"expiresAt"`
	Scope     string `json:"scope"`
}

// TaskToken is POST /task-tokens' 201 body. Token is the one-time bearer.
type TaskToken struct {
	Token     string   `json:"token"`
	TokenID   string   `json:"tokenId"`
	ExpiresAt int64    `json:"expiresAt"`
	Scopes    []string `json:"scopes"`
}

// AgentSession is the owner's session record (GET /agent-sessions/:id).
type AgentSession struct {
	SessionID string `json:"sessionId"`
	OwnerID   string `json:"ownerId"`
	Agent     string `json:"agent"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	EndedAt   *int64 `json:"endedAt"`
}

// MintRelayTicket mints a relay ticket: user scope when sessionID is empty,
// otherwise scoped to that owned active session.
func (c *Client) MintRelayTicket(ctx context.Context, sessionID string) (*RelayTicket, error) {
	body := map[string]string{"scope": "user"}
	if sessionID != "" {
		body = map[string]string{"scope": "session", "sessionId": sessionID}
	}
	var out RelayTicket
	if err := c.doJSON(ctx, http.MethodPost, "/relay-tickets", body, http.StatusCreated, &out); err != nil {
		return nil, err
	}
	if out.Ticket == "" || out.ExpiresAt <= 0 {
		return nil, fmt.Errorf("relay ticket response is incomplete")
	}
	return &out, nil
}

// MintTaskToken mints a session task token with the given scopes and TTL.
func (c *Client) MintTaskToken(ctx context.Context, sessionID string, scopes []string, ttlSeconds int) (*TaskToken, error) {
	body := map[string]any{"sessionId": sessionID, "scopes": scopes, "ttlSeconds": ttlSeconds}
	var out TaskToken
	if err := c.doJSON(ctx, http.MethodPost, "/task-tokens", body, http.StatusCreated, &out); err != nil {
		return nil, err
	}
	if out.Token == "" || out.TokenID == "" || out.ExpiresAt <= 0 {
		return nil, fmt.Errorf("task token response is incomplete")
	}
	return &out, nil
}

// RevokeTaskToken revokes a task token by id (idempotent server side).
func (c *Client) RevokeTaskToken(ctx context.Context, tokenID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/task-tokens/"+url.PathEscape(tokenID), nil, http.StatusNoContent, nil)
}

// GetAgentSession reads one owned session record.
func (c *Client) GetAgentSession(ctx context.Context, sessionID string) (*AgentSession, error) {
	var out AgentSession
	if err := c.doJSON(ctx, http.MethodGet, "/agent-sessions/"+url.PathEscape(sessionID), nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Artifact is POST /artifacts' 201 body (Story 78.18).
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	OwnerID    string `json:"ownerId"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	MediaType  string `json:"mediaType"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	CreatedAt  int64  `json:"createdAt"`
}

// PublishArtifact uploads one session artifact (POST /artifacts) as
// multipart/form-data with exactly two fields: sessionId and file. The file
// part carries name and mediaType as its Content-Type. It makes exactly one
// attempt: a network error, 429 or 5xx is returned, never retried.
func (c *Client) PublishArtifact(ctx context.Context, sessionID, name, mediaType string, data []byte) (*Artifact, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("sessionId", sessionID); err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition("file", name))
	h.Set("Content-Type", mediaType)
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	// No retry: a retried POST /artifacts could publish a duplicate artifact.
	resp, err := c.doAs(ctx, http.MethodPost, "/artifacts", &body, w.FormDataContentType(), false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return nil, ParseAPIError(resp)
	}
	var out Artifact
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode POST /artifacts response: %w", err)
	}
	if out.ArtifactID == "" || out.Digest == "" {
		return nil, fmt.Errorf("artifact response is incomplete")
	}
	return &out, nil
}

// doJSON sends an optional JSON body and decodes a JSON response. A status
// other than want is returned as *APIError (401 included); Do's typed
// failures (*ExitCodeError, network errors) pass through.
func (c *Client) doJSON(ctx context.Context, method, path string, in any, want int, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(raw)
	}
	resp, err := c.Do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		return ParseAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}
