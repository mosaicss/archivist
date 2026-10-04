package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Story 78.30: one-step agent connect. The workspace mints a single use,
// ten minute pairing code for a signed-in Pro user; `archivist connect
// --pair <code>` redeems it once for a new ak_ key. The redeem call carries
// no credential (the code is the credential) and is never retried: the code
// is burned by the first attempt that reaches the server.

// PairingAlphabet is Crockford base32: no I, L, O or U.
const PairingAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// PairingCodeLength is the normalised code length (displayed XXXXX-XXXXX).
const PairingCodeLength = 10

// maxDeviceName bounds the device name sent with a redeem (the server
// sanitises and clips it again).
const maxDeviceName = 64

// ErrPairingCodeInvalid is an unknown, malformed, expired, used or burned
// pairing code: the server answers each with one uniform 400
// PAIRING_CODE_INVALID, and a code that fails NormalizePairingCode is never
// sent.
var ErrPairingCodeInvalid = errors.New("this pairing code is not valid or has expired. Get a new code in Mosaic")

// PairingResult is POST /agent-pairing/redeem's 200 body. Key is the new
// ak_ secret: save it, never print or log it.
type PairingResult struct {
	Key   string `json:"key"`
	KeyID string `json:"key_id"`
	Name  string `json:"name"`
	Tier  string `json:"tier"`
}

// NormalizePairingCode applies the server's normalisation: uppercase, drop
// whitespace and hyphens, read I and L as 1 and O as 0. It returns
// ErrPairingCodeInvalid unless exactly PairingCodeLength alphabet characters
// remain.
func NormalizePairingCode(raw string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(raw) {
		switch {
		case r == '-' || unicode.IsSpace(r):
			continue
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'O':
			r = '0'
		}
		if !strings.ContainsRune(PairingAlphabet, r) {
			return "", ErrPairingCodeInvalid
		}
		b.WriteRune(r)
		if b.Len() > PairingCodeLength {
			return "", ErrPairingCodeInvalid
		}
	}
	if b.Len() != PairingCodeLength {
		return "", ErrPairingCodeInvalid
	}
	return b.String(), nil
}

// SanitizeDeviceName keeps printable characters, collapses whitespace and
// clips to 64 runes; an empty result becomes "device".
func SanitizeDeviceName(name string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range name {
		if n >= maxDeviceName {
			break
		}
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			continue // control characters are stripped
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
			if n >= maxDeviceName {
				break
			}
		}
		b.WriteRune(r)
		n++
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "device"
	}
	return out
}

// RedeemPairing redeems a pairing code (already normalised) for a new key
// with POST /agent-pairing/redeem. It sends no Authorization header and
// makes exactly one attempt. A 400 PAIRING_CODE_INVALID is
// ErrPairingCodeInvalid; any other non-2xx is a *APIError; a transport
// failure is a wrapped network error.
func (c *Client) RedeemPairing(ctx context.Context, code, device string) (*PairingResult, error) {
	payload, err := json.Marshal(map[string]string{"code": code, "device_name": SanitizeDeviceName(device)})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/agent-pairing/redeem", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// The pairing code is the credential: no Bearer, whatever c.Token holds.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Archivist-CLI-Version", c.Version)
	req.Header.Set("X-Archivist-Origin", c.Origin)
	req.Header.Set("User-Agent", fmt.Sprintf("archivist-cli/%s (%s/%s)", c.Version, c.OS, c.Arch))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("archivist: network error: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		apiErr := ParseAPIError(resp)
		if apiErr.Status == http.StatusBadRequest && apiErr.Code == "PAIRING_CODE_INVALID" {
			return nil, ErrPairingCodeInvalid
		}
		return nil, apiErr
	}
	var out PairingResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode pairing response: %w", err)
	}
	if !strings.HasPrefix(out.Key, "ak_") || out.KeyID == "" {
		return nil, fmt.Errorf("pairing response is incomplete")
	}
	return &out, nil
}
