package auth_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/auth"
	"github.com/zalando/go-keyring"
)

// Story 94.6: auth login saves the key in the keychain where one is usable
// and no credentials file exists; connect keeps its key in the file. These
// tests use the in memory keychain (auth.MockKeychainForTest); every other
// test in the binary sees no keychain, so the file rung behaves as before.

func credPath(home string) string { return filepath.Join(home, ".archivist", "credentials") }

// markerPath is the keychain marker, ~/.archivist/keychain.
func markerPath(home string) string { return filepath.Join(home, ".archivist", "keychain") }

// markKeychain writes the marker, as a keychain save does (for tests whose
// keychain refuses every call: a saved item that cannot be read now).
func markKeychain(t *testing.T, home string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(markerPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(home), []byte("marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func marked(home string) bool {
	_, err := os.Lstat(markerPath(home))
	return err == nil
}

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
	// The marker records the item; it holds no secret.
	if data, err := os.ReadFile(markerPath(home)); err != nil || len(data) == 0 || string(data) == "ak_keychaintoken1\n" {
		t.Fatalf("marker: %q %v", data, err)
	}
	if fi, err := os.Stat(markerPath(home)); runtimeHasUnixModes() && (err != nil || fi.Mode().Perm() != 0o600) {
		t.Fatalf("marker mode: %v %v", fi, err)
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

// Once the credentials file exists it stays the store: a new key replaces
// it there and the keychain is not used.
func TestSaveTokenKeepsAnExistingFileAsTheStore(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	writeCredFile(t, home, "ak_legacyfiletoken\n")
	auth.MockKeychainForTest(t, false)

	loc, err := auth.SaveToken("ak_newfiletoken12")
	if err != nil || loc.Keychain || loc.Path != credPath(home) {
		t.Fatalf("SaveToken: got (%+v, %v), want the file", loc, err)
	}
	if data, _ := os.ReadFile(credPath(home)); string(data) != "ak_newfiletoken12\n" {
		t.Fatalf("file: got %q", data)
	}
	if moved, err := auth.MoveKeychainKeyToFile(); moved || err != nil {
		t.Fatalf("nothing in the keychain: got (%v, %v)", moved, err)
	}
	token, source, err := auth.Resolve("")
	if err != nil || token != "ak_newfiletoken12" || source != auth.SourceFile {
		t.Fatalf("Resolve: got (%q, %v, %v)", token, source, err)
	}
}

// SaveTokenToFile never uses the keychain, even where one is usable.
func TestSaveTokenToFileSkipsTheKeychain(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	loc, err := auth.SaveTokenToFile("ak_connectfilekey1")
	if err != nil || loc.Keychain || loc.Path != credPath(home) {
		t.Fatalf("got (%+v, %v)", loc, err)
	}
	if runtimeHasUnixModes() {
		if fi, err := os.Stat(credPath(home)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("file: %v, mode %v", err, fi)
		}
	}
	if existed, _, _ := auth.DeleteCredentials(); !existed {
		t.Fatal("no key saved")
	}
	// Only the file was written: the keychain held nothing.
	if _, err := auth.SaveTokenToFile("ak_connectfilekey1"); err != nil {
		t.Fatal(err)
	}
	if _, where, _ := auth.DeleteCredentials(); where != credPath(home) {
		t.Fatalf("removed from %q, want only the file", where)
	}
}

// MoveKeychainKeyToFile writes the keychain key to the 0600 file and then
// deletes the item; a keychain that fails keeps the item and says so.
func TestMoveKeychainKeyToFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	if moved, err := auth.MoveKeychainKeyToFile(); moved || err != nil {
		t.Fatalf("nothing saved: got (%v, %v)", moved, err)
	}
	auth.MockKeychainForTest(t, false)
	if loc, err := auth.SaveToken("ak_tobemoved12345"); err != nil || !loc.Keychain {
		t.Fatalf("SaveToken: %+v %v", loc, err)
	}
	moved, err := auth.MoveKeychainKeyToFile()
	if !moved || err != nil {
		t.Fatalf("move: got (%v, %v)", moved, err)
	}
	if data, _ := os.ReadFile(credPath(home)); string(data) != "ak_tobemoved12345\n" {
		t.Fatalf("file: got %q", data)
	}
	if runtimeHasUnixModes() {
		if fi, err := os.Stat(credPath(home)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("file: %v, mode %v", err, fi)
		}
	}
	if _, where, _ := auth.DeleteCredentials(); where != credPath(home) {
		t.Fatalf("the keychain item stayed: removed from %q", where)
	}

	if marked(home) {
		t.Fatal("the marker stayed after the item was moved")
	}

	// A keychain that cannot be read: nothing moves and the error returns.
	auth.MockKeychainForTest(t, true)
	markKeychain(t, home)
	if moved, err := auth.MoveKeychainKeyToFile(); moved || !errors.Is(err, auth.ErrKeychainUnavailable) {
		t.Fatalf("locked: got (%v, %v)", moved, err)
	}
	if _, err := os.Stat(credPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was written: %v", err)
	}
	if !marked(home) {
		t.Fatal("the marker went while the item may remain")
	}
}

func TestAFailingKeychainFallsBackToTheFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, true)

	// Nothing ever saved there (no marker): the keychain is not asked, so
	// a locked one reads as no key.
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("no marker: got %v, want ErrNoToken", err)
	}
	markKeychain(t, home)
	// A saved item that does not answer is not "no key".
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

// The marker gate (Story 94.6 follow-up review): without the marker the
// keychain is never asked, so an item Archivist did not record is not read,
// moved or deleted, and a locked keyring costs nothing on a computer that
// never saved a key there.
func TestTheKeychainIsAskedOnlyWithTheMarker(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	account, err := filepath.Abs(credPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Set("archivist", account, "ak_unrecordeditem1"); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("SavedToken: got %v, want ErrNoToken", err)
	}
	if moved, err := auth.MoveKeychainKeyToFile(); moved || err != nil {
		t.Fatalf("move: got (%v, %v)", moved, err)
	}
	if existed, _, err := auth.DeleteCredentials(); existed || err != nil {
		t.Fatalf("delete: got (%v, %v)", existed, err)
	}
	if got, err := keyring.Get("archivist", account); err != nil || got != "ak_unrecordeditem1" {
		t.Fatalf("the item was touched: %q %v", got, err)
	}
	if _, err := os.Stat(credPath(home)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file was written: %v", err)
	}

	// A locked keyring with nothing recorded: no error anywhere.
	auth.MockKeychainForTest(t, true)
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("locked, no marker: SavedToken got %v", err)
	}
	if existed, _, err := auth.DeleteCredentials(); existed || err != nil {
		t.Fatalf("locked, no marker: delete got (%v, %v)", existed, err)
	}
	if err := auth.DeleteKeychainKey(); err != nil {
		t.Fatalf("locked, no marker: DeleteKeychainKey got %v", err)
	}
}

// An item found gone takes its marker with it, so it is not asked again.
func TestAMissingItemRemovesTheMarker(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	markKeychain(t, home)
	if _, err := auth.SavedToken(); !errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("SavedToken: got %v", err)
	}
	if marked(home) {
		t.Fatal("the marker stayed for a missing item")
	}
	markKeychain(t, home)
	if existed, _, err := auth.DeleteCredentials(); existed || err != nil || marked(home) {
		t.Fatalf("delete: got (%v, %v), marker %v", existed, err, marked(home))
	}
}

// With the file holding the saved key, an item left in the keychain is
// deleted by MoveKeychainKeyToFile; a failure keeps the item and the marker,
// so the next call (the next connect start) tries again.
func TestALeftoverKeychainItemIsRetriedUntilGone(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	if loc, err := auth.SaveToken("ak_leftoveritem123"); err != nil || !loc.Keychain {
		t.Fatalf("SaveToken: %+v %v", loc, err)
	}
	writeCredFile(t, home, "ak_filekeyconnect1\n")
	account, err := filepath.Abs(credPath(home))
	if err != nil {
		t.Fatal(err)
	}

	// The keychain refuses: the item and the marker stay.
	auth.MockKeychainForTest(t, true)
	if moved, err := auth.MoveKeychainKeyToFile(); moved || err == nil {
		t.Fatalf("locked: got (%v, %v), want an error", moved, err)
	}
	if !marked(home) {
		t.Fatal("the marker went while the item remains")
	}

	// It answers again: the leftover goes, the file key stays.
	auth.MockKeychainForTest(t, false)
	if err := keyring.Set("archivist", account, "ak_leftoveritem123"); err != nil {
		t.Fatal(err)
	}
	if moved, err := auth.MoveKeychainKeyToFile(); moved || err != nil {
		t.Fatalf("retry: got (%v, %v)", moved, err)
	}
	if _, err := keyring.Get("archivist", account); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("the leftover item stayed: %v", err)
	}
	if marked(home) {
		t.Fatal("the marker stayed after the item went")
	}
	if token, source, err := auth.Resolve(""); err != nil || token != "ak_filekeyconnect1" || source != auth.SourceFile {
		t.Fatalf("Resolve: got (%q, %v, %v)", token, source, err)
	}
}

// A keychain save whose marker cannot be written is undone: the key goes to
// the file and no unrecorded item stays behind.
func TestAKeychainSaveWithoutItsMarkerUsesTheFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	auth.MockKeychainForTest(t, false)
	// A directory where the marker file goes: the marker write fails.
	if err := os.MkdirAll(filepath.Join(markerPath(home), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	loc, err := auth.SaveToken("ak_markerfails1234")
	if err != nil || loc.Keychain {
		t.Fatalf("SaveToken: got (%+v, %v), want the file", loc, err)
	}
	account, err := filepath.Abs(credPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Get("archivist", account); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("an unrecorded item stayed: %v", err)
	}
	if data, _ := os.ReadFile(credPath(home)); string(data) != "ak_markerfails1234\n" {
		t.Fatalf("file: got %q", data)
	}
}

// DeleteCredentials reports each store on its own: with both failing, the
// error names both; with only the keychain failing, the file still counts as
// removed.
func TestDeleteCredentialsReportsEachStore(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	writeCredFile(t, home, "ak_filekeystore123\n")
	markKeychain(t, home)
	auth.MockKeychainForTest(t, true)
	existed, where, err := auth.DeleteCredentials()
	if !existed || where != credPath(home) || err == nil || !strings.Contains(err.Error(), "the copy of the key in the keychain could not be removed") {
		t.Fatalf("keychain fails: got (%v, %q, %v)", existed, where, err)
	}
	// The file cannot be removed either (a directory with an entry).
	if err := os.MkdirAll(filepath.Join(credPath(home), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	existed, _, err = auth.DeleteCredentials()
	if existed || err == nil || !strings.Contains(err.Error(), "remove credentials file") || !strings.Contains(err.Error(), "keychain could not be removed") {
		t.Fatalf("both fail: got (%v, %v)", existed, err)
	}
}
