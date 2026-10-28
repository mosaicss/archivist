package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaicss/archivist/internal/service"
)

// Story 94.3: pairing names the account and saves nothing before it is
// confirmed; Story 94.15: every pairing is confirmed on the Mosaic page.

const (
	earlierKey   = "ak_earliersecretvalue987654"
	accountEmail = "b@example.test"
)

// fakeTTY is a terminal whose answer is fixed in advance.
type fakeTTY struct {
	in      io.Reader
	mu      sync.Mutex
	out     bytes.Buffer
	reading chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func (f *fakeTTY) Read(p []byte) (int, error) {
	f.once.Do(func() { close(f.reading) })
	return f.in.Read(p)
}

func (f *fakeTTY) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.Write(p)
}

func (f *fakeTTY) Close() error {
	f.closed.Store(true)
	if c, ok := f.in.(io.Closer); ok {
		_ = c.Close()
	}
	return nil
}

func (f *fakeTTY) written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.String()
}

// useTerminal gives the test a terminal that answers input (call it after
// sandbox, which installs "no terminal").
func useTerminal(t *testing.T, input string) *fakeTTY {
	t.Helper()
	return useTerminalReader(t, strings.NewReader(input))
}

func useTerminalReader(t *testing.T, in io.Reader) *fakeTTY {
	t.Helper()
	f := &fakeTTY{in: in, reading: make(chan struct{})}
	prev, prevVisible := openConfirmTerminal, pairOutputVisible
	openConfirmTerminal = func() (io.ReadWriteCloser, error) { return f, nil }
	pairOutputVisible = func() bool { return true }
	t.Cleanup(func() { openConfirmTerminal, pairOutputVisible = prev, prevVisible })
	return f
}

// fakeMosaic answers the redeem, the self revoke and the key list.
type fakeMosaic struct {
	mu       sync.Mutex
	redeems  int
	confirms []string       // the confirm field of every redeem
	bodies   []string       // every redeem body as sent
	revoked  []string       // bearer of every DELETE /account/cli-tokens/current
	revokeAs map[string]int // status per bearer; default 204
	listAs   map[string]int // GET /account/cli-tokens status per bearer; default 200
	other    []string
	// Story 94.3 browser mode: pending (when set) answers a browser redeem
	// with 202; polls answer in turn, the last one repeating.
	pending  string
	polls    []fakeAnswer
	pollHits int
	onPoll   func(answer fakeAnswer) // called before a poll is answered
	declines []string                // reason of every decline
}

type fakeAnswer struct {
	status int
	body   string
}

func newFakeMosaic(t *testing.T, redeemBody string) *fakeMosaic {
	t.Helper()
	f := &fakeMosaic{revokeAs: map[string]int{}, listAs: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent-pairing/redeem":
			f.redeems++
			if r.Header.Get("Authorization") != "" {
				t.Errorf("redeem sent Authorization")
			}
			raw, _ := io.ReadAll(r.Body)
			f.bodies = append(f.bodies, string(raw))
			var body map[string]string
			_ = json.Unmarshal(raw, &body)
			f.confirms = append(f.confirms, body["confirm"])
			if body["confirm"] == "browser" && f.pending != "" {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(f.pending))
				return
			}
			_, _ = w.Write([]byte(redeemBody))
		case r.Method == http.MethodPost && r.URL.Path == "/agent-pairing/pending/poll":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("poll sent Authorization")
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["confirm_id"] != testConfirmID || body["poll_token"] != testPollToken {
				t.Errorf("poll body %v", body)
			}
			answer := fakeAnswer{http.StatusOK, `{"state":"pending"}`}
			if len(f.polls) > 0 {
				answer = f.polls[min(f.pollHits, len(f.polls)-1)]
			}
			f.pollHits++
			if f.onPoll != nil {
				f.onPoll(answer)
			}
			w.WriteHeader(answer.status)
			_, _ = w.Write([]byte(answer.body))
		case r.Method == http.MethodPost && r.URL.Path == "/agent-pairing/pending/decline":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["confirm_id"] != testConfirmID {
				t.Errorf("decline body %v", body)
			}
			f.declines = append(f.declines, body["reason"])
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/account/cli-tokens/current":
			f.revoked = append(f.revoked, bearer)
			status := f.revokeAs[bearer]
			if status == 0 {
				status = http.StatusNoContent
			}
			w.WriteHeader(status)
			if status != http.StatusNoContent {
				_, _ = w.Write([]byte(`{"error":"Failed to revoke CLI token.","code":"CLERK_ERROR"}`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/account/cli-tokens":
			status := f.listAs[bearer]
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			if status == http.StatusOK {
				_ = json.NewEncoder(w).Encode(map[string]any{"user_email": accountEmail, "tier": "pro", "tokens": []any{}})
			} else {
				_, _ = w.Write([]byte(`{"error":"Invalid CLI token","code":"INVALID_TOKEN"}`))
			}
		default:
			f.other = append(f.other, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ARCHIVIST_BASE_URL", srv.URL)
	return f
}

func (f *fakeMosaic) state() (int, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.redeems, append([]string(nil), f.revoked...), append([]string(nil), f.other...)
}

func redeemBody(email string) string {
	b, _ := json.Marshal(map[string]string{"key": pairedKey, "key_id": "ak_kid", "name": "archivist box", "tier": "pro", "email": email})
	return string(b)
}

func credentials(t *testing.T, home string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".archivist", "credentials"))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func noKeyLeaks(t *testing.T, out string, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if strings.Contains(out, k) {
			t.Fatalf("output leaks a key:\n%s", out)
		}
	}
}

// With no browser on this computer (SSH, a server) nothing is opened: the
// link is printed, the CLI polls, and a confirm from another device
// completes the pairing.
func TestConnectPairNoBrowserPrintsLinkAndWaits(t *testing.T) {
	home := sandbox(t)
	browser := useBrowser(t, &fakeBrowser{available: false})
	fake := browserMosaic(t, 300, fakeAnswer{http.StatusOK, `{"state":"pending"}`},
		fakeAnswer{http.StatusOK, `{"state":"pending"}`}, confirmedPoll())
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := browser.opened(); len(got) != 0 {
		t.Fatalf("opened %v with no browser", got)
	}
	for _, want := range []string{
		"This computer will connect to b@example.test. Click Confirm on the Mosaic page to finish.\n",
		"Open this link in a browser signed in to that account, on this or any other device: " + confirmURLFor(os.Getenv("ARCHIVIST_BASE_URL")) + "\n",
		"Paired as archivist box.",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("output has %q %d times:\n%s", want, strings.Count(out, want), out)
		}
	}
	if polls, declines, confirms := fake.browserState(); polls != 3 || len(declines) != 0 || len(confirms) != 1 || confirms[0] != "browser" {
		t.Fatalf("polls %d declines %v confirms %v", polls, declines, confirms)
	}
	if saved, _ := credentials(t, home); saved != pairedKey {
		t.Fatalf("saved %q", saved)
	}
	noKeyLeaks(t, out, pairedKey, testPollToken)
}

// A terminal a person can see asks nothing: the pairing takes the page, and
// the variable that once chose the confirmation changes nothing.
func TestConnectPairTerminalAsksNothing(t *testing.T) {
	for _, env := range []string{"", "terminal", "browser"} {
		t.Run(env+"|", func(t *testing.T) {
			home := sandbox(t)
			t.Setenv("ARCHIVIST_PAIR_CONFIRM", env)
			tty := useTerminal(t, "n\n")
			opens := 0
			prev := openConfirmTerminal
			openConfirmTerminal = func() (io.ReadWriteCloser, error) { opens++; return prev() }
			t.Cleanup(func() { openConfirmTerminal = prev })
			browser := useBrowser(t, &fakeBrowser{available: true})
			fake := browserMosaic(t, 300, confirmedPoll())
			out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if opens != 0 || tty.written() != "" {
				t.Fatalf("terminal opened %d times, asked %q", opens, tty.written())
			}
			if _, _, confirms := fake.browserState(); len(confirms) != 1 || confirms[0] != "browser" {
				t.Fatalf("confirms %v", confirms)
			}
			if len(browser.opened()) != 1 {
				t.Fatalf("opened %v", browser.opened())
			}
			if saved, _ := credentials(t, home); saved != pairedKey {
				t.Fatalf("saved %q", saved)
			}
		})
	}
}

// The replaced key: a failed revoke keeps the "still active" note; a 401
// means it no longer works; a task token, a non ak_ key or the same key is
// never revoked.
func TestConnectPairReplacedKeyOutcomes(t *testing.T) {
	cases := []struct {
		name, previous string
		status         int
		revoke         bool
		says, never    string
	}{
		{"revoke fails", earlierKey, http.StatusBadGateway, true, "the key saved here before (ak_earlier...654) is still active. Revoke it in Mosaic", "no longer works"},
		{"already revoked", earlierKey, http.StatusUnauthorized, true, "(ak_earlier...654) no longer works", "still active"},
		{"task token", "mst_00000000-0000-4000-8000-000000000000." + strings.Repeat("a", 43), 0, false, "", "still active"},
		{"legacy prefix", "mc_pat_legacysecretvalue1234", 0, false, "is still active", "no longer works"},
		{"same key", pairedKey, 0, false, "", "saved here before"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := sandbox(t)
			saveKey(t, home, tc.previous)
			useBrowser(t, &fakeBrowser{available: true})
			fake := browserMosaic(t, 300, confirmedPoll())
			if tc.status != 0 {
				fake.revokeAs[tc.previous] = tc.status
			}
			out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			_, revoked, _ := fake.state()
			if tc.revoke != (len(revoked) == 1 && revoked[0] == tc.previous) || len(revoked) > 1 {
				t.Fatalf("revoked %v", revoked)
			}
			if tc.says != "" && !strings.Contains(out, tc.says) {
				t.Errorf("output lacks %q:\n%s", tc.says, out)
			}
			if strings.Contains(out, tc.never) {
				t.Errorf("output has %q:\n%s", tc.never, out)
			}
			if saved, _ := credentials(t, home); saved != pairedKey {
				t.Fatalf("saved %q", saved)
			}
			noKeyLeaks(t, out, earlierKey, pairedKey)
		})
	}
}

// A failed save revokes the new key, which nothing could use.
func TestConnectPairSaveFailureRevokesNewKey(t *testing.T) {
	home := sandbox(t)
	if err := os.WriteFile(filepath.Join(home, ".archivist"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	useBrowser(t, &fakeBrowser{available: true})
	fake := browserMosaic(t, 300, confirmedPoll())
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != ExitGenericError || !strings.Contains(out, "saving the key failed") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, revoked, _ := fake.state(); len(revoked) != 1 || revoked[0] != pairedKey {
		t.Fatalf("revoked %v", revoked)
	}
	noKeyLeaks(t, out, pairedKey)
}

// A manual re-pair with the background service installed says how to
// restart it with the new key; under the installers (ARCHIVIST_INSTALLER=1,
// Story 94.14), which reinstall it next, it does not.
func TestConnectPairServiceInstalledHint(t *testing.T) {
	for _, c := range []struct {
		installed bool
		installer string
		hint      bool
	}{{true, "", true}, {false, "", false}, {true, "1", false}, {false, "1", false}, {true, "0", true}} {
		sandbox(t)
		t.Setenv("ARCHIVIST_INSTALLER", c.installer)
		pairServiceInstalled = func() bool { return c.installed }
		useBrowser(t, &fakeBrowser{available: true})
		browserMosaic(t, 300, confirmedPoll())
		out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
		const hint = "Run 'archivist connect --install' to restart background connect with the new key."
		if code != 0 || strings.Contains(out, hint) != c.hint {
			t.Fatalf("%+v: exit %d\n%s", c, code, out)
		}
	}
}

// Control characters in the server's email never reach the terminal.
func TestConnectPairAccountLineStripsControlCharacters(t *testing.T) {
	sandbox(t)
	useBrowser(t, &fakeBrowser{available: true})
	const raw = "b@example.test\x1b[2J\r\n"
	browserMosaicFor(t, raw, 300, confirmedPollFor(raw))
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 || strings.Contains(out, "\x1b") {
		t.Fatalf("exit %d\n%q", code, out)
	}
	if !strings.Contains(out, "This computer will connect to b@example.test[2J. Click Confirm") {
		t.Fatalf("account line:\n%q", out)
	}
}

// --status names the saved key's account; a key the server refuses and an
// unreachable server each get their own line, and the exit is unchanged.
func TestConnectStatusAccountLine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd flow")
	}
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	stubRunner(t, func(line string) service.Result {
		if strings.Contains(line, "is-active") {
			return service.Result{ExitCode: 3}
		}
		return service.Result{}
	})
	fake := newFakeMosaic(t, `{}`)
	out, code := runRoot(t, "connect", "--status")
	if code != ExitGenericError || !strings.Contains(out, "Key:      saved, ak_paireds...789") || !strings.Contains(out, "\nAccount:  b@example.test\n") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	fake.mu.Lock()
	fake.listAs[pairedKey] = http.StatusUnauthorized
	fake.mu.Unlock()
	out, code = runRoot(t, "connect", "--status")
	if code != ExitGenericError || !strings.Contains(out, "Account:  this key is no longer accepted (run 'archivist connect --pair CODE')") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	t.Setenv("ARCHIVIST_BASE_URL", url)
	out, code = runRoot(t, "connect", "--status")
	if code != ExitGenericError || !strings.Contains(out, "Account:  unknown (could not reach Mosaic)") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	// No saved key: no account line.
	sandbox(t)
	out, _ = runRoot(t, "connect", "--status")
	if strings.Contains(out, "Account:") {
		t.Fatalf("account line without a key:\n%s", out)
	}
	noKeyLeaks(t, out, pairedKey)
}

// --status with the service running and a key saved still exits 0, with
// the account line.
func TestConnectStatusRunningWithAccountExitsZero(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd flow")
	}
	home := sandbox(t)
	saveKey(t, home, pairedKey)
	unit := filepath.Join(home, ".config", "systemd", "user", "archivist-connect.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\nExecStart=/usr/bin/archivist connect --service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubRunner(t, func(string) service.Result { return service.Result{} })
	newFakeMosaic(t, `{}`)
	out, code := runRoot(t, "connect", "--status")
	if code != 0 || !strings.Contains(out, "Service:  running") || !strings.Contains(out, "\nAccount:  b@example.test\n") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// ---------------------------------------------------------------------------
// Story 94.3: the workspace confirm page answers a pending pairing; since
// Story 94.15 (owner D13) every pairing waits there for one Confirm click.
// ---------------------------------------------------------------------------

const (
	testConfirmID = "cccccccccccccccccccccccccccccccccccccccccc_"
	testPollToken = "ppppppppppppppppppppppppppppppppppppppppp-p"
)

func confirmURLFor(base string) string { return base + "/connect/confirm#" + testConfirmID }

// fakeBrowser stands in for this computer's browser: it records the URLs it
// opens, and can fail to start or exit with an error.
type fakeBrowser struct {
	mu        sync.Mutex
	urls      []string
	startErr  error
	exitErr   error
	available bool
}

func useBrowser(t *testing.T, b *fakeBrowser) *fakeBrowser {
	t.Helper()
	pairBrowserAvailable = func() bool { return b.available }
	openPairBrowser = func(url string) (<-chan error, error) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.urls = append(b.urls, url)
		if b.startErr != nil {
			return nil, b.startErr
		}
		done := make(chan error, 1)
		done <- b.exitErr
		return done, nil
	}
	prev := pairPollEvery
	pairPollEvery = func(int) time.Duration { return 5 * time.Millisecond }
	t.Cleanup(func() { pairPollEvery = prev })
	return b
}

func (b *fakeBrowser) opened() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.urls...)
}

// browserMosaic is a fake server whose browser redeem answers 202 pending
// for accountEmail.
func browserMosaic(t *testing.T, expires int, polls ...fakeAnswer) *fakeMosaic {
	t.Helper()
	return browserMosaicFor(t, accountEmail, expires, polls...)
}

// browserMosaicFor is browserMosaic for another account's code.
func browserMosaicFor(t *testing.T, email string, expires int, polls ...fakeAnswer) *fakeMosaic {
	t.Helper()
	fake := newFakeMosaic(t, redeemBody(email))
	b, _ := json.Marshal(map[string]any{
		"pending": true, "email": email, "confirm_id": testConfirmID, "poll_token": testPollToken,
		"confirm_url": confirmURLFor(os.Getenv("ARCHIVIST_BASE_URL")), "expires_in_seconds": expires,
		"poll_interval_seconds": 2,
	})
	fake.pending = string(b)
	fake.polls = polls
	return fake
}

func confirmedPoll() fakeAnswer { return confirmedPollFor(accountEmail) }

// confirmedPollFor is the confirmed poll that brings the key for email.
func confirmedPollFor(email string) fakeAnswer {
	b, _ := json.Marshal(map[string]string{
		"state": "confirmed", "key": pairedKey, "key_id": "ak_kid", "name": "archivist box", "tier": "pro", "email": email,
	})
	return fakeAnswer{http.StatusOK, string(b)}
}

func (f *fakeMosaic) browserState() (polls int, declines, confirms []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pollHits, append([]string(nil), f.declines...), append([]string(nil), f.confirms...)
}

// Same account: one Confirm click on the page (the fake confirms), the poll
// brings the key, it is saved and the replaced key is revoked. The account
// and the link are printed although the browser opened.
func TestConnectPairBrowserSameAccount(t *testing.T) {
	home := sandbox(t)
	saveKey(t, home, earlierKey)
	browser := useBrowser(t, &fakeBrowser{available: true})
	fake := browserMosaic(t, 300, fakeAnswer{http.StatusOK, `{"state":"pending"}`},
		fakeAnswer{http.StatusInternalServerError, `{"error":"x","code":"INTERNAL"}`}, confirmedPoll())
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := browser.opened(); len(got) != 1 || got[0] != confirmURLFor(os.Getenv("ARCHIVIST_BASE_URL")) {
		t.Fatalf("opened %v", got)
	}
	if saved, _ := credentials(t, home); saved != pairedKey {
		t.Fatalf("saved %q", saved)
	}
	polls, declines, confirms := fake.browserState()
	if polls != 3 || len(declines) != 0 || len(confirms) != 1 || confirms[0] != "browser" {
		t.Fatalf("polls %d declines %v confirms %v", polls, declines, confirms)
	}
	if _, revoked, _ := fake.state(); len(revoked) != 1 || revoked[0] != earlierKey {
		t.Fatalf("revoked %v", revoked)
	}
	for _, want := range []string{"This computer will connect to b@example.test. Click Confirm on the Mosaic page that opens in your browser.\n", "Paired as archivist box.", "Account: b@example.test", "no longer works"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if link := "Open this link in a browser signed in to that account, on this or any other device: " + confirmURLFor(os.Getenv("ARCHIVIST_BASE_URL")); strings.Count(out, link) != 1 {
		t.Errorf("link not shown once:\n%s", out)
	}
	// The link carries confirm_id by design; the poll token never shows.
	noKeyLeaks(t, out, earlierKey, pairedKey, testPollToken)
}

// The page refuses: another account, signed out, or declined (Don't connect,
// or a page that could not finish). Nothing is saved, no key was minted,
// nothing is revoked.
func TestConnectPairBrowserRefused(t *testing.T) {
	for reason, says := range map[string]string{
		"other_account": "a different Mosaic account than the one signed in to your browser",
		"signed_out":    "your browser is not signed in to Mosaic",
		"cancelled":     "this computer was not connected. Nothing was saved. To connect it, get a new code in Mosaic",
	} {
		t.Run(reason, func(t *testing.T) {
			home := sandbox(t)
			saveKey(t, home, earlierKey)
			useBrowser(t, &fakeBrowser{available: true})
			fake := browserMosaic(t, 300, fakeAnswer{http.StatusOK, `{"state":"pending"}`},
				fakeAnswer{http.StatusOK, `{"state":"refused","reason":"` + reason + `"}`})
			out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
			if code != ExitGenericError || !strings.Contains(out, says) {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if saved, _ := credentials(t, home); saved != earlierKey {
				t.Fatalf("saved %q", saved)
			}
			if _, revoked, _ := fake.state(); len(revoked) != 0 {
				t.Fatalf("revoked %v", revoked)
			}
			if strings.Contains(out, "Paired as") {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

// Expired, or still pending at the deadline: not confirmed in time, nothing
// saved.
func TestConnectPairBrowserNotConfirmedInTime(t *testing.T) {
	for name, fake := range map[string]func(t *testing.T) *fakeMosaic{
		"expired": func(t *testing.T) *fakeMosaic {
			return browserMosaic(t, 300, fakeAnswer{http.StatusOK, `{"state":"expired"}`})
		},
		"deadline": func(t *testing.T) *fakeMosaic { return browserMosaic(t, 1) },
		"unknown": func(t *testing.T) *fakeMosaic {
			return browserMosaic(t, 300, fakeAnswer{http.StatusBadRequest, `{"error":"x","code":"PAIRING_CODE_INVALID"}`})
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := sandbox(t)
			useBrowser(t, &fakeBrowser{available: true})
			fake(t)
			out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
			if code != ExitGenericError || !strings.Contains(out, "not confirmed in time. Nothing was saved.") {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if _, ok := credentials(t, home); ok {
				t.Fatal("credentials written")
			}
		})
	}
}

// When the browser cannot be opened (the opener fails to start, or exits
// with an error) the link printed before is enough: it is not printed again
// and the CLI keeps polling.
func TestConnectPairBrowserOpenFailsKeepsPolling(t *testing.T) {
	for name, b := range map[string]*fakeBrowser{
		"start fails": {available: true, startErr: errors.New("no opener")},
		"opener exit": {available: true, exitErr: errors.New("exit status 3")},
	} {
		t.Run(name, func(t *testing.T) {
			home := sandbox(t)
			useBrowser(t, b)
			browserMosaic(t, 300, fakeAnswer{http.StatusOK, `{"state":"pending"}`}, fakeAnswer{http.StatusOK, `{"state":"pending"}`}, confirmedPoll())
			out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			link := "Open this link in a browser signed in to that account, on this or any other device: " + confirmURLFor(os.Getenv("ARCHIVIST_BASE_URL"))
			if strings.Count(out, link) != 1 {
				t.Fatalf("link lines:\n%s", out)
			}
			if saved, _ := credentials(t, home); saved != pairedKey {
				t.Fatalf("saved %q", saved)
			}
		})
	}
}

// Ctrl-C while waiting for the browser declines the pairing: nothing saved.
func TestConnectPairBrowserInterruptDeclines(t *testing.T) {
	home := sandbox(t)
	useBrowser(t, &fakeBrowser{available: true})
	fake := browserMosaic(t, 300)
	prev := pairInterrupt
	pairInterrupt = func(ctx context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		go func() {
			for {
				if polls, _, _ := fake.browserState(); polls >= 2 {
					cancel()
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		return ctx, cancel
	}
	t.Cleanup(func() { pairInterrupt = prev })
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != ExitGenericError || !strings.Contains(out, "Pairing cancelled. Nothing was saved.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, declines, _ := fake.browserState(); len(declines) != 1 || declines[0] != "cancelled" {
		t.Fatalf("declines %v", declines)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials written")
	}
}

// A server that mints at once in browser mode (before Story 94.3), or a
// pending answer that names no account: nothing saved, exit 5; a minted key
// is revoked, a pending pairing declined.
func TestConnectPairBrowserOldOrUnnamedServer(t *testing.T) {
	t.Run("minted at once", func(t *testing.T) {
		home := sandbox(t)
		useBrowser(t, &fakeBrowser{available: true})
		fake := newFakeMosaic(t, redeemBody(accountEmail))
		out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
		if code != ExitServerError || !strings.Contains(out, "nothing was saved") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if _, revoked, _ := fake.state(); len(revoked) != 1 || revoked[0] != pairedKey {
			t.Fatalf("revoked %v", revoked)
		}
		if _, ok := credentials(t, home); ok {
			t.Fatal("credentials written")
		}
	})
	t.Run("no account named", func(t *testing.T) {
		home := sandbox(t)
		browser := useBrowser(t, &fakeBrowser{available: true})
		fake := browserMosaic(t, 300, confirmedPoll())
		fake.pending = strings.Replace(fake.pending, `"email":"b@example.test"`, `"email":""`, 1)
		out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
		if code != ExitServerError || !strings.Contains(out, "nothing was saved") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if polls, declines, _ := fake.browserState(); polls != 0 || len(declines) != 1 {
			t.Fatalf("polls %d declines %v", polls, declines)
		}
		if len(browser.opened()) != 0 {
			t.Fatal("browser opened")
		}
		if _, ok := credentials(t, home); ok {
			t.Fatal("credentials written")
		}
	})
}

// A key the server could not create at the confirmed poll: exit 5.
func TestConnectPairBrowserMintFailure(t *testing.T) {
	home := sandbox(t)
	useBrowser(t, &fakeBrowser{available: true})
	browserMosaic(t, 300, fakeAnswer{http.StatusBadGateway, `{"error":"x","code":"CLERK_ERROR"}`})
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != ExitServerError || !strings.Contains(out, "could not create the key") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials written")
	}
}

// flagContext is done only once the test sets its flag and the CLI next
// looks (Err), so an interrupt can land right after a poll answered.
type flagContext struct {
	context.Context
	flag atomic.Bool
	ch   chan struct{}
	once sync.Once
}

func (f *flagContext) Done() <-chan struct{} { return f.ch }

func (f *flagContext) Err() error {
	if f.flag.Load() {
		f.once.Do(func() { close(f.ch) })
		return context.Canceled
	}
	return nil
}

// Ctrl-C right as the poll brings the key: the key is not saved and is
// revoked, and the pairing is declined.
func TestConnectPairBrowserInterruptRevokesArrivingKey(t *testing.T) {
	home := sandbox(t)
	useBrowser(t, &fakeBrowser{available: true})
	fake := browserMosaic(t, 300, confirmedPoll())
	fc := &flagContext{Context: context.Background(), ch: make(chan struct{})}
	fake.onPoll = func(fakeAnswer) { fc.flag.Store(true) }
	prev := pairInterrupt
	pairInterrupt = func(context.Context) (context.Context, context.CancelFunc) { return fc, func() {} }
	t.Cleanup(func() { pairInterrupt = prev })
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK")
	if code != ExitGenericError || !strings.Contains(out, "Pairing cancelled. Nothing was saved.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, revoked, _ := fake.state(); len(revoked) != 1 || revoked[0] != pairedKey {
		t.Fatalf("revoked %v", revoked)
	}
	if _, declines, _ := fake.browserState(); len(declines) != 1 {
		t.Fatalf("declines %v", declines)
	}
	if _, ok := credentials(t, home); ok {
		t.Fatal("credentials written")
	}
	noKeyLeaks(t, out, pairedKey)
}
