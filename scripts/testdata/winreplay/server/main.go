// Command server is the fake release and redeem endpoint for
// scripts/test-install.ps1 (Story 78.34): `server <root> <log> <portfile>`
// serves <root> as static files on 127.0.0.1 and answers POST
// /agent-pairing/redeem like chat-api for the codes ABCDE12345 and
// ABCDE12346 (400 PAIRING_CODE_INVALID otherwise), logging each request.
// .ps1 files are served as application/octet-stream, like GitHub release
// assets.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

func main() {
	root, logPath, portFile := os.Args[1], os.Args[2], os.Args[3]
	var mu sync.Mutex
	files := http.FileServer(http.Dir(root))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
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
		entry, _ := json.Marshal(map[string]any{"path": r.URL.Path, "auth": r.Header.Get("Authorization"), "body": body})
		mu.Lock()
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, _ = f.Write(append(entry, '\n'))
			_ = f.Close()
		}
		mu.Unlock()
		code, _ := body["code"].(string)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/agent-pairing/redeem" && (code == "ABCDE12345" || code == "ABCDE12346") {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"key":    "ak_replay_" + strings.ToLower(code) + strings.Repeat("x", 24),
				"key_id": "ak_id_replay", "name": "archivist replay 2026-10-04 00:00:00Z ab12", "tier": "pro",
			})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"This pairing code is not valid or has expired.","code":"PAIRING_CODE_INVALID"}`)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.WriteFile(portFile, []byte(fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)), 0o600)
	_ = http.Serve(ln, nil)
}
