package connect

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// testBrowser stands in for a workspace browser's signing key (Story 94.7):
// a P-256 key generated per test, a counter, and the payloads the workspace
// signs.
type testBrowser struct {
	key  *ecdsa.PrivateKey
	spki []byte
	fp   string

	mu      sync.Mutex
	counter int64
}

func newTestBrowser(t *testing.T) *testBrowser {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &testBrowser{key: key, spki: spki, fp: BrowserFingerprint(spki)}
}

// nextCounter is the browser's next counter.
func (b *testBrowser) nextCounter() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.counter++
	return b.counter
}

// sign wraps fields (the kind's own fields) into a signed payload with the
// next counter and returns the sig envelope.
func (b *testBrowser) sign(kind string, fields map[string]any) map[string]any {
	return b.signWithCounter(kind, b.nextCounter(), fields)
}

func (b *testBrowser) signWithCounter(kind string, counter int64, fields map[string]any) map[string]any {
	p := map[string]any{"v": 1, "kind": kind, "counter": counter, "iat": time.Now().UnixMilli()}
	for k, v := range fields {
		p[k] = v
	}
	payload, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return b.signBytes(payload)
}

// signBytes signs raw payload bytes (P1363 r||s, as WebCrypto does).
func (b *testBrowser) signBytes(payload []byte) map[string]any {
	digest := sha256.Sum256(payload)
	r, s, err := ecdsa.Sign(rand.Reader, b.key, digest[:])
	if err != nil {
		panic(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return map[string]any{"v": 1, "key": b64u(b.spki), "payload": b64u(payload), "sig": b64u(sig)}
}

// trust adds the browser to the trusted list in dir.
func (b *testBrowser) trust(t *testing.T, dir string) {
	t.Helper()
	if _, err := TrustBrowser(dir, b.fp, ViaPair); err != nil {
		t.Fatal(err)
	}
}

// textHash is the workspace's textSha256.
func textHash(s string) string { return sha256b64u([]byte(s)) }

// signCommand signs cmd in place, as the workspace does, when the relay has
// a signer and cmd needs a signature and carries none: a start or a
// message as a turn, a mode change, an allowing answer to a card the relay
// received (its digest computed from the event as the relay stored it).
func (r *fakeRelay) signCommand(sid string, cmd map[string]any) {
	r.mu.Lock()
	b := r.signer
	r.mu.Unlock()
	if b == nil || cmd["sig"] != nil {
		return
	}
	str := func(k string) string { s, _ := cmd[k].(string); return s }
	switch cmd["kind"] {
	case "start_session":
		f := map[string]any{"agent": str("agent"), "textSha256": textHash(str("prompt"))}
		if m := str("mode"); m != "" {
			f["mode"] = m
		}
		r.mu.Lock()
		r.agents[str("sessionId")] = str("agent")
		r.mu.Unlock()
		cmd["sig"] = b.sign("turn", f)
	case "user_message":
		r.mu.Lock()
		agent := r.agents[str("sessionId")]
		r.mu.Unlock()
		if agent == "" {
			cmd["sig"] = b.sign("user_message", map[string]any{"sessionId": str("sessionId"), "textSha256": textHash(str("text"))})
			return
		}
		cmd["sig"] = b.sign("turn", map[string]any{"agent": agent, "sessionId": str("sessionId"), "textSha256": textHash(str("text"))})
	case "set_mode":
		cmd["sig"] = b.sign("set_mode", map[string]any{"sessionId": str("sessionId"), "mode": str("mode")})
	case "approval_response":
		if str("decision") != "allow" {
			return
		}
		cmd["sig"] = b.sign("approval_response", map[string]any{"sessionId": str("sessionId"), "approvalId": str("approvalId"),
			"decision": "allow", "scope": str("scope"), "cardSha256": r.cardHash(sid, str("approvalId"))})
	}
}

// cardHash is the digest of the card the relay received for approvalId,
// computed from the stored event ("" when there is none).
func (r *fakeRelay) cardHash(sid, approvalID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events[sid] {
		p, _ := e["payload"].(map[string]any)
		if p["type"] == "tool-approval-request" && p["approvalId"] == approvalID {
			canon, err := canonicalJSON(p["approvalDescriptor"])
			if err != nil {
				return ""
			}
			return sha256b64u(canon)
		}
	}
	return ""
}

// lastSigning returns the newest signing frame (nil before one).
func (r *fakeRelay) lastSigning() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.signing) == 0 {
		return nil
	}
	return r.signing[len(r.signing)-1]
}

// userFrameKinds lists the user socket's signing, capabilities and
// controls frames in arrival order.
func (r *fakeRelay) userFrameKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.userKinds...)
}
