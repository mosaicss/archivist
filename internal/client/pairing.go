package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Story 78.30: one-step agent connect. The workspace mints a single use,
// ten minute pairing code for a signed-in Pro user; `archivist connect
// --pair <code>` redeems it once for a new ak_ key. The redeem call carries
// no credential (the code is the credential) and is never retried: the code
// is burned by the first attempt that reaches the server.
//
// Story 94.3: the redeem names the account, and the CLI saves the key only
// once that account is confirmed on the workspace confirm page, which
// answers for the account signed in there; the CLI polls for the key.
// Story 94.15: every pairing takes that page, confirmed by one click.

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
// ak_ secret: save it, never print or log it. Email (Story 94.3) is the
// verified address of the account the code belongs to, shown before the key
// is saved; a server that predates it leaves it empty.
type PairingResult struct {
	Key   string `json:"key"`
	KeyID string `json:"key_id"`
	Name  string `json:"name"`
	Tier  string `json:"tier"`
	Email string `json:"email"`
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

// PairConfirm is how the CLI confirms the account a pairing code names
// (Story 94.3), sent as the redeem's "confirm" field. Since Story 94.15
// every pairing is confirmed on the confirm page.
type PairConfirm string

// ConfirmBrowser: no key yet; the redeem answers 202 with a pending pairing
// that one Confirm click on the workspace confirm page answers, in any
// browser signed in to the code's account.
const ConfirmBrowser PairConfirm = "browser"

// PendingPairing is the redeem's 202 body in browser mode. ConfirmID (the
// confirm page URL fragment) and PollToken (sent only to the poll) are
// independent secrets: never print or log PollToken.
type PendingPairing struct {
	Email               string `json:"email"`
	ConfirmID           string `json:"confirm_id"`
	PollToken           string `json:"poll_token"`
	ConfirmURL          string `json:"confirm_url"`
	ExpiresInSeconds    int    `json:"expires_in_seconds"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
}

// pairSecretRe is a confirm_id or poll_token: 32 random bytes, base64url.
var pairSecretRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// confirmURLRe keeps the confirm URL to characters that no opener (a shell,
// cmd's start, which expands %NAME%) reads as anything but the URL.
var confirmURLRe = regexp.MustCompile(`^[A-Za-z0-9:/._~#?=-]+$`)

// validConfirmURL is an https URL, or http on a loopback host (local test
// servers), made only of safe characters, whose fragment is confirmID.
func validConfirmURL(raw, confirmID string) bool {
	if !confirmURLRe.MatchString(raw) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != confirmID {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		return host == "127.0.0.1" || host == "localhost" || host == "::1"
	}
	return false
}

// postPublic sends a JSON POST that carries no credential (the pairing code
// or the pending pairing's secrets are the credential), exactly once.
func (c *Client) postPublic(ctx context.Context, path string, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// No Bearer, whatever c.Token holds.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Archivist-CLI-Version", c.Version)
	req.Header.Set("X-Archivist-Origin", c.Origin)
	req.Header.Set("User-Agent", fmt.Sprintf("archivist-cli/%s (%s/%s)", c.Version, c.OS, c.Arch))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("archivist: network error: %w", err)
	}
	return resp, nil
}

// RedeemPairing redeems a pairing code (already normalised) with POST
// /agent-pairing/redeem, saying how the CLI confirms. It sends no
// Authorization header and makes exactly one attempt. A 202 is a
// *PendingPairing and no key; a 200 is a key minted at once
// (*PairingResult, from a server older than Story 94.3, which the caller
// revokes and never saves). A
// 400 PAIRING_CODE_INVALID is ErrPairingCodeInvalid; any other non-2xx is a
// *APIError; a transport failure is a wrapped network error.
func (c *Client) RedeemPairing(ctx context.Context, code, device string, confirm PairConfirm) (*PairingResult, *PendingPairing, error) {
	resp, err := c.postPublic(ctx, "/agent-pairing/redeem", map[string]string{
		"code": code, "device_name": SanitizeDeviceName(device), "confirm": string(confirm),
	})
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		res, err := decodePairingResult(resp.Body)
		return res, nil, err
	case http.StatusAccepted:
		var out PendingPairing
		if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil {
			return nil, nil, fmt.Errorf("decode pairing response: %w", err)
		}
		if !pairSecretRe.MatchString(out.ConfirmID) || !pairSecretRe.MatchString(out.PollToken) ||
			!validConfirmURL(out.ConfirmURL, out.ConfirmID) || out.ExpiresInSeconds <= 0 {
			return nil, nil, fmt.Errorf("pairing response is incomplete")
		}
		return nil, &out, nil
	}
	apiErr := ParseAPIError(resp)
	if apiErr.Status == http.StatusBadRequest && apiErr.Code == "PAIRING_CODE_INVALID" {
		return nil, nil, ErrPairingCodeInvalid
	}
	return nil, nil, apiErr
}

func decodePairingResult(r io.Reader) (*PairingResult, error) {
	var out PairingResult
	if err := json.NewDecoder(io.LimitReader(r, 64*1024)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode pairing response: %w", err)
	}
	if !strings.HasPrefix(out.Key, "ak_") || out.KeyID == "" {
		return nil, fmt.Errorf("pairing response is incomplete")
	}
	return &out, nil
}

// PollState is the browser's answer to a pending pairing.
type PollState string

const (
	PollPending   PollState = "pending"
	PollConfirmed PollState = "confirmed"
	PollRefused   PollState = "refused"
	PollExpired   PollState = "expired"
)

// PollResult is POST /agent-pairing/pending/poll's 200 body. Key is set only
// with PollConfirmed, exactly once (save it, never print or log it); Reason
// (other_account, signed_out or cancelled) only with PollRefused.
type PollResult struct {
	State  PollState
	Reason string
	Key    *PairingResult
}

// PollPairing asks once whether the browser answered the pending pairing,
// with POST /agent-pairing/pending/poll and no credential. An unknown
// pairing or a wrong poll token (400 PAIRING_CODE_INVALID) is
// ErrPairingCodeInvalid; any other non-2xx is a *APIError; a transport
// failure is a wrapped network error.
func (c *Client) PollPairing(ctx context.Context, p *PendingPairing) (*PollResult, error) {
	resp, err := c.postPublic(ctx, "/agent-pairing/pending/poll", map[string]string{
		"confirm_id": p.ConfirmID, "poll_token": p.PollToken,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		apiErr := ParseAPIError(resp)
		if apiErr.Status == http.StatusBadRequest && apiErr.Code == "PAIRING_CODE_INVALID" {
			return nil, ErrPairingCodeInvalid
		}
		return nil, apiErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read poll response: %w", err)
	}
	var head struct {
		State  PollState `json:"state"`
		Reason string    `json:"reason"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("decode poll response: %w", err)
	}
	switch head.State {
	case PollPending, PollExpired:
		return &PollResult{State: head.State}, nil
	case PollRefused:
		return &PollResult{State: head.State, Reason: head.Reason}, nil
	case PollConfirmed:
		key, err := decodePairingResult(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		return &PollResult{State: head.State, Key: key}, nil
	}
	return nil, fmt.Errorf("poll response has an unknown state")
}

// DeclinePairing refuses the pending pairing (POST
// /agent-pairing/pending/decline, reason "cancelled", no credential), so the
// browser page shows it was not connected. The server answers 204 whatever
// the pairing's state.
func (c *Client) DeclinePairing(ctx context.Context, p *PendingPairing) error {
	resp, err := c.postPublic(ctx, "/agent-pairing/pending/decline", map[string]string{
		"confirm_id": p.ConfirmID, "reason": "cancelled",
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ParseAPIError(resp)
	}
	return nil
}

// RevokeCurrentKey revokes the client's own key with DELETE
// /account/cli-tokens/current (Story 94.3): the server revokes exactly the
// calling ak_ key, so the key then answers 401. A 204 is nil; a 401 (the key
// no longer works) is ErrUnauthorized; any other non-2xx is a *APIError; a
// transport failure is a wrapped network error.
func (c *Client) RevokeCurrentKey(ctx context.Context) error {
	resp, err := c.Do(ctx, http.MethodDelete, "/account/cli-tokens/current", nil)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return ParseAPIError(resp)
	}
	return nil
}
