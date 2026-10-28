package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/connect"
	"github.com/mosaicss/archivist/internal/service"
)

// Story 94.7: commands from the browser are signed. --pair CODE.BROWSER
// and --trust <code> add a browser to this computer's list.

const (
	testBrowserCode  = "KnakR2lnF3Vw9e7hsxlc4cpMuAP1Elyam2CFBVVjVnY"
	otherBrowserCode = "yHbxPe0ofG_46qa2wj-fy7MBxtaVuthcaNndCslykA0"
)

func browserList(t *testing.T, home string) *connect.BrowserList {
	t.Helper()
	l, err := connect.LoadBrowsers(filepath.Join(home, ".archivist", "connect"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The browser code rides after the pairing code: the redeem sends only the
// code, and once the key is saved the browser is trusted for that account.
func TestConnectPairWithBrowserCode(t *testing.T) {
	home := sandbox(t)
	useBrowser(t, &fakeBrowser{available: true})
	fake := browserMosaic(t, 300, confirmedPoll())
	out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK."+testBrowserCode)
	if code != 0 || !strings.Contains(out, "This browser can now use this computer.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	fake.mu.Lock()
	bodies := append([]string(nil), fake.bodies...)
	fake.mu.Unlock()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"ABCDEFGHJK"`) || strings.Contains(bodies[0], testBrowserCode) {
		t.Fatalf("redeem bodies %v", bodies)
	}
	l := browserList(t, home)
	if l.Account != accountEmail || len(l.Browsers) != 1 || l.Browsers[0].Fingerprint != testBrowserCode || l.Browsers[0].Via != connect.ViaPair {
		t.Fatalf("list %+v", l)
	}
	if fi, err := os.Stat(filepath.Join(home, ".archivist", "connect", connect.BrowsersFile)); err != nil || (fi.Mode().Perm() != 0o600 && runtime.GOOS != "windows") {
		t.Fatalf("browsers.json: %v %v", fi, err)
	}
	// Pairing again for the same account without a browser code keeps it;
	// typed with spaces, the code still works.
	browserMosaic(t, 300, confirmedPoll())
	if out, code := runRoot(t, "connect", "--pair", "abcde", "fghjk"); code != 0 || strings.Contains(out, "This browser can now") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if l := browserList(t, home); len(l.Browsers) != 1 {
		t.Fatalf("same account lost its browsers: %+v", l)
	}
	// Another account's pairing empties the list, then trusts its browser.
	browserMosaicFor(t, "other@example.test", 300, confirmedPollFor("other@example.test"))
	if out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK."+otherBrowserCode); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if l := browserList(t, home); l.Account != "other@example.test" || len(l.Browsers) != 1 || l.Browsers[0].Fingerprint != otherBrowserCode {
		t.Fatalf("other account: %+v", l)
	}
	browserMosaic(t, 300, confirmedPoll())
	if out, code := runRoot(t, "connect", "--pair", "ABCDE-FGHJK"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if l := browserList(t, home); l.Account != accountEmail || len(l.Browsers) != 0 {
		t.Fatalf("pairing another account without a code kept browsers: %+v", l)
	}
}

// A malformed browser code exits 2 before anything is sent or saved.
func TestConnectPairMalformedBrowserCode(t *testing.T) {
	for _, value := range []string{"ABCDE-FGHJK.", "ABCDE-FGHJK.abc", "ABCDE-FGHJK." + testBrowserCode + "x",
		"ABCDE-FGHJK." + strings.Replace(testBrowserCode, "K", "+", 1), "ABCDE-FGHJK." + testBrowserCode + "." + testBrowserCode} {
		t.Run(value, func(t *testing.T) {
			home := sandbox(t)
			fake := newFakeMosaic(t, redeemBody(accountEmail))
			out, code := runRoot(t, "connect", "--pair", value)
			if code != ExitUsageError || !strings.Contains(out, "the browser code after the pairing code is not valid") {
				t.Fatalf("exit %d\n%s", code, out)
			}
			if redeems, _, other := fake.state(); redeems != 0 || len(other) != 0 {
				t.Fatalf("redeems %d other %v", redeems, other)
			}
			if _, ok := credentials(t, home); ok {
				t.Fatal("credentials written")
			}
			if _, err := os.Stat(filepath.Join(home, ".archivist", "connect", connect.BrowsersFile)); !os.IsNotExist(err) {
				t.Fatal("browsers.json written")
			}
		})
	}
}

func TestConnectTrust(t *testing.T) {
	home := sandbox(t)
	out, code := runRoot(t, "connect", "--trust", testBrowserCode)
	if code != 0 || strings.TrimSpace(out) != "This browser can now use this computer." {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if out, code := runRoot(t, "connect", "--trust", " "+testBrowserCode+" "); code != 0 {
		t.Fatalf("again: exit %d\n%s", code, out)
	}
	l := browserList(t, home)
	if len(l.Browsers) != 1 || l.Browsers[0].Via != connect.ViaTrust || l.Account != "" {
		t.Fatalf("list %+v", l)
	}
	for _, bad := range []string{"", "abc", testBrowserCode[:42], testBrowserCode + "=", "ABCDE-FGHJK"} {
		out, code := runRoot(t, "connect", "--trust", bad)
		if code != ExitUsageError || !strings.Contains(out, "this browser code is not valid") {
			t.Errorf("%q: exit %d\n%s", bad, code, out)
		}
	}
	if l := browserList(t, home); len(l.Browsers) != 1 {
		t.Fatalf("a refused code was added: %+v", l)
	}
	// --status counts them.
	stubRunner(t, func(string) service.Result { return service.Result{} })
	out, _ = runRoot(t, "connect", "--status")
	if !strings.Contains(out, "\nBrowsers: 1\n") {
		t.Fatalf("status:\n%s", out)
	}
	_ = os.WriteFile(filepath.Join(home, ".archivist", "connect", connect.BrowsersFile), []byte("{"), 0o600)
	out, _ = runRoot(t, "connect", "--status")
	if !strings.Contains(out, "\nBrowsers: unreadable") {
		t.Fatalf("status:\n%s", out)
	}
}
