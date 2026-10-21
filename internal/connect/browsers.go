package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mosaicss/archivist/internal/fsutil"
)

// The browsers this computer trusts (Story 94.7): ~/.archivist/connect/
// browsers.json, written by `archivist connect --pair CODE.FP`, by
// `archivist connect --trust <code>` and by the daemon when an already
// trusted browser adds another (trust_browser). The daemon reads it at each
// verification and announces the list to the relay (the signing frame), so
// the workspace knows which browsers can drive this computer. Nothing a
// server sends ever adds to it.

// File names in the state directory.
const (
	BrowsersFile = "browsers.json"
	countersFile = "browser-counters.json"
)

// MaxBrowsers caps the trusted browsers; adding one more drops the oldest.
const MaxBrowsers = 32

// How a browser was trusted.
const (
	ViaPair    = "pair"
	ViaTrust   = "trust"
	ViaBrowser = "browser"
)

// TrustedBrowser is one trusted browser key.
type TrustedBrowser struct {
	Fingerprint string `json:"fingerprint"`
	Added       string `json:"added"`
	Via         string `json:"via"`
}

// BrowserList is browsers.json: the account the computer was paired with
// ("" = not known) and its trusted browsers, oldest first.
type BrowserList struct {
	Version  int              `json:"version"`
	Account  string           `json:"account"`
	Browsers []TrustedBrowser `json:"browsers"`
}

// browserListMu serializes this process's read-modify-write of the list.
var browserListMu sync.Mutex

// LoadBrowsers reads the trusted browser list from the state directory. A
// missing file is an empty list; an unreadable or malformed one is an error.
func LoadBrowsers(dir string) (*BrowserList, error) {
	data, err := fsutil.ReadFile(filepath.Join(dir, BrowsersFile))
	if errors.Is(err, os.ErrNotExist) {
		return &BrowserList{Version: 1, Browsers: []TrustedBrowser{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var l BrowserList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", BrowsersFile, err)
	}
	if l.Version != 1 {
		return nil, fmt.Errorf("%s has version %d, not 1", BrowsersFile, l.Version)
	}
	for _, b := range l.Browsers {
		if !ValidBrowserCode(b.Fingerprint) {
			return nil, fmt.Errorf("%s holds an entry that is not a browser code", BrowsersFile)
		}
	}
	if l.Browsers == nil {
		l.Browsers = []TrustedBrowser{}
	}
	return &l, nil
}

// SaveBrowsers writes the list (0600, atomically).
func SaveBrowsers(dir string, l *BrowserList) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	l.Version = 1
	if l.Browsers == nil {
		l.Browsers = []TrustedBrowser{}
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, BrowsersFile), append(data, '\n'))
}

// Trusts reports whether fp is on the list.
func (l *BrowserList) Trusts(fp string) bool {
	return slices.ContainsFunc(l.Browsers, func(b TrustedBrowser) bool { return b.Fingerprint == fp })
}

// Fingerprints lists the trusted fingerprints, oldest first, unique.
func (l *BrowserList) Fingerprints() []string {
	out := make([]string, 0, len(l.Browsers))
	for _, b := range l.Browsers {
		if !slices.Contains(out, b.Fingerprint) {
			out = append(out, b.Fingerprint)
		}
	}
	return out
}

// Add trusts fp (no change when it is already trusted) and drops the
// oldest entries beyond MaxBrowsers; it reports whether fp was added.
func (l *BrowserList) Add(fp, via string, now time.Time) bool {
	if l.Trusts(fp) {
		return false
	}
	l.Browsers = append(l.Browsers, TrustedBrowser{Fingerprint: fp, Added: now.UTC().Format(time.RFC3339), Via: via})
	if n := len(l.Browsers); n > MaxBrowsers {
		l.Browsers = slices.Clone(l.Browsers[n-MaxBrowsers:])
	}
	return true
}

// TrustBrowser adds fp to the list in dir (via "trust" or "browser"); it
// reports whether fp was new.
func TrustBrowser(dir, fp, via string) (bool, error) {
	if !ValidBrowserCode(fp) {
		return false, errors.New("not a browser code")
	}
	browserListMu.Lock()
	defer browserListMu.Unlock()
	l, err := LoadBrowsers(dir)
	if err != nil {
		return false, err
	}
	if !l.Add(fp, via, time.Now()) {
		return false, nil
	}
	return true, SaveBrowsers(dir, l)
}

// PairBrowsers records a pairing with account (the paired account email):
// a list kept for another account is emptied (one with no account recorded
// is kept and takes this account), then fp, when not "", is trusted (via
// "pair"). An unreadable list is replaced.
func PairBrowsers(dir, account, fp string) error {
	if fp != "" && !ValidBrowserCode(fp) {
		return errors.New("not a browser code")
	}
	browserListMu.Lock()
	defer browserListMu.Unlock()
	l, err := LoadBrowsers(dir)
	if err != nil {
		l = &BrowserList{Version: 1}
	}
	if l.Account != "" && !strings.EqualFold(l.Account, account) {
		l.Browsers = nil
	}
	l.Account = account
	if fp != "" {
		l.Add(fp, ViaPair, time.Now())
	}
	return SaveBrowsers(dir, l)
}

// ─── counters ───────────────────────────────────────────────────────────────

// counterWindow is the number of recent counters kept per browser; older
// ones fold into the floor.
const counterWindow = 256

// browserCounters is browser-counters.json: per browser fingerprint, the
// floor (every counter at or below it is used) and the last accepted
// counters above it. Only the daemon writes it.
type browserCounters struct {
	Version  int                      `json:"version"`
	Browsers map[string]*counterState `json:"browsers"`
}

type counterState struct {
	Floor int64   `json:"floor"`
	Seen  []int64 `json:"seen"`
}

// counterStore guards one browser key's counters against reuse. A counter
// is reserved when its command passes the checks and recorded only once
// the command runs; a command that does not run releases it.
type counterStore struct {
	path     string
	mu       sync.Mutex
	reserved map[string]map[int64]bool
}

func newCounterStore(dir string) *counterStore {
	return &counterStore{path: filepath.Join(dir, countersFile), reserved: map[string]map[int64]bool{}}
}

func (c *counterStore) load() (*browserCounters, error) {
	data, err := fsutil.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return &browserCounters{Version: 1, Browsers: map[string]*counterState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var f browserCounters
	if err := json.Unmarshal(data, &f); err != nil || f.Version != 1 {
		return nil, fmt.Errorf("%s is not valid", countersFile)
	}
	if f.Browsers == nil {
		f.Browsers = map[string]*counterState{}
	}
	return &f, nil
}

// counterTicket is a reserved counter.
type counterTicket struct {
	fp      string
	counter int64
}

// reserve checks that counter is fresh for fp (above the floor, not seen,
// not reserved by a command in flight) and reserves it.
func (c *counterStore) reserve(fp string, counter int64) (*counterTicket, *signFailure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := c.load()
	if err != nil {
		return nil, &signFailure{reason: "browser counters unreadable: " + err.Error(), store: true}
	}
	if st := f.Browsers[fp]; st != nil && (counter <= st.Floor || slices.Contains(st.Seen, counter)) {
		return nil, mismatch("counter %d was already used", counter)
	}
	if c.reserved[fp][counter] {
		return nil, mismatch("counter %d is in use", counter)
	}
	if c.reserved[fp] == nil {
		c.reserved[fp] = map[int64]bool{}
	}
	c.reserved[fp][counter] = true
	return &counterTicket{fp: fp, counter: counter}, nil
}

// release gives a reserved counter back (its command did not run).
func (c *counterStore) release(t *counterTicket) {
	if t == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reserved[t.fp], t.counter)
}

// commit records a reserved counter as used (its command ran). When the
// file cannot be written the counter stays reserved for this run.
func (c *counterStore) commit(t *counterTicket) error {
	if t == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, err := c.load()
	if err != nil {
		return err
	}
	st := f.Browsers[t.fp]
	if st == nil {
		st = &counterState{}
		f.Browsers[t.fp] = st
	}
	st.Seen = append(st.Seen, t.counter)
	slices.Sort(st.Seen)
	if n := len(st.Seen); n > counterWindow {
		st.Floor = max(st.Floor, st.Seen[n-counterWindow-1])
		st.Seen = slices.Clone(st.Seen[n-counterWindow:])
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(c.path, data); err != nil {
		return err
	}
	delete(c.reserved[t.fp], t.counter)
	return nil
}
