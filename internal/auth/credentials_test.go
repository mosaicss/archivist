package auth_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mosaicss/archivist/internal/auth"
)

// sandboxHome points os.UserHomeDir at a temp dir so the file rung never
// reads the developer's real ~/.archivist/credentials. UserHomeDir reads
// $HOME on both darwin and linux, so t.Setenv sandboxes both CI and dev.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// writeCredFile writes ~/.archivist/credentials inside the sandboxed home.
func writeCredFile(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".archivist")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "credentials")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestResolvePrecedence(t *testing.T) {
	cases := []struct {
		name       string
		flag       string
		env        string
		file       string // credentials file content; "" = no file
		hasFile    bool
		wantToken  string
		wantSource auth.Source
		wantErr    error
	}{
		{
			name: "flag wins over env and file",
			flag: "ak_flagvalue", env: "ak_envvalue", file: "ak_filevalue\n", hasFile: true,
			wantToken: "ak_flagvalue", wantSource: auth.SourceFlag,
		},
		{
			name: "env wins over file",
			env:  "ak_envvalue", file: "ak_filevalue\n", hasFile: true,
			wantToken: "ak_envvalue", wantSource: auth.SourceEnv,
		},
		{
			name: "file alone works",
			file: "ak_filevalue\n", hasFile: true,
			wantToken: "ak_filevalue", wantSource: auth.SourceFile,
		},
		{
			name: "file content is trimmed",
			file: "  ak_filevalue  \n\n", hasFile: true,
			wantToken: "ak_filevalue", wantSource: auth.SourceFile,
		},
		{
			name:    "none set returns ErrNoToken",
			wantErr: auth.ErrNoToken,
		},
		{
			name:    "missing file falls through cleanly",
			hasFile: false,
			wantErr: auth.ErrNoToken,
		},
		{
			name: "whitespace only file means no credential",
			file: "   \n\t\n", hasFile: true,
			wantErr: auth.ErrNoToken,
		},
		{
			name: "empty file means no credential",
			file: "", hasFile: true,
			wantErr: auth.ErrNoToken,
		},
		{
			name: "malformed token in file is a format error",
			file: "not_a_token\n", hasFile: true,
			wantErr: auth.ErrInvalidFormat,
		},
		{
			name: "malformed flag token is a format error",
			flag: "bogus", env: "ak_envvalue",
			wantErr: auth.ErrInvalidFormat,
		},
		{
			name: "mc_pat_ prefix accepted from file",
			file: "mc_pat_legacy\n", hasFile: true,
			wantToken: "mc_pat_legacy", wantSource: auth.SourceFile,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := sandboxHome(t)
			t.Setenv("ARCHIVIST_TOKEN", tc.env)
			if tc.hasFile {
				writeCredFile(t, home, tc.file)
			}

			token, source, err := auth.Resolve(tc.flag)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if token != tc.wantToken {
				t.Errorf("token: got %q, want %q", token, tc.wantToken)
			}
			if source != tc.wantSource {
				t.Errorf("source: got %v, want %v", source, tc.wantSource)
			}
		})
	}
}

func TestResolveMalformedFileTokenNamesFile(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	path := writeCredFile(t, home, "garbage_token\n")

	_, _, err := auth.Resolve("")
	if !errors.Is(err, auth.ErrInvalidFormat) {
		t.Fatalf("want ErrInvalidFormat, got %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the credentials file %q, got: %v", path, err)
	}
}

func TestResolveFileReadErrorSurfaces(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	// Make the credentials path a non-empty DIRECTORY: os.ReadFile fails with
	// a non-IsNotExist error, which must surface rather than mask as ErrNoToken.
	dir := filepath.Join(home, ".archivist", "credentials")
	if err := os.MkdirAll(filepath.Join(dir, "x"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, _, err := auth.Resolve("")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, auth.ErrNoToken) {
		t.Fatalf("read error must not be masked as ErrNoToken, got: %v", err)
	}
}

func TestResolveSourceStrings(t *testing.T) {
	cases := map[auth.Source]string{
		auth.SourceFlag: "flag",
		auth.SourceEnv:  "env",
		auth.SourceFile: "file",
	}
	for src, want := range cases {
		if got := src.String(); got != want {
			t.Errorf("Source(%d).String(): got %q, want %q", src, got, want)
		}
	}
}

func TestCredentialsPath(t *testing.T) {
	home := sandboxHome(t)
	path, err := auth.CredentialsPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, ".archivist", "credentials")
	if path != want {
		t.Errorf("got %q, want %q", path, want)
	}
}

func TestSaveTokenWritesFileWithModes(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")

	path, err := auth.SaveToken("ak_savedtoken")
	if err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	want := filepath.Join(home, ".archivist", "credentials")
	if path != want {
		t.Errorf("path: got %q, want %q", path, want)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "ak_savedtoken\n" {
		t.Errorf("content: got %q, want token plus trailing newline", string(data))
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode: got %o, want 0600", fi.Mode().Perm())
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode: got %o, want 0700", di.Mode().Perm())
	}

	// Round trip: the saved token resolves from the file rung.
	token, source, err := auth.Resolve("")
	if err != nil {
		t.Fatalf("Resolve after save: %v", err)
	}
	if token != "ak_savedtoken" || source != auth.SourceFile {
		t.Errorf("round trip: got (%q, %v), want (ak_savedtoken, SourceFile)", token, source)
	}
}

func TestSaveTokenOverwritesExisting(t *testing.T) {
	home := sandboxHome(t)
	writeCredFile(t, home, "ak_oldtoken\n")

	if _, err := auth.SaveToken("ak_newtoken"); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	t.Setenv("ARCHIVIST_TOKEN", "")
	token, _, err := auth.Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if token != "ak_newtoken" {
		t.Errorf("got %q, want ak_newtoken", token)
	}
}

func TestDeleteCredentials(t *testing.T) {
	home := sandboxHome(t)
	path := writeCredFile(t, home, "ak_tokenvalue\n")

	existed, gotPath, err := auth.DeleteCredentials()
	if err != nil {
		t.Fatalf("DeleteCredentials: %v", err)
	}
	if !existed {
		t.Error("existed: got false, want true")
	}
	if gotPath != path {
		t.Errorf("path: got %q, want %q", gotPath, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be gone, stat err: %v", err)
	}
}

func TestDeleteCredentialsIdempotent(t *testing.T) {
	sandboxHome(t)
	existed, _, err := auth.DeleteCredentials()
	if err != nil {
		t.Fatalf("DeleteCredentials on missing file: %v", err)
	}
	if existed {
		t.Error("existed: got true, want false for missing file")
	}
}

func TestMaskToken(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ak_1234567890abcdefxyz", "ak_1234567...xyz"},
		{"mc_pat_12345678abc", "mc_pat_123...abc"},
		{"short", "ak_???"},
		{"", "ak_???"},
	}
	for _, tc := range cases {
		if got := auth.MaskToken(tc.in); got != tc.want {
			t.Errorf("MaskToken(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- ResolveToken wrapper: the seven existing call sites keep this signature ---

func TestResolveTokenFlagWins(t *testing.T) {
	sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_env_value")
	got, err := auth.ResolveToken("mc_pat_flag_value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "mc_pat_flag_value" {
		t.Errorf("got %q, want mc_pat_flag_value", got)
	}
}

func TestResolveTokenEnvFallback(t *testing.T) {
	sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "mc_pat_env_value")
	got, err := auth.ResolveToken("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "mc_pat_env_value" {
		t.Errorf("got %q, want mc_pat_env_value", got)
	}
}

func TestResolveTokenFileFallback(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	writeCredFile(t, home, "ak_fromfile\n")
	got, err := auth.ResolveToken("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ak_fromfile" {
		t.Errorf("got %q, want ak_fromfile", got)
	}
}

func TestResolveTokenNeitherExits(t *testing.T) {
	sandboxHome(t)
	t.Setenv("ARCHIVIST_TOKEN", "")
	_, err := auth.ResolveToken("")
	if !errors.Is(err, auth.ErrNoToken) {
		t.Errorf("expected ErrNoToken, got %v", err)
	}
}

func TestTokenFormatValidation(t *testing.T) {
	validToken := "mc_pat_validtoken"
	if err := auth.ValidateTokenFormat(validToken); err != nil {
		t.Errorf("expected valid token to pass, got %v", err)
	}

	invalidToken := "wrong_prefix_notaclittoken"
	if err := auth.ValidateTokenFormat(invalidToken); err == nil {
		t.Error("expected invalid token to fail, got nil")
	}

	emptyToken := ""
	if err := auth.ValidateTokenFormat(emptyToken); err == nil {
		t.Error("expected empty token to fail, got nil")
	}
}
