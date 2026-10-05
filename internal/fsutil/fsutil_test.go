package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A reader never fails while a writer replaces the file atomically (temp
// file plus Rename) as fast as it can: on Windows the reader's handle
// shares delete access and a refused open is retried (Story 78.34).
func TestReadFileDuringAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if err := os.WriteFile(path, []byte(`{"n":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			tmp := filepath.Join(dir, fmt.Sprintf(".record-%d.tmp", i))
			if err := os.WriteFile(tmp, []byte(fmt.Sprintf(`{"n":%d}`, i)), 0o600); err != nil {
				writeErr = err
				return
			}
			if err := Rename(tmp, path); err != nil {
				writeErr = err
				return
			}
		}
	}()
	deadline := time.Now().Add(time.Second)
	reads := 0
	for time.Now().Before(deadline) {
		b, err := ReadFile(path)
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("read %d failed during a replace: %v", reads, err)
		}
		if len(b) == 0 || b[0] != '{' {
			t.Fatalf("partial content %q", b)
		}
		reads++
	}
	close(stop)
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("writer: %v", writeErr)
	}
	if reads == 0 {
		t.Fatal("no reads")
	}
}

func TestReadFileMissingAndRemoveAll(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadFile(filepath.Join(dir, "absent")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "a", "b", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tree); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tree kept: %v", err)
	}
	if err := RemoveAll(tree); err != nil {
		t.Fatalf("removing nothing: %v", err)
	}
}
