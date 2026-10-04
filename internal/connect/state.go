package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// Store keeps session records under <dir>/sessions (0700 dirs, 0600 files)
// and per-session private runtime files (task token, MCP config) under
// <dir>/run/<session>.
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
	for _, d := range []string{dir, filepath.Join(dir, "sessions"), filepath.Join(dir, "run")} {
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
	data, err := os.ReadFile(path)
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
		_ = os.RemoveAll(filepath.Join(s.dir, "run", id))
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
		_ = os.RemoveAll(dir)
	}
}

// cwdPrefix is the makeCwd temp directory prefix.
const cwdPrefix = "archivist-connect-"

// removeSessionCwd deletes a session working directory read from a record,
// but only one makeCwd could have created: base name with cwdPrefix, never
// the home directory or a filesystem root.
func removeSessionCwd(dir string) bool {
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
	return os.RemoveAll(clean) == nil
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
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// isNotExist reports a missing record.
func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
