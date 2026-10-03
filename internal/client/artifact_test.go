package client_test

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mosaicss/archivist/internal/client"
)

// PublishArtifact sends multipart/form-data with exactly sessionId then
// file, the file part carrying its name and exact media type (Story 78.18).
func TestPublishArtifactMultipart(t *testing.T) {
	type part struct{ name, filename, ctype, body string }
	var parts []part
	var auth, method, path, reqType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, method, path, reqType = r.Header.Get("Authorization"), r.Method, r.URL.Path, r.Header.Get("Content-Type")
		mt, params, err := mime.ParseMediaType(reqType)
		if err != nil || mt != "multipart/form-data" {
			w.WriteHeader(400)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(400)
				return
			}
			b, _ := io.ReadAll(p)
			parts = append(parts, part{p.FormName(), p.FileName(), p.Header.Get("Content-Type"), string(b)})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"artifactId":"a1","ownerId":"user_1","sessionId":"s1","name":"q \"3\".md","mediaType":"text/markdown","size":7,"digest":"` +
			strings.Repeat("ab", 32) + `","createdAt":1}`))
	}))
	defer srv.Close()

	c := client.New("mst_x", "test")
	c.BaseURL = srv.URL
	out, err := c.PublishArtifact(context.Background(), "s1", `q "3".md`, "text/markdown", []byte("# hello"))
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/artifacts" || auth != "Bearer mst_x" || !strings.HasPrefix(reqType, "multipart/form-data; boundary=") {
		t.Fatalf("request %s %s auth=%q type=%q", method, path, auth, reqType)
	}
	if len(parts) != 2 || parts[0] != (part{"sessionId", "", "", "s1"}) ||
		parts[1] != (part{"file", `q "3".md`, "text/markdown", "# hello"}) {
		t.Fatalf("parts %+v", parts)
	}
	if out.ArtifactID != "a1" || out.Size != 7 || out.MediaType != "text/markdown" || out.SessionID != "s1" {
		t.Fatalf("artifact %+v", out)
	}
}

func TestPublishArtifactErrors(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		apiErr bool
	}{
		{403, `{"error":"This task token does not allow this operation.","code":"TASK_SCOPE_REQUIRED"}`, true},
		{415, `{"error":"Unsupported artifact type.","code":"ARTIFACT_TYPE_INVALID"}`, true},
		{201, `{"artifactId":""}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		cl := client.New("mst_x", "test")
		cl.BaseURL = srv.URL
		_, err := cl.PublishArtifact(context.Background(), "s1", "a.txt", "text/plain", []byte("x"))
		srv.Close()
		var apiErr *client.APIError
		if err == nil || errors.As(err, &apiErr) != c.apiErr || (c.apiErr && apiErr.Status != c.status) {
			t.Fatalf("status %d: err %v", c.status, err)
		}
	}
}

// PublishArtifact makes exactly one attempt: a network error, 503 or 429 is
// returned, never retried, so a retry cannot publish a duplicate artifact.
func TestPublishArtifactNeverRetries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler func(w http.ResponseWriter)
	}{
		{"network error", func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}},
		{"503", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }},
		{"429", func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				tc.handler(w)
			}))
			defer srv.Close()
			cl := client.New("mst_x", "test")
			cl.BaseURL = srv.URL
			_, err := cl.PublishArtifact(context.Background(), "s1", "a.txt", "text/plain", []byte("x"))
			if err == nil {
				t.Fatal("expected an error")
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("expected exactly 1 attempt, got %d", n)
			}
		})
	}
}
