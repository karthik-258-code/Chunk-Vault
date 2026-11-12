// Package keys implements chunkvault's key management: a passphrase is
// stretched with Argon2id into a key-encryption key (KEK), which wraps a random
// 256-bit master key with AES-256-GCM; the encryption and MAC subkeys used by
// package codec are derived from the master key with HKDF-SHA-256.
//
// Only the salt, the KDF parameters and the wrapped (encrypted) master key are
// ever stored. The passphrase, the KEK and the master key never are.
//
// Because the master key is random and merely wrapped, changing the passphrase
// only needs the small wrapped key to be rewritten, not the whole repository.
package keys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

const (
	// KeySize is the size in bytes of the master key, the KEK and each subkey.
	KeySize = 32
	// SaltSize is the size in bytes of the Argon2id salt.
	SaltSize = 32

	nonceSize = 12

	// AlgorithmArgon2id is the only supported KDF.
	AlgorithmArgon2id = "argon2id"

	// wrapAAD domain-separates wrapped-key ciphertexts from every other use of
	// AES-GCM in the repository.
	wrapAAD = "chunkvault/v1/wrapped-master-key"

	infoEnc = "chunkvault/v1/enc"
	infoMac = "chunkvault/v1/mac"
)

// Limits on KDF parameters read from a repository config. They exist so that a
// hostile or corrupted config cannot make the CLI allocate unbounded memory or
// run for hours; they are far above any sensible setting.
const (
	maxKDFTime      = 64
	maxKDFMemoryKiB = 4 << 20 // 4 GiB
	maxKDFThreads   = 64
)

var (
	// ErrWrongPassphrase is returned by Unwrap when the passphrase does not
	// unlock the wrapped key.
	ErrWrongPassphrase = errors.New("keys: wrong passphrase (or the repository key material is corrupt)")
	// ErrInvalidParams is returned for unusable KDF parameters or key material.
	ErrInvalidParams = errors.New("keys: invalid key parameters")
)

// KDFParams are the Argon2id cost parameters. They are stored in the
// repository config so that old repositories stay readable if the defaults are
// raised.
type KDFParams struct {
	// Algorithm must be AlgorithmArgon2id.
	Algorithm string `json:"algorithm"`
	// Time is the number of passes over the memory.
	Time uint32 `json:"time"`
	// MemoryKiB is the memory cost in KiB.
	MemoryKiB uint32 `json:"memory_kib"`
	// Threads is the degree of parallelism.
	Threads uint8 `json:"threads"`
}

// DefaultKDFParams returns time=3, memory=64 MiB, threads=4.
func DefaultKDFParams() KDFParams {
	return KDFParams{Algorithm: AlgorithmArgon2id, Time: 3, MemoryKiB: 64 * 1024, Threads: 4}
}

// Validate reports whether p is usable and within sane limits.
func (p KDFParams) Validate() error {
	switch {
	case p.Algorithm != AlgorithmArgon2id:
		return fmt.Errorf("%w: unsupported KDF %q", ErrInvalidParams, p.Algorithm)
	case p.Time < 1 || p.Time > maxKDFTime:
		return fmt.Errorf("%w: argon2id time %d outside 1..%d", ErrInvalidParams, p.Time, maxKDFTime)
	case p.Threads < 1 || p.Threads > maxKDFThreads:
		return fmt.Errorf("%w: argon2id threads %d outside 1..%d", ErrInvalidParams, p.Threads, maxKDFThreads)
	case p.MemoryKiB < 8*uint32(p.Threads) || p.MemoryKiB > maxKDFMemoryKiB:
		return fmt.Errorf("%w: argon2id memory %d KiB outside %d..%d", ErrInvalidParams, p.MemoryKiB, 8*uint32(p.Threads), maxKDFMemoryKiB)
	}
	return nil
}

// Wrapped is the persisted form of an encrypted master key.
type Wrapped struct {
	// Salt is the random Argon2id salt.
	Salt []byte `json:"salt"`
	// KDF are the Argon2id parameters the KEK was derived with.
	KDF KDFParams `json:"kdf"`
	// WrappedKey is the random AES-GCM nonce followed by the master key
	// ciphertext and its authentication tag.
	WrappedKey []byte `json:"wrapped_key"`
}

// Validate checks the structure of w without needing the passphrase.
func (w Wrapped) Validate() error {
	if err := w.KDF.Validate(); err != nil {
		return err
	}
	if len(w.Salt) != SaltSize {
		return fmt.Errorf("%w: salt is %d bytes, want %d", ErrInvalidParams, len(w.Salt), SaltSize)
	}
	if want := nonceSize + KeySize + 16; len(w.WrappedKey) != want {
		return fmt.Errorf("%w: wrapped key is %d bytes, want %d", ErrInvalidParams, len(w.WrappedKey), want)
	}
	return nil
}

// Master is a repository's master key. Use Derive to obtain the subkeys.
type Master [KeySize]byte

// Subkeys are the keys derived from a Master.
type Subkeys struct {
	// Enc is the AES-256-GCM key.
	Enc []byte
	// Mac is the HMAC-SHA-256 key used to derive storage keys.
	Mac []byte
}

// NewWrapped generates a random master key, wraps it under passphrase using
// kdf, and returns both the persistable form and the master key itself.
func NewWrapped(passphrase []byte, kdf KDFParams) (Wrapped, Master, error) {
	var m Master
	if err := kdf.Validate(); err != nil {
		return Wrapped{}, m, err
	}
	if len(passphrase) == 0 {
		return Wrapped{}, m, fmt.Errorf("%w: empty passphrase", ErrInvalidParams)
	}
	if _, err := io.ReadFull(rand.Reader, m[:]); err != nil {
		return Wrapped{}, m, fmt.Errorf("keys: generate master key: %w", err)
	}
	w := Wrapped{Salt: make([]byte, SaltSize), KDF: kdf}
	if _, err := io.ReadFull(rand.Reader, w.Salt); err != nil {
		return Wrapped{}, Master{}, fmt.Errorf("keys: generate salt: %w", err)
	}
	aead, err := kekAEAD(passphrase, w.Salt, kdf)
	if err != nil {
		return Wrapped{}, Master{}, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return Wrapped{}, Master{}, fmt.Errorf("keys: generate nonce: %w", err)
	}
	w.WrappedKey = aead.Seal(nonce, nonce, m[:], []byte(wrapAAD))
	return w, m, nil
}

// Unwrap recovers the master key using passphrase. It returns
// ErrWrongPassphrase if the passphrase is wrong.
func (w Wrapped) Unwrap(passphrase []byte) (Master, error) {
	var m Master
	if err := w.Validate(); err != nil {
		return m, err
	}
	aead, err := kekAEAD(passphrase, w.Salt, w.KDF)
	if err != nil {
		return m, err
	}
	pt, err := aead.Open(nil, w.WrappedKey[:nonceSize], w.WrappedKey[nonceSize:], []byte(wrapAAD))
	if err != nil {
		return m, ErrWrongPassphrase
	}
	copy(m[:], pt)
	return m, nil
}

// Derive returns the encryption and MAC subkeys for m using HKDF-SHA-256 with
// distinct context strings, so neither key reveals anything about the other.
func (m Master) Derive() (Subkeys, error) {
	var s Subkeys
	for _, d := range []struct {
		dst  *[]byte
		info string
	}{{&s.Enc, infoEnc}, {&s.Mac, infoMac}} {
		k := make([]byte, KeySize)
		if _, err := io.ReadFull(hkdf.New(sha256.New, m[:], nil, []byte(d.info)), k); err != nil {
			return Subkeys{}, fmt.Errorf("keys: derive subkey: %w", err)
		}
		*d.dst = k
	}
	return s, nil
}

// kekAEAD derives the KEK from passphrase and returns an AES-256-GCM AEAD
// keyed with it.
func kekAEAD(passphrase, salt []byte, p KDFParams) (cipher.AEAD, error) {
	kek := argon2.IDKey(passphrase, salt, p.Time, p.MemoryKiB, p.Threads, KeySize)
	defer clear(kek)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("keys: init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keys: init gcm: %w", err)
	}
	return aead, nil
}
