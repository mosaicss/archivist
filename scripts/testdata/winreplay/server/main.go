// Command server is the fake release and pairing endpoint for
// scripts/test-install.ps1 (Story 78.34): `server <root> <log> <portfile>`
// serves <root> as static files on 127.0.0.1 and answers the pairing routes
// like chat-api for the codes ABCDE12345 and ABCDE12346 (400
// PAIRING_CODE_INVALID otherwise), logging each POST and DELETE. .ps1 files
// are served as application/octet-stream, like GitHub release assets.
//
// Story 94.3: a redeem with "confirm":"browser" answers 202 with a pending
// pairing whose confirm_url is on this server; POST /test/confirm marks it
// confirmed (what the workspace page does in a browser signed in to the
// code's account), and the next poll returns the key once. DELETE
// /account/cli-tokens/current answers 204 and GET /account/cli-tokens names
// the account.
//
// `server <url>` is the replay's BROWSER: it confirms the pending pairing
// named by the URL's fragment and exits.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const replayEmail = "replay@example.test"

type pendingPairing struct {
	pollToken, code, state string
}

func main() {
	if len(os.Args) == 2 && strings.HasPrefix(os.Args[1], "http") {
		os.Exit(openAsBrowser(os.Args[1]))
	}
	root, logPath, portFile := os.Args[1], os.Args[2], os.Args[3]
	var mu sync.Mutex
	pending := map[string]*pendingPairing{}
	files := http.FileServer(http.Dir(root))
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	invalid := func(w http.ResponseWriter) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "This pairing code is not valid or has expired.", "code": "PAIRING_CODE_INVALID",
		})
	}
	keyFor := func(code string) map[string]string {
		return map[string]string{
			"key":    "ak_replay_" + strings.ToLower(code) + strings.Repeat("x", 24),
			"key_id": "ak_id_replay", "name": "archivist replay 2026-10-04 00:00:00Z ab12", "tier": "pro",
			"email": replayEmail,
		}
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/account/cli-tokens" {
			writeJSON(w, http.StatusOK, map[string]any{"user_email": replayEmail, "tier": "pro", "tokens": []any{}})
			return
		}
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			if strings.HasSuffix(r.URL.Path, ".ps1") {
				// GitHub serves release assets as binary; irm must still
				// return the script text.
				w.Header().Set("Content-Type", "application/octet-stream")
			}
			files.ServeHTTP(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		entry, _ := json.Marshal(map[string]any{"method": r.Method, "path": r.URL.Path, "auth": r.Header.Get("Authorization"), "body": body})
		mu.Lock()
		defer mu.Unlock()
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(entry, '\n'))
			_ = f.Close()
		}
		str := func(k string) string { s, _ := body[k].(string); return s }
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/account/cli-tokens/current":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ak_") {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid CLI token", "code": "INVALID_TOKEN"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/agent-pairing/redeem" && (str("code") == "ABCDE12345" || str("code") == "ABCDE12346"):
			if str("confirm") != "browser" {
				writeJSON(w, http.StatusOK, keyFor(str("code")))
				return
			}
			confirmID, pollToken := secret(), secret()
			pending[confirmID] = &pendingPairing{pollToken: pollToken, code: str("code"), state: "pending"}
			writeJSON(w, http.StatusAccepted, map[string]any{
				"pending": true, "email": replayEmail, "confirm_id": confirmID, "poll_token": pollToken,
				"confirm_url":        "http://" + r.Host + "/connect/confirm#" + confirmID,
				"expires_in_seconds": 60, "poll_interval_seconds": 1,
			})
		case r.URL.Path == "/test/confirm":
			if p := pending[str("confirm_id")]; p != nil && p.state == "pending" {
				p.state = "confirmed"
			}
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/agent-pairing/pending/poll":
			p := pending[str("confirm_id")]
			if p == nil || p.pollToken != str("poll_token") {
				invalid(w)
				return
			}
			switch p.state {
			case "confirmed":
				p.state = "completed"
				out := keyFor(p.code)
				out["state"] = "confirmed"
				writeJSON(w, http.StatusOK, out)
			case "pending":
				writeJSON(w, http.StatusOK, map[string]string{"state": "pending"})
			default:
				writeJSON(w, http.StatusOK, map[string]string{"state": "expired"})
			}
		case r.URL.Path == "/agent-pairing/pending/decline":
			w.WriteHeader(http.StatusNoContent)
		default:
			invalid(w)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.WriteFile(portFile, []byte(fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)), 0o600)
	_ = http.Serve(ln, nil)
}

// secret is 32 random bytes, base64url, like chat-api's pairing secrets.
func secret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// openAsBrowser confirms the pending pairing named by raw's fragment.
func openAsBrowser(raw string) int {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment == "" {
		fmt.Fprintln(os.Stderr, "server: not a confirm URL")
		return 2
	}
	body, _ := json.Marshal(map[string]string{"confirm_id": u.Fragment})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(u.Scheme+"://"+u.Host+"/test/confirm", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	return 0
}
