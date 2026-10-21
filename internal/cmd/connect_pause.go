package cmd

// Story 94.6: Pause on this computer. A pause is the marker file
// ~/.archivist/connect/paused, written by `archivist connect --pause` or by
// the daemon when the workspace pauses it ({kind:"pause"} on the user
// socket). Paused, the daemon holds no relay socket: it ends its run
// (sessions stop, task tokens are revoked), registers the pause with the
// relay over HTTPS and then asks the relay every 10 s whether the workspace
// resumed it. Removing the marker (`archivist connect --resume`) or the
// relay answering "not paused" once the pause was registered starts a new
// daemon run. The marker records a successful registration ("registered"),
// so a daemon restarted while paused reads the relay first instead of
// registering again (which would take the place of a Resume clicked while
// the computer was off).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mosaicss/archivist/internal/client"
	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/fsutil"
	"github.com/spf13/cobra"
)

// pauseMarkerName is the pause marker inside the connect state directory.
const pauseMarkerName = "paused"

// pauseRegisteredLine is the marker line written once the relay registered
// the pause.
const pauseRegisteredLine = "registered"

// pauseClearTimeout bounds uninstall's best effort relay pause clear.
const pauseClearTimeout = 5 * time.Second

// Pause timings (vars so tests can shorten them): the marker is checked
// every pauseWatchEvery, a registered pause is read every pausePollEvery,
// and an unregistered one is retried with a doubling wait up to
// pauseRetryMax.
var (
	pauseWatchEvery  = 2 * time.Second
	pausePollEvery   = 10 * time.Second
	pauseRetryFirst  = 2 * time.Second
	pauseRetryMax    = 60 * time.Second
	pauseHTTPTimeout = 10 * time.Second
)

// pauseTicketMargin: a ticket is reused until this long before it expires.
const pauseTicketMargin = 30 * time.Second

// pauseMarkerPath is the marker in the connect state directory.
func pauseMarkerPath(stateDir string) string {
	return filepath.Join(stateDir, pauseMarkerName)
}

// pausedHere reports whether this computer is paused.
func pausedHere(stateDir string) bool {
	_, err := os.Lstat(pauseMarkerPath(stateDir))
	return err == nil
}

// writePauseMarker records the pause (directory 0700, file 0600).
func writePauseMarker(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(pauseMarkerPath(stateDir), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "%s\n", time.Now().UTC().Format(time.RFC3339))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// pauseRegistered reports whether the marker records that the relay
// registered this pause.
func pauseRegistered(stateDir string) bool {
	data, err := fsutil.ReadFile(pauseMarkerPath(stateDir))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pauseRegisteredLine {
			return true
		}
	}
	return false
}

// markPauseRegistered appends the registered line to an existing marker. A
// marker already removed (a CLI resume in the meantime) is never created
// again.
func markPauseRegistered(stateDir string) error {
	f, err := os.OpenFile(pauseMarkerPath(stateDir), os.O_WRONLY|os.O_APPEND, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "%s\n", pauseRegisteredLine)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// removePauseMarker ends the pause; a missing marker is not an error (on
// Windows the removal is retried while the daemon briefly holds it open).
func removePauseMarker(stateDir string) error {
	return fsutil.RemoveAll(pauseMarkerPath(stateDir))
}

// runPauseCmd is `archivist connect --pause` (pause true) or `--resume`.
func runPauseCmd(cmd *cobra.Command, pause bool) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	stateDir, err := connect.DefaultStateDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	paused := pausedHere(stateDir)
	if pause {
		if paused {
			_, _ = fmt.Fprintln(stdout, "Already paused on this computer. Run 'archivist connect --resume' to connect again.")
			return nil
		}
		if err := writePauseMarker(stateDir); err != nil {
			_, _ = fmt.Fprintf(stderr, "archivist connect: could not pause: %v\n", err)
			return &ExitError{Code: ExitGenericError}
		}
		_, _ = fmt.Fprintln(stdout, "Paused on this computer. Your agents stay off here until you run 'archivist connect --resume' or choose Resume in Mosaic.")
		return nil
	}
	if !paused {
		_, _ = fmt.Fprintln(stdout, "Archivist is not paused on this computer.")
		return nil
	}
	if err := removePauseMarker(stateDir); err != nil {
		_, _ = fmt.Fprintf(stderr, "archivist connect: could not resume: %v\n", err)
		return &ExitError{Code: ExitGenericError}
	}
	if pairServiceInstalled() {
		_, _ = fmt.Fprintln(stdout, "Resumed on this computer. Background connect reconnects within a few seconds.")
	} else {
		_, _ = fmt.Fprintln(stdout, "Resumed on this computer. Run 'archivist connect' to connect.")
	}
	return nil
}

// ticketMinter mints the user relay ticket the pause routes take.
type ticketMinter interface {
	MintRelayTicket(ctx context.Context, sessionID string) (*client.RelayTicket, error)
}

// pauseLoop runs the daemon and keeps it paused while the marker exists.
type pauseLoop struct {
	stateDir  string
	relayHTTP string // the relay origin over http(s)
	api       ticketMinter
	http      *http.Client
	log       *connect.Logger
	ticket    *client.RelayTicket
	owner     string
	// oldRelaySaid: the "relay does not know Pause" line was logged.
	oldRelaySaid bool
	// cancelRun ends the current daemon run; pausing records that it was
	// ended for a pause (both guarded by mu).
	mu        sync.Mutex
	cancelRun context.CancelFunc
	pausing   bool
}

// pause is the daemon's Config.OnPause: the workspace paused this computer.
// The pause is recorded and the current run ends; when the marker cannot be
// written the daemon stays connected.
func (p *pauseLoop) pause() {
	if err := writePauseMarker(p.stateDir); err != nil {
		p.log.Printf("warning: could not record the pause (%v); staying connected", err)
		return
	}
	p.endRunForPause()
}

// endRunForPause ends the current daemon run because of a pause.
func (p *pauseLoop) endRunForPause() {
	p.mu.Lock()
	cancel := p.cancelRun
	if cancel != nil {
		p.pausing = true
	}
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func newPauseLoop(stateDir, relayURL string, api ticketMinter, log *connect.Logger) *pauseLoop {
	return &pauseLoop{stateDir: stateDir, relayHTTP: relayHTTPURL(relayURL), api: api, log: log,
		http: &http.Client{Timeout: pauseHTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}}
}

// relayHTTPURL is the relay origin for the pause routes: wss maps to https
// and ws (a loopback relay) to http.
func relayHTTPURL(relayURL string) string {
	u := strings.TrimRight(relayURL, "/")
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	}
	return u
}

// serve runs the daemon (run) until ctx ends, pausing whenever the marker
// appears or the workspace pauses (pause), and resuming per the file
// comment. It returns run's result, or nil when ctx ends; a fatal ticket
// refusal while paused (a revoked key) is returned as a *connect.FatalError.
// A run ended for a pause never makes it return while ctx lives (the
// service would then stay stopped): the loop waits paused, or runs again at
// once when the marker went while that run was ending.
func (p *pauseLoop) serve(ctx context.Context, run func(ctx context.Context) error) error {
	for {
		if pausedHere(p.stateDir) {
			p.log.Printf("paused on this computer: no relay connection; resume in Mosaic or run 'archivist connect --resume'")
			if err := p.waitPaused(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			p.log.Printf("resumed on this computer: connecting")
		}
		runCtx, cancel := context.WithCancel(ctx)
		p.mu.Lock()
		p.cancelRun, p.pausing = cancel, false
		p.mu.Unlock()
		watched := make(chan struct{})
		go func() {
			defer close(watched)
			t := time.NewTicker(pauseWatchEvery)
			defer t.Stop()
			for {
				select {
				case <-runCtx.Done():
					return
				case <-t.C:
					if pausedHere(p.stateDir) {
						p.log.Printf("paused with 'archivist connect --pause'")
						p.endRunForPause()
						return
					}
				}
			}
		}()
		err := run(runCtx)
		p.mu.Lock()
		pausing := p.pausing
		p.cancelRun, p.pausing = nil, false
		p.mu.Unlock()
		cancel()
		<-watched
		switch {
		case ctx.Err() != nil || err != nil:
			return err
		case pausedHere(p.stateDir):
			p.log.Printf("pausing: sessions stopped and the relay connection closed")
		case pausing:
			p.log.Printf("resumed while pausing: connecting again")
		default:
			return nil
		}
	}
}

// waitPaused holds the pause until it ends: the marker is removed (CLI
// resume), or the relay, once it has registered this pause, answers that
// the workspace resumed it. "Not paused" from a relay that never registered
// the pause is never taken as a resume.
func (p *pauseLoop) waitPaused(ctx context.Context) error {
	// A pause registered before a restart is read, never registered again.
	registered := pauseRegistered(p.stateDir)
	wait := pauseRetryFirst
	for {
		if !pausedHere(p.stateDir) {
			return nil
		}
		method := http.MethodGet
		if !registered {
			method = http.MethodPost
		}
		paused, err := p.call(ctx, method)
		var fatal *connect.FatalError
		switch {
		case errors.As(err, &fatal):
			return err
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			if !registered {
				p.logUnregistered(err)
			}
		case !registered:
			registered = true
			p.log.Printf("pause registered with the relay")
			if !paused {
				p.log.Printf("resumed from Mosaic")
				return removePauseMarker(p.stateDir)
			}
			if err := markPauseRegistered(p.stateDir); err != nil {
				p.log.Printf("warning: could not record the pause registration: %v", err)
			}
		case !paused:
			p.log.Printf("resumed from Mosaic")
			return removePauseMarker(p.stateDir)
		}
		next := pausePollEvery
		if !registered {
			next = wait
			wait = min(2*wait, pauseRetryMax)
		}
		if err := p.sleepWatching(ctx, next); err != nil {
			return err
		}
	}
}

// logUnregistered says why the pause is not registered yet (an older relay
// once; anything else every time).
func (p *pauseLoop) logUnregistered(err error) {
	var st *pauseStatusError
	if errors.As(err, &st) && st.status == http.StatusNotFound {
		if !p.oldRelaySaid {
			p.oldRelaySaid = true
			p.log.Printf("the relay does not know Pause yet: paused here only (Mosaic shows this computer offline) until 'archivist connect --resume'")
		}
		return
	}
	p.log.Printf("pause not registered with the relay yet, retrying: %v", err)
}

// sleepWatching waits d, returning early (nil) when the marker is removed.
func (p *pauseLoop) sleepWatching(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		left := time.Until(deadline)
		if left <= 0 || !pausedHere(p.stateDir) {
			return nil
		}
		t := time.NewTimer(min(left, pauseWatchEvery))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// pauseStatusError is a non-2xx answer from a relay pause route.
type pauseStatusError struct{ status int }

func (e *pauseStatusError) Error() string { return fmt.Sprintf("relay answered HTTP %d", e.status) }

// call makes one pause route request (POST registers, GET reads, DELETE
// clears) and returns the relay's "paused".
func (p *pauseLoop) call(ctx context.Context, method string) (bool, error) {
	if err := p.ensureTicket(ctx); err != nil {
		return false, err
	}
	u := p.relayHTTP + "/users/" + url.PathEscape(p.owner) + "/pause?role=daemon"
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+p.ticket.Ticket)
	resp, err := p.http.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		p.ticket = nil // a fresh ticket next time
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, &pauseStatusError{status: resp.StatusCode}
	}
	var answer struct {
		Paused *bool `json:"paused"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Paused == nil {
		return false, fmt.Errorf("relay pause answer is not {paused: boolean}")
	}
	return *answer.Paused, nil
}

// ensureTicket mints a user relay ticket unless the current one is good for
// at least pauseTicketMargin more. A refused key (or Mosaic agent sessions
// switched off) is fatal; anything else is retried by the caller.
func (p *pauseLoop) ensureTicket(ctx context.Context) error {
	if p.ticket != nil && time.Until(time.UnixMilli(p.ticket.ExpiresAt)) > pauseTicketMargin {
		return nil
	}
	p.ticket = nil
	t, err := p.api.MintRelayTicket(ctx, "")
	if err != nil {
		var apiErr *client.APIError
		var exitErr *client.ExitCodeError
		switch {
		case errors.As(err, &apiErr) && apiErr.Code == "FEATURE_DISABLED":
			return &connect.FatalError{Code: apiErr.Code, Message: "Mosaic agent sessions are not enabled on this server (FEATURE_DISABLED)."}
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized:
			return &connect.FatalError{Code: "UNAUTHORIZED", Auth: true, Message: "chat-api rejected the credential (invalid or revoked ak_ key)."}
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden:
			return &connect.FatalError{Code: apiErr.Code, Auth: true, Message: fmt.Sprintf("chat-api refused the credential: %s", apiErr.Error())}
		case errors.As(err, &exitErr) && exitErr.APICode == "MIN_CLI_VERSION":
			return &connect.FatalError{Code: exitErr.APICode, Message: exitErr.Message}
		}
		return fmt.Errorf("relay ticket: %w", err)
	}
	owner, err := ticketOwner(t.Ticket)
	if err != nil {
		return err
	}
	p.ticket, p.owner = t, owner
	return nil
}

// ticketOwner reads the owner (sub) from a relay ticket's claims; the
// signature is the relay's to verify.
func ticketOwner(ticket string) (string, error) {
	payload, _, ok := strings.Cut(ticket, ".")
	if !ok {
		return "", errors.New("malformed relay ticket")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", errors.New("malformed relay ticket claims")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("relay ticket has no subject")
	}
	return claims.Sub, nil
}

// clearRelayPause clears this computer's pause at the relay before
// uninstall revokes the key (Story 94.6), so Mosaic stops showing it paused
// and pairing works again. Best effort within pauseClearTimeout: a failure
// prints one note (none for an older relay without the route). Nothing
// happens when this computer is not paused.
func clearRelayPause(ctx context.Context, stderr io.Writer, version, key string) {
	stateDir, err := connect.DefaultStateDir()
	if err != nil || !pausedHere(stateDir) {
		return
	}
	relayURL, err := relayURLFromEnv(os.Getenv("ARCHIVIST_RELAY_URL"))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pauseClearTimeout)
		defer cancel()
		loop := newPauseLoop(stateDir, relayURL, client.New(key, version), connect.NewLogger(io.Discard))
		_, err = loop.call(ctx, http.MethodDelete)
	}
	// An older relay has no pause routes (404) and never showed a pause.
	var st *pauseStatusError
	oldRelay := errors.As(err, &st) && st.status == http.StatusNotFound
	if err != nil && !oldRelay {
		_, _ = fmt.Fprintf(stderr, "Note: Mosaic may show this computer as paused for a while: %v\n", err)
	}
}
