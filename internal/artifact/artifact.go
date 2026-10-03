// Package artifact loads a file an agent session publishes (Story 78.18):
// confined to the session's working directory, never through a symlink, a
// regular file of at most MaxBytes whose extension names an allowed media
// type and whose content matches it. The checks mirror chat-api's
// validateArtifact (chat-api/src/lib/agent-artifacts.ts) so a refused file is
// refused locally, before anything is uploaded.
package artifact

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxBytes is the largest artifact (10 MiB), as chat-api enforces.
const MaxBytes = 10 * 1024 * 1024

// maxNameLen is chat-api's file name limit (JavaScript string length).
const maxNameLen = 200

// MediaTypes maps an allowed file extension to its media type.
var MediaTypes = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".md":   "text/markdown",
	".csv":  "text/csv",
	".json": "application/json",
}

// File is a loaded, validated artifact.
type File struct {
	Name      string
	MediaType string
	Data      []byte
	// Digest is the lowercase hex SHA-256 of Data.
	Digest string
}

// Load reads path (relative to root, or absolute) for publishing. root is
// the session working directory; its own symlinks are resolved first, and
// the file must lie strictly beneath it with no symlink on the way.
func Load(root, path string) (*File, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, errors.New("the session working directory is unknown")
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("path is empty")
	}
	if strings.ContainsRune(path, 0) {
		return nil, errors.New("path contains a NUL byte")
	}
	for _, part := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return nil, errors.New("path must not contain '..'")
		}
	}
	rootClean := filepath.Clean(root)
	rootResolved, err := filepath.EvalSymlinks(rootClean)
	if err != nil {
		return nil, fmt.Errorf("session working directory: %w", err)
	}
	rel, err := relativeTo(path, rootClean, rootResolved)
	if err != nil {
		return nil, err
	}

	r, err := os.OpenRoot(rootResolved)
	if err != nil {
		return nil, fmt.Errorf("session working directory: %w", err)
	}
	defer func() { _ = r.Close() }()

	// No symlink anywhere beneath the root, the file itself included.
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		prefix := filepath.Join(parts[:i+1]...)
		info, err := r.Lstat(prefix)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%s does not exist in the session working directory", rel)
			}
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symbolic link; only regular files in the session working directory can be published", prefix)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", prefix)
		}
	}
	info, err := r.Lstat(rel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("%s is empty", rel)
	}
	if info.Size() > MaxBytes {
		return nil, fmt.Errorf("%s exceeds 10 MiB", rel)
	}
	name := filepath.Base(rel)
	mediaType, ok := MediaTypes[strings.ToLower(filepath.Ext(name))]
	if !ok {
		return nil, fmt.Errorf("%s has an unsupported type; publish a .pdf, .txt, .md, .csv or .json file", name)
	}
	if err := checkName(name); err != nil {
		return nil, err
	}

	f, err := r.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()
	// The file opened must be the regular file checked above (not a link
	// swapped in meanwhile).
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%s changed while it was being read", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s is empty", rel)
	}
	if len(data) > MaxBytes {
		return nil, fmt.Errorf("%s exceeds 10 MiB", rel)
	}
	if err := checkContent(mediaType, data); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	sum := sha256.Sum256(data)
	return &File{Name: name, MediaType: mediaType, Data: data, Digest: hex.EncodeToString(sum[:])}, nil
}

// relativeTo returns path relative to the root (given as written and as
// resolved), refusing anything that is not strictly beneath it.
func relativeTo(path, rootClean, rootResolved string) (string, error) {
	outside := errors.New("the path is outside the session working directory")
	if !filepath.IsAbs(path) {
		rel := filepath.Clean(path)
		if rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
			return "", outside
		}
		return rel, nil
	}
	abs := filepath.Clean(path)
	for _, base := range []string{rootResolved, rootClean} {
		rel, err := filepath.Rel(base, abs)
		if err == nil && rel != "." && filepath.IsLocal(rel) {
			return rel, nil
		}
	}
	return "", outside
}

// checkName mirrors chat-api's file name rule: not blank, at most 200
// UTF-16 code units, no control characters.
func checkName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("the file name is blank")
	}
	if len(utf16.Encode([]rune(name))) > maxNameLen {
		return fmt.Errorf("the file name is longer than %d characters", maxNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("the file name contains a control character")
		}
	}
	return nil
}

// checkContent requires the bytes to match the media type: a PDF header,
// or UTF-8 text without NUL (JSON must also parse).
func checkContent(mediaType string, data []byte) error {
	if mediaType == "application/pdf" {
		if !bytes.HasPrefix(data, []byte("%PDF-")) {
			return errors.New("the file is not a PDF")
		}
		return nil
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return errors.New("the file is not UTF-8 text")
	}
	if mediaType == "application/json" && !json.Valid(data) {
		return errors.New("the file is not valid JSON")
	}
	return nil
}
