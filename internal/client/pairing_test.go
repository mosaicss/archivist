package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mosaicss/archivist/internal/client"
)

func TestNormalizePairingCode(t *testing.T) {
	ok := map[string]string{
		"ABCDE-FGHJK":         "ABCDEFGHJK",
		"abcde fghjk":         "ABCDEFGHJK",
		" abcde-\tfghjk ":     "ABCDEFGHJK",
		"0123456789":          "0123456789",
		"oIlL0-ab2cd":         "0111" + "0AB2CD",
		"a-b-c-d-e-f-g-h-j-k": "ABCDEFGHJK",
	}
	for in, want := range ok {
		got, err := client.NormalizePairingCode(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ABCDE", "ABCDE-FGHJKM", "ABCDE-FGHJU", "ABCDE_FGHJK", "ÄBCDE-FGHJK", "ABCDE-FGHJK-0"} {
		if _, err := client.NormalizePairingCode(in); !errors.Is(err, client.ErrPairingCodeInvalid) {
			t.Errorf("%q accepted: %v", in, err)
		}
	}
}

func TestSanitizeDeviceName(t *testing.T) {
	cases := map[string]string{
		"my-mac.local":           "my-mac.local",
		"  a \n\t b\x00c  ":      "a bc",
		"":                       "device",
		"\x1b[31m":               "[31m",
		strings.Repeat("x", 100): strings.Repeat("x", 64),
	}
	for in, want := range cases {
		if got := client.SanitizeDeviceName(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestRedeemPairingSendsNoCredentialAndNoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/agent-pairing/redeem" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization sent: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" || !strings.HasPrefix(r.Header.Get("User-Agent"), "archivist-cli/v1.2.3 (") {
			t.Errorf("headers %v", r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		if len(b) >= 1024 {
			t.Errorf("body %d bytes", len(b))
		}
		var body map[string]string
		if err := json.Unmarshal(b, &body); err != nil || body["code"] != "ABCDEFGHJK" || body["device_name"] != "box" || len(body) != 2 {
			t.Errorf("body %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"ak_secretsecretsecret","key_id":"ak_id","name":"archivist box 2026-10-04 10:00Z ab12","tier":"pro"}`))
	}))
	defer srv.Close()
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	c := client.New("ak_should_not_be_sent_00000", "v1.2.3")
	res, err := c.RedeemPairing(context.Background(), "ABCDEFGHJK", "box")
	if err != nil || res.Key != "ak_secretsecretsecret" || res.KeyID != "ak_id" || res.Tier != "pro" {
		t.Fatalf("%+v %v", res, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits %d", hits.Load())
	}
}

func TestRedeemPairingErrors(t *testing.T) {
	cases := []struct {
		status  int
		body    string
		invalid bool
	}{
		{400, `{"error":"This pairing code is not valid.","code":"PAIRING_CODE_INVALID"}`, true},
		{403, `{"error":"This requires a Mosaic Pro account.","code":"PRO_REQUIRED","account_url":"https://x"}`, false},
		{404, `{"error":"Not found","code":"FEATURE_DISABLED"}`, false},
		{429, `{"error":"Too many","code":"RATE_LIMITED"}`, false},
		{502, `{"error":"Failed","code":"CLERK_ERROR"}`, false},
		{503, `not json`, false},
	}
	for _, tc := range cases {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		_, err := client.New("", "dev").RedeemPairing(context.Background(), "ABCDEFGHJK", "box")
		srv.Close()
		if hits.Load() != 1 {
			t.Errorf("%d: %d attempts, want exactly 1", tc.status, hits.Load())
		}
		if tc.invalid {
			if !errors.Is(err, client.ErrPairingCodeInvalid) {
				t.Errorf("%d: %v", tc.status, err)
			}
			continue
		}
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
			t.Errorf("%d: %v", tc.status, err)
		}
	}
	// An incomplete 200 is an error, not a saved credential.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"key":"nope","key_id":""}`))
	}))
	defer srv.Close()
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	if _, err := client.New("", "dev").RedeemPairing(context.Background(), "ABCDEFGHJK", "box"); err == nil {
		t.Fatal("incomplete response accepted")
	}
}
