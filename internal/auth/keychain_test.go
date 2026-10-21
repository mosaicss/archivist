package auth_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mosaicss/archivist/internal/auth"
)

// Story 94.6: the saved key lives in the keychain where one is usable. These
// tests use the in memory keychain (auth.MockKeychainForTest); every other
// test in the binary sees no keychain, so the file rung behaves as before.

func credPath(home string) string { return filepath.Join(home, ".archivist", "credentials") }

func TestSaveTokenUsesTheKeychainAndWritesNoFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)

	loc, err := auth.SaveToken("ak_keychaintoken1")
	if err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if !loc.Keychain || loc.String() != "the keychain" || loc.Path != credPath(home) {
		t.Fatalf("location: got %+v, want the keychain", loc)
	}
	if _, err := os.Stat(credPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials file: want none, stat err %v", err)
	}
	// Every reader gets the key.
	token, source, err := auth.Resolve("")
	if err != nil || token != "ak_keychaintoken1" || source != auth.SourceKeychain {
		t.Fatalf("Resolve: got (%q, %v, %v), want the keychain key", token, source, err)
	}
	if source.String() != "keychain" {
		t.Errorf("source name: got %q", source.String())
	}
	if saved, err := auth.SavedToken(); err != nil || saved != "ak_keychaintoken1" {
		t.Fatalf("SavedToken: got (%q, %v)", saved, err)
	}
	if got, err := auth.ResolveToken(""); err != nil || got != "ak_keychaintoken1" {
		t.Fatalf("ResolveToken: got (%q, %v)", got, err)
	}
	// The flag and the environment still win.
	t.Setenv("ARCHIVIST_TOKEN", "ak_envtoken12345")
	if token, source, _ := auth.Resolve(""); token != "ak_envtoken12345" || source != auth.SourceEnv {
		t.Fatalf("env rung: got (%q, %v)", token, source)
	}
}

func TestSaveTokenMovesALegacyFileIntoTheKeychain(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	writeCredFile(t, home, "ak_legacyfiletoken\n")
	auth.MockKeychainForTest(t, false)

	if _, err := auth.SaveToken("ak_newkeychaintoken"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if _, err := os.Stat(credPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy file: want removed, stat err %v", err)
	}
	if saved, err := auth.SavedToken(); err != nil || saved != "ak_newkeychaintoken" {
		t.Fatalf("SavedToken: got (%q, %v)", saved, err)
	}
}

func TestMigrateToKeychainMovesTheFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	writeCredFile(t, home, "ak_tobemigrated1\n")

	// Without a keychain nothing moves.
	if moved, err := auth.MigrateToKeychain(); moved || err != nil {
		t.Fatalf("no keychain: got (%v, %v), want (false, nil)", moved, err)
	}
	if _, err := os.Stat(credPath(home)); err != nil {
		t.Fatalf("file must stay without a keychain: %v", err)
	}

	auth.MockKeychainForTest(t, false)
	moved, err := auth.MigrateToKeychain()
	if !moved || err != nil {
		t.Fatalf("migrate: got (%v, %v), want (true, nil)", moved, err)
	}
	if _, err := os.Stat(credPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file: want removed, stat err %v", err)
	}
	token, source, err := auth.Resolve("")
	if err != nil || token != "ak_tobemigrated1" || source != auth.SourceKeychain {
		t.Fatalf("Resolve after migrate: got (%q, %v, %v)", token, source, err)
	}
	// Nothing left to move.
	if moved, err := auth.MigrateToKeychain(); moved || err != nil {
		t.Fatalf("second migrate: got (%v, %v)", moved, err)
	}
}

func TestMigrateToKeychainLeavesAMalformedFile(t *testing.T) {
	home := sandboxHome(t)
	writeCredFile(t, home, "not-a-key\n")
	auth.MockKeychainForTest(t, false)
	if moved, err := auth.MigrateToKeychain(); moved || err != nil {
		t.Fatalf("got (%v, %v), want (false, nil)", moved, err)
	}
	if _, err := os.Stat(credPath(home)); err != nil {
		t.Fatalf("malformed file must stay: %v", err)
	}
}

func TestAFailingKeychainFallsBackToTheFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, true)

	// Nothing saved: a keychain that does not answer is not "no key".
	if _, _, err := auth.Resolve(""); !errors.Is(err, auth.ErrKeychainUnavailable) {
		t.Fatalf("Resolve: got %v, want ErrKeychainUnavailable", err)
	}
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrKeychainUnavailable) {
		t.Fatalf("SavedToken: got %v, want ErrKeychainUnavailable", err)
	}

	loc, err := auth.SaveToken("ak_fallbackfile1")
	if err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	if loc.Keychain || loc.Path != credPath(home) {
		t.Fatalf("location: got %+v, want the file", loc)
	}
	if runtimeHasUnixModes() {
		if fi, err := os.Stat(credPath(home)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("file: %v, mode %v", err, fi)
		}
	}
	token, source, err := auth.Resolve("")
	if err != nil || token != "ak_fallbackfile1" || source != auth.SourceFile {
		t.Fatalf("Resolve: got (%q, %v, %v), want the file", token, source, err)
	}
}

func TestDeleteCredentialsRemovesTheKeychainItemAndTheFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	if _, err := auth.SaveToken("ak_deleteme12345"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	writeCredFile(t, home, "ak_strayfile12345\n")

	existed, where, err := auth.DeleteCredentials()
	if err != nil || !existed || where != "the keychain and "+credPath(home) {
		t.Fatalf("delete: got (%v, %q, %v)", existed, where, err)
	}
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("SavedToken after delete: got %v, want ErrNoToken", err)
	}
	existed, _, err = auth.DeleteCredentials()
	if existed || err != nil {
		t.Fatalf("second delete: got (%v, %v), want (false, nil)", existed, err)
	}
}

func TestMalformedKeychainKeyNamesTheKeychain(t *testing.T) {
	sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	if _, err := auth.SaveToken("bogus-value"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	token, source, err := auth.Resolve("")
	if token != "bogus-value" || source != auth.SourceKeychain || err == nil {
		t.Fatalf("got (%q, %v, %v)", token, source, err)
	}
	if !errors.Is(err, auth.ErrInvalidFormat) {
		t.Fatalf("error %v does not wrap ErrInvalidFormat", err)
	}
}

// TestASymlinkedCredentialsFileKeepsTheFileStore: a dotfiles setup (the
// credentials file is a link) keeps its file; the keychain is not used and
// the link target gets the new key.
func TestASymlinkedCredentialsFileKeepsTheFileStore(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	target := filepath.Join(t.TempDir(), "dotfiles-credentials")
	if err := os.WriteFile(target, []byte("ak_olddotfilekey1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(credPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, credPath(home)); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if moved, err := auth.MigrateToKeychain(); moved || err != nil {
		t.Fatalf("MigrateToKeychain: got (%v, %v), want nothing moved", moved, err)
	}
	loc, err := auth.SaveToken("ak_newdotfilekey1")
	if err != nil || loc.Keychain {
		t.Fatalf("SaveToken: got (%+v, %v), want the file", loc, err)
	}
	if info, err := os.Lstat(credPath(home)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link must stay: %v", err)
	}
	if data, _ := os.ReadFile(target); string(data) != "ak_newdotfilekey1\n" {
		t.Fatalf("link target: got %q", data)
	}
	token, source, err := auth.Resolve("")
	if err != nil || token != "ak_newdotfilekey1" || source != auth.SourceFile {
		t.Fatalf("Resolve: got (%q, %v, %v)", token, source, err)
	}
}
