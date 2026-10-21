package release

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var (
	testKeysOnce sync.Once
	testKeyA     *rsa.PrivateKey
	testKeyB     *rsa.PrivateKey
)

// testKeys returns two throwaway 3072 bit keys, generated once per run.
func testKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	testKeysOnce.Do(func() {
		var err error
		if testKeyA, err = rsa.GenerateKey(rand.Reader, 3072); err != nil {
			panic(err)
		}
		if testKeyB, err = rsa.GenerateKey(rand.Reader, 3072); err != nil {
			panic(err)
		}
	})
	return testKeyA, testKeyB
}

func pubPEM(t *testing.T, key *rsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func sign(t *testing.T, key *rsa.PrivateKey, data []byte) []byte {
	t.Helper()
	d := sha256.Sum256(data)
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func TestEmbeddedKeyIsRSA4096(t *testing.T) {
	pems, err := EmbeddedKeys()
	if err != nil || len(pems) == 0 {
		t.Fatalf("embedded keys: %d %v", len(pems), err)
	}
	v, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	keys := v.Keys()
	if len(keys) != len(pems) {
		t.Fatalf("%d keys from %d files", len(keys), len(pems))
	}
	for i, k := range keys {
		if k.N.BitLen() != 4096 || k.E != 65537 {
			t.Fatalf("key %d: %d bits, e=%d", i, k.N.BitLen(), k.E)
		}
	}
}

func TestVerify(t *testing.T) {
	a, b := testKeys(t)
	v, err := NewVerifier(pubPEM(t, &a.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("0123  archivist_v9.9.9_linux_amd64.tar.gz\n")
	good := sign(t, a, sums)
	if err := v.Verify(sums, good); err != nil {
		t.Fatalf("good signature: %v", err)
	}
	cases := map[string]struct{ sums, sig []byte }{
		"other key":     {sums, sign(t, b, sums)},
		"changed sums":  {append([]byte("x"), sums...), good},
		"truncated sig": {sums, good[:len(good)-1]},
		"empty sig":     {sums, nil},
		"longer sig":    {sums, append(append([]byte(nil), good...), 0)},
	}
	for name, c := range cases {
		if err := v.Verify(c.sums, c.sig); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	// Any trusted key verifies (key rotation).
	both, err := NewVerifier(pubPEM(t, &b.PublicKey), pubPEM(t, &a.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := both.Verify(sums, good); err != nil {
		t.Fatalf("second trusted key: %v", err)
	}
	var none *Verifier
	if err := none.Verify(sums, good); err == nil {
		t.Fatal("a nil verifier verified")
	}
}

// A signature made the way the release workflow makes it
// (openssl dgst -sha256 -sign) verifies, and not over other content.
func TestVerifyOpenSSLSignature(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}
	dir := t.TempDir()
	key, pub := filepath.Join(dir, "key.pem"), filepath.Join(dir, "pub.pem")
	sums, sig := filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "SHA256SUMS.sig")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("openssl", args...).CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	run("genpkey", "-algorithm", "RSA", "-pkeyopt", "rsa_keygen_bits:3072", "-out", key)
	run("pkey", "-in", key, "-pubout", "-out", pub)
	body := []byte("0123  archivist_v9.9.9_linux_amd64.tar.gz\n")
	if err := os.WriteFile(sums, body, 0o600); err != nil {
		t.Fatal(err)
	}
	run("dgst", "-sha256", "-sign", key, "-out", sig, sums)

	pubPEMBytes, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	sigBytes, err := os.ReadFile(sig)
	if err != nil {
		t.Fatal(err)
	}
	v, err := NewVerifier(pubPEMBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Verify(body, sigBytes); err != nil {
		t.Fatalf("openssl signature: %v", err)
	}
	if err := v.Verify(append([]byte("x"), body...), sigBytes); err == nil {
		t.Fatal("openssl signature verified over changed checksums")
	}
}

func TestNewVerifierRejects(t *testing.T) {
	a, _ := testKeys(t)
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	two := append(pubPEM(t, &a.PublicKey), pubPEM(t, &a.PublicKey)...)
	for name, p := range map[string][]byte{
		"2048 bit key": pubPEM(t, &small.PublicKey),
		"garbage":      []byte("not a key"),
		"PKCS#1 block": pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(&a.PublicKey)}),
		"two blocks":   two,
	} {
		if _, err := NewVerifier(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := NewVerifier(); err == nil {
		t.Error("no keys accepted")
	}
}

func TestVersionOrdering(t *testing.T) {
	ordered := []string{
		"v0.2.9", "v0.2.10", "v0.2.34", "v0.2.35-alpha", "v0.2.35-alpha.1", "v0.2.35-alpha.beta",
		"v0.2.35-beta", "v0.2.35-beta.2", "v0.2.35-beta.11", "v0.2.35-rc.1", "v0.2.35", "v0.3.0", "v1.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			a, err := ParseTag(ordered[i])
			if err != nil {
				t.Fatal(err)
			}
			b, err := ParseTag(ordered[j])
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := a.Compare(b); got != want {
				t.Errorf("%s vs %s: %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestParseTagStrict(t *testing.T) {
	for _, bad := range []string{
		"", "v", "0.2.34", "v0.2", "v0.2.34.1", "v01.2.3", "v0.2.x", "v0.2.34-", "v0.2.34-a..b",
		"v0.2.34-a-b", "v0.2.34+build", "v0.2.34 ", "v0.2.34/../x", "v0.2.34-01", "V0.2.34",
		"v99999999999999999999.0.0", "v1.2.3-.", "v1.2.3-rc..1",
	} {
		if _, err := ParseTag(bad); err == nil {
			t.Errorf("ParseTag(%q) accepted", bad)
		}
	}
	if _, err := ParseTag("v1.2.3-0a"); err != nil {
		t.Errorf("ParseTag(v1.2.3-0a): %v", err)
	}
	v, err := ParseTag("v0.2.35-rc.1")
	if err != nil || v.String() != "0.2.35-rc.1" {
		t.Fatalf("%v %v", v, err)
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"0.2.34":          "0.2.34",
		"v0.2.34":         "0.2.34",
		"0.2.35-snapshot": "0.2.35-snapshot",
		"v0.2.35-0.20261020120000-abcdef123456+dirty": "0.2.35-0.20261020120000-abcdef123456",
	} {
		v, err := ParseVersion(in)
		if err != nil || v.String() != want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %s", in, v, err, want)
		}
	}
	for _, bad := range []string{"dev", "", "unknown", "0.2", "0.2.34+", "(devel)"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	snap, _ := ParseVersion("0.2.35-snapshot")
	rel, _ := ParseTag("v0.2.35")
	prev, _ := ParseTag("v0.2.34")
	if snap.Compare(rel) >= 0 || snap.Compare(prev) <= 0 {
		t.Fatal("a snapshot must sort between the previous release and its own")
	}
}

// installerBlock returns the lines between the release key markers of an
// installer script; exactly one marked block must exist.
func installerBlock(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	const begin, end = "# BEGIN ARCHIVIST RELEASE KEY\n", "# END ARCHIVIST RELEASE KEY\n"
	if strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
		t.Fatalf("%s: want one release key block", path)
	}
	i := strings.Index(text, begin) + len(begin)
	j := strings.Index(text, end)
	if j < i {
		t.Fatalf("%s: markers out of order", path)
	}
	return strings.Split(strings.TrimSuffix(text[i:j], "\n"), "\n")
}

func embeddedKeys(t *testing.T) []*rsa.PublicKey {
	t.Helper()
	v, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	return v.Keys()
}

// Both installers embed one of the compiled in keys (the current one); a
// change to either side fails here.
func TestInstallersEmbedTheReleaseKey(t *testing.T) {
	trusted := embeddedKeys(t)

	sh := installerBlock(t, "../../scripts/install.sh")
	body := strings.Join(sh, "\n")
	if !strings.HasPrefix(body, "RELEASE_KEY='") || !strings.HasSuffix(body, "'") {
		t.Fatalf("install.sh key block is not RELEASE_KEY='<PEM>':\n%s", body)
	}
	shKey, err := ParsePublicKey([]byte(strings.TrimSuffix(strings.TrimPrefix(body, "RELEASE_KEY='"), "'") + "\n"))
	if err != nil {
		t.Fatalf("install.sh key: %v", err)
	}

	ps := installerBlock(t, "../../scripts/install.ps1")
	if len(ps) != 2 {
		t.Fatalf("install.ps1 key block has %d lines, want 2", len(ps))
	}
	mod := regexp.MustCompile(`^\$ReleaseModulus = '([A-Za-z0-9+/=]+)'$`).FindStringSubmatch(ps[0])
	exp := regexp.MustCompile(`^\$ReleaseExponent = '([A-Za-z0-9+/=]+)'$`).FindStringSubmatch(ps[1])
	if mod == nil || exp == nil {
		t.Fatalf("install.ps1 key block:\n%s", strings.Join(ps, "\n"))
	}
	n, err := base64.StdEncoding.DecodeString(mod[1])
	if err != nil {
		t.Fatal(err)
	}
	e, err := base64.StdEncoding.DecodeString(exp[1])
	if err != nil {
		t.Fatal(err)
	}
	// .NET RSAParameters: big endian, no leading zero byte.
	if len(n) == 0 || n[0] == 0 || len(e) == 0 || e[0] == 0 {
		t.Fatal("install.ps1 modulus or exponent has a leading zero byte")
	}
	psKey := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}

	if !shKey.Equal(psKey) {
		t.Fatal("install.sh and install.ps1 embed different keys")
	}
	found := false
	for _, k := range trusted {
		if k.Equal(shKey) {
			found = true
		}
	}
	if !found {
		t.Fatal("the installers' key is not one of internal/release/keys")
	}
	if len(trusted) == 1 && !bytes.Equal(trusted[0].N.Bytes(), n) {
		t.Fatal("install.ps1 modulus differs from the release key")
	}
}
