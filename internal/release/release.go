// Package release checks downloaded releases: the signature on a release's
// SHA256SUMS file (RSA PKCS#1 v1.5 over SHA-256, made by the release
// workflow with the key whose public half is in keys/) and release version
// ordering.
//
// The trusted keys are compiled in. install.sh and install.ps1 embed the
// current key too; a test keeps them equal to one of these keys.
package release

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"
)

// MinKeyBits is the smallest RSA key accepted as a release key.
const MinKeyBits = 3072

//go:embed keys/*.pem
var keyFiles embed.FS

// ErrBadSignature means no trusted key verifies the signature.
var ErrBadSignature = errors.New("the release checksums are not signed by a trusted key")

// Verifier checks SHA256SUMS signatures against a fixed set of keys.
type Verifier struct {
	keys []*rsa.PublicKey
}

// NewVerifier returns a verifier trusting each PEM encoded public key
// (SubjectPublicKeyInfo, RSA, at least MinKeyBits). Any of them verifies.
func NewVerifier(pems ...[]byte) (*Verifier, error) {
	if len(pems) == 0 {
		return nil, errors.New("no release keys")
	}
	v := &Verifier{}
	for i, p := range pems {
		key, err := ParsePublicKey(p)
		if err != nil {
			return nil, fmt.Errorf("release key %d: %w", i+1, err)
		}
		v.keys = append(v.keys, key)
	}
	return v, nil
}

// ParsePublicKey decodes one PEM "PUBLIC KEY" block holding an RSA key of
// at least MinKeyBits.
func ParsePublicKey(p []byte) (*rsa.PublicKey, error) {
	block, rest := pem.Decode(p)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM PUBLIC KEY block")
	}
	if extra, _ := pem.Decode(rest); extra != nil {
		return nil, errors.New("more than one PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	if key.N.BitLen() < MinKeyBits {
		return nil, fmt.Errorf("RSA key of %d bits; at least %d needed", key.N.BitLen(), MinKeyBits)
	}
	return key, nil
}

// Keys returns the verifier's keys.
func (v *Verifier) Keys() []*rsa.PublicKey {
	return append([]*rsa.PublicKey(nil), v.keys...)
}

// Verify reports whether sig is a signature of sums by any trusted key.
func (v *Verifier) Verify(sums, sig []byte) error {
	if v == nil || len(v.keys) == 0 {
		return ErrBadSignature
	}
	if len(sig) == 0 {
		return ErrBadSignature
	}
	digest := sha256.Sum256(sums)
	for _, key := range v.keys {
		if len(sig) != key.Size() {
			continue
		}
		if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) == nil {
			return nil
		}
	}
	return ErrBadSignature
}

// EmbeddedKeys returns the PEM files compiled into the binary, in name order.
func EmbeddedKeys() ([][]byte, error) {
	names, err := fs.Glob(keyFiles, "keys/*.pem")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	out := make([][]byte, 0, len(names))
	for _, n := range names {
		b, err := keyFiles.ReadFile(n)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

var (
	defaultOnce     sync.Once
	defaultVerifier *Verifier
	defaultErr      error
)

// Default returns the verifier for the compiled in keys.
func Default() (*Verifier, error) {
	defaultOnce.Do(func() {
		pems, err := EmbeddedKeys()
		if err != nil {
			defaultErr = err
			return
		}
		defaultVerifier, defaultErr = NewVerifier(pems...)
	})
	return defaultVerifier, defaultErr
}

// Verify checks sig over sums with the compiled in keys.
func Verify(sums, sig []byte) error {
	v, err := Default()
	if err != nil {
		return err
	}
	return v.Verify(sums, sig)
}
