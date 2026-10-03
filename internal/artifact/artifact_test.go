package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAllowedTypes(t *testing.T) {
	root := t.TempDir()
	cases := map[string]struct {
		body []byte
		mt   string
	}{
		"report.pdf":     {[]byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj\n"), "application/pdf"},
		"notes.txt":      {[]byte("plain text, café\n"), "text/plain"},
		"README.MD":      {[]byte("# Title\n"), "text/markdown"},
		"table.csv":      {[]byte("a,b\n1,2\n"), "text/csv"},
		"sub/data.json":  {[]byte(`{"a":[1,2,{"b":null}]}`), "application/json"},
		"deep/er/x.json": {[]byte(`[]`), "application/json"},
	}
	for name, c := range cases {
		write(t, filepath.Join(root, name), c.body)
		for _, p := range []string{name, filepath.Join(root, name), "./" + name} {
			f, err := Load(root, p)
			if err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			sum := sha256.Sum256(c.body)
			if f.Name != filepath.Base(name) || f.MediaType != c.mt || string(f.Data) != string(c.body) || f.Digest != hex.EncodeToString(sum[:]) {
				t.Fatalf("%s: got %+v", p, f)
			}
		}
	}
}

func TestLoadRefusals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink cases need unix")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "cwd")
	write(t, filepath.Join(root, "ok.txt"), []byte("ok"))
	write(t, filepath.Join(parent, "secret.txt"), []byte("outside"))
	write(t, filepath.Join(root, "empty.txt"), nil)
	write(t, filepath.Join(root, "image.png"), []byte("\x89PNG"))
	write(t, filepath.Join(root, "noext"), []byte("x"))
	write(t, filepath.Join(root, "fake.pdf"), []byte("not a pdf"))
	write(t, filepath.Join(root, "nul.txt"), []byte("a\x00b"))
	write(t, filepath.Join(root, "latin1.md"), []byte("caf\xe9"))
	write(t, filepath.Join(root, "bad.json"), []byte("{nope"))
	write(t, filepath.Join(root, "csvbin.csv"), []byte{0xff, 0xfe, 0x00})
	big := make([]byte, MaxBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	write(t, filepath.Join(root, "big.txt"), big)
	write(t, filepath.Join(root, "real", "inner.txt"), []byte("inner"))
	if err := os.Symlink(filepath.Join(root, "ok.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(parent, filepath.Join(root, "up")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("n", 201) + ".txt"
	write(t, filepath.Join(root, long), []byte("x"))

	cases := map[string]string{
		filepath.Join(parent, "secret.txt"): "outside",
		"../secret.txt":                     "'..'",
		"sub/../../secret.txt":              "'..'",
		"real/../ok.txt":                    "'..'",
		"/etc/passwd":                       "outside",
		root:                                "outside",
		".":                                 "outside",
		"":                                  "empty",
		"link.txt":                          "symbolic link",
		"escape.txt":                        "symbolic link",
		"linkdir/inner.txt":                 "symbolic link",
		"up/secret.txt":                     "symbolic link",
		"missing.txt":                       "does not exist",
		"dir.txt":                           "not a regular file",
		"empty.txt":                         "empty",
		"big.txt":                           "exceeds 10 MiB",
		"image.png":                         "unsupported type",
		"noext":                             "unsupported type",
		"fake.pdf":                          "not a PDF",
		"nul.txt":                           "not UTF-8 text",
		"latin1.md":                         "not UTF-8 text",
		"csvbin.csv":                        "not UTF-8 text",
		"bad.json":                          "not valid JSON",
		long:                                "longer than 200",
		"ok.txt/x.txt":                      "not a directory",
	}
	for p, want := range cases {
		f, err := Load(root, p)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) = %+v, %v; want error containing %q", p, f, err, want)
		}
	}
	if _, err := Load("relative", "ok.txt"); err == nil {
		t.Error("relative root accepted")
	}
	if _, err := Load(root, "real/inner.txt"); err != nil {
		t.Errorf("regular file in a real subdirectory refused: %v", err)
	}
}

// The root's own symlinks are resolved: a path under the root as written or
// as resolved is inside.
func TestLoadResolvesRootSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink cases need unix")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	write(t, filepath.Join(real, "a.md"), []byte("# a"))
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.md", filepath.Join(alias, "a.md"), filepath.Join(real, "a.md")} {
		if _, err := Load(alias, p); err != nil {
			t.Errorf("Load(alias, %q): %v", p, err)
		}
	}
}
