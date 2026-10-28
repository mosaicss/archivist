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

// The redeem always asks for the browser confirmation (Story 94.15); a 200
// with a key (a server older than Story 94.3) still decodes, so the caller
// can revoke it.
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
		if err := json.Unmarshal(b, &body); err != nil || body["code"] != "ABCDEFGHJK" || body["device_name"] != "box" || body["confirm"] != "browser" || len(body) != 3 {
			t.Errorf("body %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key":"ak_secretsecretsecret","key_id":"ak_id","name":"archivist box 2026-10-04 10:00Z ab12","tier":"pro"}`))
	}))
	defer srv.Close()
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	c := client.New("ak_should_not_be_sent_00000", "v1.2.3")
	res, pending, err := c.RedeemPairing(context.Background(), "ABCDEFGHJK", "box", client.ConfirmBrowser)
	if err != nil || pending != nil || res.Key != "ak_secretsecretsecret" || res.KeyID != "ak_id" || res.Tier != "pro" {
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
		{426, `{"error":"This version of Archivist can no longer pair.","code":"PAIRING_UPDATE_REQUIRED"}`, false},
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
		_, _, err := client.New("", "dev").RedeemPairing(context.Background(), "ABCDEFGHJK", "box", client.ConfirmBrowser)
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
	if _, _, err := client.New("", "dev").RedeemPairing(context.Background(), "ABCDEFGHJK", "box", client.ConfirmBrowser); err == nil {
		t.Fatal("incomplete response accepted")
	}
}

// Story 94.3: the redeem body names the account; a body without the field
// (an older server) still decodes, with Email empty.
func TestRedeemPairingDecodesEmailAndOlderBody(t *testing.T) {
	for body, want := range map[string]string{
		`{"key":"ak_secretsecretsecret","key_id":"ak_id","name":"n","tier":"pro","email":"b@example.test"}`:              "b@example.test",
		`{"key":"ak_secretsecretsecret","key_id":"ak_id","name":"n","tier":"pro"}`:                                       "",
		`{"key":"ak_secretsecretsecret","key_id":"ak_id","name":"n","tier":"pro","email":"b@example.test","extra":true}`: "b@example.test",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		res, _, err := client.New("", "dev").RedeemPairing(context.Background(), "ABCDEFGHJK", "box", client.ConfirmBrowser)
		srv.Close()
		if err != nil || res.Email != want || res.Key != "ak_secretsecretsecret" || res.KeyID != "ak_id" || res.Tier != "pro" {
			t.Errorf("%s: %+v %v", body, res, err)
		}
	}
}

func TestRevokeCurrentKey(t *testing.T) {
	cases := []struct {
		status       int
		body         string
		unauthorized bool
		ok           bool
	}{
		{204, ``, false, true},
		{401, `{"error":"Invalid CLI token","code":"INVALID_TOKEN"}`, true, false},
		{403, `{"error":"This needs an Archivist CLI key.","code":"CLI_KEY_REQUIRED"}`, false, false},
		{404, `{"error":"Not found","code":"NOT_FOUND"}`, false, false},
	}
	for _, tc := range cases {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if r.Method != http.MethodDelete || r.URL.Path != "/account/cli-tokens/current" || r.URL.RawQuery != "" {
				t.Errorf("unexpected %s %s", r.Method, r.URL)
			}
			if r.Header.Get("Authorization") != "Bearer ak_selfrevoke_secret_0000" {
				t.Errorf("Authorization %q", r.Header.Get("Authorization"))
			}
			if b, _ := io.ReadAll(r.Body); len(b) != 0 {
				t.Errorf("body %q", b)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		err := client.New("ak_selfrevoke_secret_0000", "dev").RevokeCurrentKey(context.Background())
		srv.Close()
		switch {
		case tc.ok:
			if err != nil {
				t.Errorf("%d: %v", tc.status, err)
			}
		case tc.unauthorized:
			if !errors.Is(err, client.ErrUnauthorized) {
				t.Errorf("%d: %v", tc.status, err)
			}
		default:
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != tc.status {
				t.Errorf("%d: %v", tc.status, err)
			}
		}
		if hits.Load() != 1 {
			t.Errorf("%d: %d attempts", tc.status, hits.Load())
		}
	}
}

const (
	testConfirmID = "cccccccccccccccccccccccccccccccccccccccccc_"
	testPollToken = "ppppppppppppppppppppppppppppppppppppppppp-p"
)

func pendingBody(confirmURL string) string {
	b, _ := json.Marshal(map[string]any{
		"pending": true, "email": "b@example.test", "confirm_id": testConfirmID, "poll_token": testPollToken,
		"confirm_url": confirmURL, "expires_in_seconds": 300, "poll_interval_seconds": 2,
	})
	return string(b)
}

// Story 94.3: the browser mode redeem answers 202 with a pending pairing
// and no key; its URL must be a safe https (or loopback http) confirm link.
func TestRedeemPairingBrowserPending(t *testing.T) {
	good := "https://workspace.mosaic-finance.com/connect/confirm#" + testConfirmID
	cases := map[string]bool{
		good: true,
		"http://127.0.0.1:8080/connect/confirm#" + testConfirmID:       true,
		"http://example.test/connect/confirm#" + testConfirmID:         false,
		"https://workspace.mosaic-finance.com/connect/confirm#other":   false,
		"https://workspace.mosaic-finance.com/a&b#" + testConfirmID:    false,
		"https://workspace.mosaic-finance.com/%PATH%#" + testConfirmID: false,
		"https://user@workspace.mosaic-finance.com/c#" + testConfirmID: false,
		"file:///etc/passwd#" + testConfirmID:                          false,
		"https://workspace.mosaic-finance.com/a b#" + testConfirmID:    false,
		"https://workspace.mosaic-finance.com/a\"^|#" + testConfirmID:  false,
	}
	for confirmURL, ok := range cases {
		var sent map[string]string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &sent)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(pendingBody(confirmURL)))
		}))
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		res, pending, err := client.New("", "dev").RedeemPairing(context.Background(), "ABCDEFGHJK", "box", client.ConfirmBrowser)
		srv.Close()
		if sent["confirm"] != "browser" {
			t.Errorf("%s: sent %v", confirmURL, sent)
		}
		if res != nil {
			t.Errorf("%s: a key came with a pending pairing", confirmURL)
		}
		if ok != (err == nil && pending != nil) {
			t.Errorf("%s: pending %+v err %v", confirmURL, pending, err)
			continue
		}
		if ok && (pending.ConfirmID != testConfirmID || pending.PollToken != testPollToken || pending.Email != "b@example.test" ||
			pending.ExpiresInSeconds != 300 || pending.PollIntervalSeconds != 2) {
			t.Errorf("%s: %+v", confirmURL, pending)
		}
	}
	// Short secrets are refused too.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"pending":true,"email":"b@example.test","confirm_id":"short","poll_token":"short","confirm_url":"https://w.test/connect/confirm#short","expires_in_seconds":300}`))
	}))
	defer srv.Close()
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	if _, p, err := client.New("", "dev").RedeemPairing(context.Background(), "ABCDEFGHJK", "box", client.ConfirmBrowser); err == nil || p != nil {
		t.Fatal("short secrets accepted")
	}
}

func TestPollPairing(t *testing.T) {
	pending := &client.PendingPairing{ConfirmID: testConfirmID, PollToken: testPollToken}
	cases := []struct {
		status        int
		body          string
		state, reason string
		key           bool
		invalid, fail bool
	}{
		{200, `{"state":"pending"}`, "pending", "", false, false, false},
		{200, `{"state":"expired"}`, "expired", "", false, false, false},
		{200, `{"state":"refused","reason":"other_account"}`, "refused", "other_account", false, false, false},
		{200, `{"state":"refused","reason":"cancelled"}`, "refused", "cancelled", false, false, false},
		{200, `{"state":"confirmed","key":"ak_secretsecretsecret","key_id":"ak_id","name":"n","tier":"pro","email":"b@example.test"}`, "confirmed", "", true, false, false},
		{200, `{"state":"confirmed","key":"nope","key_id":""}`, "", "", false, false, true},
		{200, `{"state":"later"}`, "", "", false, false, true},
		{400, `{"error":"x","code":"PAIRING_CODE_INVALID"}`, "", "", false, true, true},
		{502, `{"error":"x","code":"CLERK_ERROR"}`, "", "", false, false, true},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/agent-pairing/pending/poll" || r.Header.Get("Authorization") != "" {
				t.Errorf("unexpected %s %s auth %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			var body map[string]string
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &body); err != nil || body["confirm_id"] != testConfirmID || body["poll_token"] != testPollToken || len(body) != 2 {
				t.Errorf("body %s", b)
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
		r, err := client.New("ak_should_not_be_sent_00000", "dev").PollPairing(context.Background(), pending)
		srv.Close()
		if tc.fail {
			if err == nil {
				t.Errorf("%s: accepted %+v", tc.body, r)
			}
			if tc.invalid && !errors.Is(err, client.ErrPairingCodeInvalid) {
				t.Errorf("%s: %v", tc.body, err)
			}
			continue
		}
		if err != nil || string(r.State) != tc.state || r.Reason != tc.reason || (r.Key != nil) != tc.key {
			t.Errorf("%s: %+v %v", tc.body, r, err)
		}
		if tc.key && (r.Key.Key != "ak_secretsecretsecret" || r.Key.Email != "b@example.test" || r.Key.Tier != "pro") {
			t.Errorf("key %+v", r.Key)
		}
	}
}

func TestDeclinePairing(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/agent-pairing/pending/decline" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	err := client.New("ak_should_not_be_sent_00000", "dev").DeclinePairing(context.Background(), &client.PendingPairing{ConfirmID: testConfirmID, PollToken: testPollToken})
	if err != nil || got["confirm_id"] != testConfirmID || got["reason"] != "cancelled" || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
}
