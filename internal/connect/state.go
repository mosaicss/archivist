package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/mosaicss/archivist/internal/fsutil"
)

// errCorruptRecord marks a session record that exists but cannot be used.
var errCorruptRecord = errors.New("corrupt session record")

// maxHandled bounds the remembered command ids per session.
const maxHandled = 512

// SessionRecord is the local state for one relay session. It holds the
// session id mapping, working directory and expiry, never tokens.
type SessionRecord struct {
	SessionID          string   `json:"sessionId"`
	Agent              string   `json:"agent"`
	StartCorrelationID string   `json:"startCorrelationId"`
	ClaudeSessionID    string   `json:"claudeSessionId,omitempty"`
	CodexThreadID      string   `json:"codexThreadId,omitempty"`
	Cwd                string   `json:"cwd,omitempty"`
	ExpiresAt          int64    `json:"expiresAt"`
	Status             string   `json:"status"` // active | ended | failed
	CreatedAt          int64    `json:"createdAt"`
	Handled            []string `json:"handled"`
	// Session controls (Story 78.32): the permission mode (re-clamped to
	// the ceiling on resume), and the model and effort over the machine
	// flags ("" = the flags).
	Mode   string `json:"mode,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// HasHandled reports whether a command id was already processed.
func (r *SessionRecord) HasHandled(cid string) bool {
	for _, h := range r.Handled {
		if h == cid {
			return true
		}
	}
	return false
}

// MarkHandled remembers a processed command id (bounded, oldest first out).
func (r *SessionRecord) MarkHandled(cid string) {
	if r.HasHandled(cid) {
		return
	}
	r.Handled = append(r.Handled, cid)
	if len(r.Handled) > maxHandled {
		r.Handled = r.Handled[len(r.Handled)-maxHandled:]
	}
}

// Store keeps session records under <dir>/sessions (0700 dirs, 0600 files),
// per-session private runtime files (task token, MCP config) under
// <dir>/run/<session>, Codex homes under <dir>/codex-home/<session> and the
// session folders under <dir>/work (Story 94.5).
type Store struct {
	dir string
	mu  sync.Mutex
	// failSaves makes the next N saves fail (tests of the durability rule).
	failSaves int
}

// DefaultStateDir is ~/.archivist/connect.
func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory: %w", err)
	}
	return filepath.Join(home, ".archivist", "connect"), nil
}

// OpenStore creates the state tree with private permissions.
func OpenStore(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, "sessions"), filepath.Join(dir, "run"),
		filepath.Join(dir, codexHomeDir), filepath.Join(dir, workDirName)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

// Dir is the state root.
func (s *Store) Dir() string { return s.dir }

func (s *Store) recordPath(id string) (string, error) {
	if !uuidRe.MatchString(id) {
		return "", fmt.Errorf("invalid session id")
	}
	return filepath.Join(s.dir, "sessions", id+".json"), nil
}

// Load reads a session record; a missing record wraps os.ErrNotExist.
func (s *Store) Load(id string) (*SessionRecord, error) {
	path, err := s.recordPath(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := fsutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec SessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("session record %s: %w: %v", path, errCorruptRecord, err)
	}
	if rec.SessionID != id {
		return nil, fmt.Errorf("session record %s names another session: %w", path, errCorruptRecord)
	}
	return &rec, nil
}

// Save writes a session record atomically (0600).
func (s *Store) Save(rec *SessionRecord) error {
	path, err := s.recordPath(rec.SessionID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSaves > 0 {
		s.failSaves--
		return errors.New("injected save failure")
	}
	return writeFileAtomic(path, data)
}

// List returns every readable record, sorted by creation.
func (s *Store) List() ([]*SessionRecord, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "sessions"))
	if err != nil {
		return nil, err
	}
	var out []*SessionRecord
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !uuidRe.MatchString(id) {
			continue
		}
		rec, err := s.Load(id)
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

// Remove deletes a session record (Story 94.6: pruning old ended records).
// A missing record is not an error.
func (s *Store) Remove(id string) error {
	path, err := s.recordPath(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fsutil.RemoveAll(path)
}

// RunDir returns (creating) the private runtime directory for a session.
func (s *Store) RunDir(id string) (string, error) {
	if !uuidRe.MatchString(id) {
		return "", fmt.Errorf("invalid session id")
	}
	d := filepath.Join(s.dir, "run", id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, os.Chmod(d, 0o700)
}

// RemoveRunDir deletes a session's runtime files (token, MCP config).
func (s *Store) RemoveRunDir(id string) {
	if uuidRe.MatchString(id) {
		_ = fsutil.RemoveAll(filepath.Join(s.dir, "run", id))
	}
}

// codexHomeDir is the parent of per-session Codex homes (Story 78.17).
const codexHomeDir = "codex-home"

// CodexHomePath is the per-session CODEX_HOME (not created).
func (s *Store) CodexHomePath(id string) (string, error) {
	if !uuidRe.MatchString(id) {
		return "", fmt.Errorf("invalid session id")
	}
	return filepath.Join(s.dir, codexHomeDir, id), nil
}

// RemoveCodexHome deletes a session's Codex home (rollouts, the auth.json
// link and Codex's local databases). The link target, the owner's
// auth.json, is never touched: RemoveAll removes the link itself.
func (s *Store) RemoveCodexHome(id string) {
	if dir, err := s.CodexHomePath(id); err == nil {
		_ = fsutil.RemoveAll(dir)
	}
}

// cwdPrefix is the session folder name prefix (makeCwd).
const cwdPrefix = "archivist-connect-"

// workDirName is the parent of the session folders inside the state
// directory (Story 94.5): <state>/work/archivist-connect-*.
const workDirName = "work"

// WorkDir is the parent of the session folders (created by OpenStore).
func (s *Store) WorkDir() string { return filepath.Join(s.dir, workDirName) }

// cwdParents are the directories a session folder may sit in: the work
// directory and, for records written before Story 94.5, the OS temp
// directory, each as written and resolved.
func (s *Store) cwdParents() []string {
	var out []string
	for _, d := range []string{s.WorkDir(), os.TempDir()} {
		d = filepath.Clean(d)
		out = append(out, d)
		if r, err := filepath.EvalSymlinks(d); err == nil && r != d {
			out = append(out, r)
		}
	}
	return out
}

// sessionCwdOK reports a working directory path read from a record that
// makeCwd could have created: absolute, base name with cwdPrefix, directly
// inside the work directory (or, for an older record, the OS temp
// directory), never the home directory or a filesystem root.
func (s *Store) sessionCwdOK(dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	clean := filepath.Clean(dir)
	if !strings.HasPrefix(filepath.Base(clean), cwdPrefix) || filepath.Dir(clean) == clean {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == clean {
		return false
	}
	parent := filepath.Dir(clean)
	for _, p := range s.cwdParents() {
		if samePathName(parent, p) {
			return true
		}
	}
	return false
}

// samePathName compares two cleaned paths (any case on Windows).
func samePathName(a, b string) bool {
	return fenceNameEqual(runtime.GOOS, a, b)
}

// errCwdUnusable marks a session folder that fails checkSessionCwd.
var errCwdUnusable = errors.New("no usable working directory")

// checkSessionCwd checks an existing session folder before a harness runs
// in it (Lstat, never following a link): exactly a directory (not a
// symbolic link, a Windows junction or any other irregular entry), in a
// session folder parent, and on Unix owned by this user with mode 0700
// exactly. A missing folder passes (the spawn creates it); every
// other failure wraps errCwdUnusable.
func (s *Store) checkSessionCwd(dir string) error {
	if !s.sessionCwdOK(dir) {
		return fmt.Errorf("%w: %s is not a session folder", errCwdUnusable, dir)
	}
	st, err := os.Lstat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return fmt.Errorf("%w: %v", errCwdUnusable, err)
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symbolic link", errCwdUnusable, dir)
	case st.Mode().Type() != fs.ModeDir:
		return fmt.Errorf("%w: %s is not a plain directory", errCwdUnusable, dir)
	}
	if problem := cwdOwnerProblem(st); problem != "" {
		return fmt.Errorf("%w: %s %s", errCwdUnusable, dir, problem)
	}
	return nil
}

// ensureSessionCwd checks the session folder, creating a missing one
// (0700) in its parent; the result passes checkSessionCwd. Only a folder
// its own Mkdir created is chmodded: an entry that appeared meanwhile
// (ErrExist) goes straight to the check.
func (s *Store) ensureSessionCwd(dir string) error {
	err := s.checkSessionCwd(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	switch err := os.Mkdir(dir, 0o700); {
	case err == nil:
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("%w: %v", errCwdUnusable, err)
		}
	case !errors.Is(err, os.ErrExist):
		return fmt.Errorf("%w: %v", errCwdUnusable, err)
	}
	return s.checkSessionCwd(dir)
}

// sweepWork removes the session folders in the work directory that no
// active record names (keep: their cwds), as the system temp directory
// used to be cleared at reboot: leftovers would otherwise grow every
// launch's locked list.
func (s *Store) sweepWork(keep []string) {
	entries, err := os.ReadDir(s.WorkDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), cwdPrefix) {
			continue
		}
		p := filepath.Join(s.WorkDir(), e.Name())
		kept := false
		for _, k := range keep {
			kept = kept || fencePathEqual(runtime.GOOS, p, k)
		}
		if !kept {
			_ = fsutil.RemoveAll(p) // a link is removed, never its target
		}
	}
}

// removeSessionCwd deletes a session working directory read from a record,
// but only one makeCwd could have created (sessionCwdOK).
func (s *Store) removeSessionCwd(dir string) bool {
	if !s.sessionCwdOK(dir) {
		return false
	}
	return fsutil.RemoveAll(filepath.Clean(dir)) == nil
}

// writeFileAtomic writes data to path through a 0600 temp file and rename,
// so readers (mcp serve re-reading a token) never see a partial file.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return cleanup(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		return cleanup(err)
	}
	if err := fsutil.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// isNotExist reports a missing record.
func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
