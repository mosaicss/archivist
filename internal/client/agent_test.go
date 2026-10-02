package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mosaicss/archivist/internal/client"
)

// A retried POST must resend the same body; the first attempt used to
// consume the reader and the retry sent nothing.
func TestRetryPolicy_POSTRetryResendsBody(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	resp, err := c.Do(context.Background(), http.MethodPost, "/relay-tickets", strings.NewReader(`{"scope":"user"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = resp.Body.Close()
	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[1] != `{"scope":"user"}` {
		t.Fatalf("attempt bodies = %q", bodies)
	}
}

func TestAgentRoutes(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+string(b)+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /relay-tickets":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ticket":"a.b","expiresAt":123,"scope":"user"}`))
		case "POST /task-tokens":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"mst_x","tokenId":"id-1","expiresAt":9,"scopes":["search","read"]}`))
		case "DELETE /task-tokens/id-1":
			w.WriteHeader(http.StatusNoContent)
		case "GET /agent-sessions/s-1":
			_, _ = w.Write([]byte(`{"sessionId":"s-1","ownerId":"user_a","agent":"claude","title":"t","status":"active","createdAt":1,"expiresAt":2,"endedAt":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"The requested endpoint does not exist.","code":"FEATURE_DISABLED"}`))
		}
	}))
	defer srv.Close()
	c := client.New("ak_owner_key_123", "0.2.0")
	c.BaseURL = srv.URL
	ctx := context.Background()

	ticket, err := c.MintRelayTicket(ctx, "")
	if err != nil || ticket.Ticket != "a.b" || ticket.ExpiresAt != 123 {
		t.Fatalf("MintRelayTicket: %+v %v", ticket, err)
	}
	tok, err := c.MintTaskToken(ctx, "s-1", []string{"search", "read"}, 900)
	if err != nil || tok.TokenID != "id-1" {
		t.Fatalf("MintTaskToken: %+v %v", tok, err)
	}
	if err := c.RevokeTaskToken(ctx, "id-1"); err != nil {
		t.Fatalf("RevokeTaskToken: %v", err)
	}
	sess, err := c.GetAgentSession(ctx, "s-1")
	if err != nil || sess.Status != "active" || sess.EndedAt != nil {
		t.Fatalf("GetAgentSession: %+v %v", sess, err)
	}
	_, err = c.GetAgentSession(ctx, "missing")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Code != "FEATURE_DISABLED" {
		t.Fatalf("expected FEATURE_DISABLED APIError, got %v", err)
	}

	var body map[string]any
	parts := strings.SplitN(seen[1], " ", 4)
	if err := json.Unmarshal([]byte(parts[2]), &body); err != nil || body["ttlSeconds"] != float64(900) {
		t.Fatalf("task token body %q", seen[1])
	}
	for _, s := range seen {
		if !strings.HasSuffix(s, "Bearer ak_owner_key_123") {
			t.Fatalf("missing owner bearer: %q", s)
		}
	}
}
