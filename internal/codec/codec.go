// Package codec turns plaintext chunks into the blobs stored in a chunkvault
// repository and back: optional zstd compression followed by optional
// AES-256-GCM encryption, behind a small versioned header.
//
// # Blob format
//
//	offset  size  field
//	0       4     magic, see Magic
//	4       1     format version (Version)
//	5       1     flags: FlagCompressed, FlagEncrypted
//	6       12    nonce (only if FlagEncrypted)
//	...     n     payload: zstd frame if compressed, else the raw bytes;
//	              AES-256-GCM ciphertext followed by the 16 byte tag if encrypted
//
// When encrypting, the GCM additional authenticated data is the 6 byte header
// followed by the 32 byte storage key the blob is filed under. The tag
// therefore also authenticates the flags and stops a blob being swapped in
// under a different key.
//
// # Storage keys
//
// A blob is filed under Codec.Key(plaintext): the plaintext's SHA-256 for an
// unencrypted codec and HMAC-SHA-256 under a secret MAC key for an encrypted
// one, so that equality of chunks is not visible to someone who only has the
// repository. Decode recomputes the key from the decoded plaintext and rejects
// a mismatch, so a blob can only ever decode to the content it is named for.
package codec

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/karthik-258-code/Chunk-Vault/internal/store"
)

// Magic identifies a chunkvault blob.
const Magic = "CVB\x01"

// Version is the blob format version written by Encode.
const Version = 1

// Flag bits in the header.
const (
	// FlagCompressed marks a zstd compressed payload.
	FlagCompressed byte = 1 << iota
	// FlagEncrypted marks an AES-256-GCM encrypted payload.
	FlagEncrypted

	knownFlags = FlagCompressed | FlagEncrypted
)

const (
	headerLen = len(Magic) + 2
	nonceLen  = 12
	tagLen    = 16

	// KeySize is the size of the encryption and MAC keys.
	KeySize = 32
	// MaxPlaintextSize is the largest plaintext Decode will produce. It bounds
	// the memory a hostile blob can make the decompressor allocate.
	MaxPlaintextSize = 1 << 30
	// MaxWindowSize is the zstd window Encode uses and the largest window
	// Decode accepts. Capping it stops a hostile frame header from making the
	// decoder allocate a large history buffer before producing any output.
	MaxWindowSize = 8 << 20
)

var (
	// ErrFormat is returned for a blob that is not a well-formed chunkvault
	// blob: bad magic, unsupported version, unknown flags or truncation.
	ErrFormat = errors.New("codec: malformed blob")
	// ErrMode is returned when a blob's encryption flag does not match the
	// codec: an encrypted blob given to a codec without keys, or the reverse.
	ErrMode = errors.New("codec: blob encryption mode does not match repository")
	// ErrAuth is returned when GCM authentication fails: the blob was modified,
	// or it was decrypted with the wrong key or bound to the wrong storage key.
	ErrAuth = errors.New("codec: authentication failed (corrupt blob, wrong key or wrong storage key)")
	// ErrHashMismatch is returned when the decoded plaintext does not hash to
	// the storage key it was filed under.
	ErrHashMismatch = errors.New("codec: plaintext does not match its storage key")
	// ErrTooLarge is returned when the plaintext would exceed the size limit
	// given to Decode.
	ErrTooLarge = errors.New("codec: plaintext exceeds size limit")
)

// Keys are the secret subkeys of an encrypted repository.
type Keys struct {
	// Enc is the AES-256 key.
	Enc []byte
	// Mac is the HMAC-SHA-256 key that derives storage keys.
	Mac []byte
}

// Options configures a Codec.
type Options struct {
	// Compress enables zstd compression.
	Compress bool
	// Keys enables encryption when non-nil.
	Keys *Keys
}

// Codec encodes and decodes blobs. It is safe for concurrent use.
type Codec struct {
	compress bool
	aead     cipher.AEAD // nil if not encrypting
	mac      []byte
}

// New builds a Codec. It returns an error if the keys are not KeySize bytes.
func New(opts Options) (*Codec, error) {
	c := &Codec{compress: opts.Compress}
	if k := opts.Keys; k != nil {
		if len(k.Enc) != KeySize || len(k.Mac) != KeySize {
			return nil, fmt.Errorf("codec: keys must be %d bytes, got enc %d and mac %d", KeySize, len(k.Enc), len(k.Mac))
		}
		block, err := aes.NewCipher(k.Enc)
		if err != nil {
			return nil, fmt.Errorf("codec: init cipher: %w", err)
		}
		if c.aead, err = cipher.NewGCM(block); err != nil {
			return nil, fmt.Errorf("codec: init gcm: %w", err)
		}
		c.mac = append([]byte(nil), k.Mac...)
	}
	return c, nil
}

// Encrypted reports whether the codec encrypts blobs.
func (c *Codec) Encrypted() bool { return c.aead != nil }

// Key returns the storage key for plaintext: SHA-256, or HMAC-SHA-256 under
// the MAC key if the codec encrypts.
func (c *Codec) Key(plaintext []byte) store.Key {
	var k store.Key
	if c.mac == nil {
		return sha256.Sum256(plaintext)
	}
	h := hmac.New(sha256.New, c.mac)
	h.Write(plaintext)
	h.Sum(k[:0])
	return k
}

// MaxEncodedSize returns an upper bound on the encoded size of a plaintext of
// n bytes. Compression is only kept when it makes the payload smaller, so
// encoding never grows data by more than the header, nonce and tag.
func (c *Codec) MaxEncodedSize(n int64) int64 {
	return n + int64(headerLen+nonceLen+tagLen)
}

// sharedEncoder returns the process-wide zstd encoder. EncodeAll is safe for
// concurrent use, so one encoder serves every Codec.
var sharedEncoder = sync.OnceValue(func() *zstd.Encoder {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithWindowSize(MaxWindowSize))
	if err != nil {
		panic(fmt.Sprintf("codec: init zstd encoder: %v", err))
	}
	return enc
})

// decoders pools streaming zstd decoders. A decoder is only used by one
// goroutine at a time, so a concurrency of 1 is enough and keeps the memory
// held by idle pooled decoders small.
var decoders = sync.Pool{New: func() any {
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(MaxPlaintextSize),
		zstd.WithDecoderMaxWindow(MaxWindowSize))
	if err != nil {
		panic(fmt.Sprintf("codec: init zstd decoder: %v", err))
	}
	return dec
}}

// decompress inflates the zstd data in payload, returning ErrTooLarge as soon
// as more than maxPlain bytes come out. The limit is enforced while streaming,
// so it holds for frames without a content size and for payloads of many
// frames, where a header check alone would not.
func decompress(payload []byte, maxPlain int64) ([]byte, error) {
	dec := decoders.Get().(*zstd.Decoder)
	defer decoders.Put(dec)
	if err := dec.Reset(bytes.NewReader(payload)); err != nil {
		return nil, fmt.Errorf("%w: zstd: %w", ErrFormat, err)
	}
	// Drop the reference to payload on the way out; the decoder sits in the pool.
	defer func() { _ = dec.Reset(nil) }()

	out, err := io.ReadAll(io.LimitReader(dec, maxPlain+1))
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) {
			return nil, ErrTooLarge
		}
		return nil, fmt.Errorf("%w: zstd: %w", ErrFormat, err)
	}
	if int64(len(out)) > maxPlain {
		return nil, ErrTooLarge
	}
	return out, nil
}

// Encode produces the blob for plaintext, to be stored under key. key should
// be Key(plaintext); it is bound into the blob as authenticated data when
// encrypting.
func (c *Codec) Encode(key store.Key, plaintext []byte) ([]byte, error) {
	var flags byte
	payload := plaintext
	if c.compress && len(plaintext) > 0 {
		if z := sharedEncoder().EncodeAll(plaintext, nil); len(z) < len(plaintext) {
			payload, flags = z, flags|FlagCompressed
		}
	}
	if c.aead != nil {
		flags |= FlagEncrypted
	}

	out := make([]byte, 0, headerLen+nonceLen+len(payload)+tagLen)
	out = append(out, Magic...)
	out = append(out, Version, flags)
	if c.aead == nil {
		return append(out, payload...), nil
	}
	header := out[:headerLen:headerLen]
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("codec: generate nonce: %w", err)
	}
	out = append(out, nonce...)
	return c.aead.Seal(out, nonce, payload, aad(header, key)), nil
}

// Decode verifies and decodes blob, which was filed under key. It checks the
// GCM tag (if encrypted) and that the decoded plaintext hashes to key, and
// returns ErrTooLarge rather than a plaintext longer than maxPlain bytes.
func (c *Codec) Decode(key store.Key, blob []byte, maxPlain int64) ([]byte, error) {
	if len(blob) < headerLen || string(blob[:len(Magic)]) != Magic {
		return nil, fmt.Errorf("%w: bad magic", ErrFormat)
	}
	if v := blob[len(Magic)]; v != Version {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrFormat, v)
	}
	flags := blob[len(Magic)+1]
	if flags&^knownFlags != 0 {
		return nil, fmt.Errorf("%w: unknown flags %#x", ErrFormat, flags)
	}
	if encrypted := flags&FlagEncrypted != 0; encrypted != (c.aead != nil) {
		return nil, ErrMode
	}

	payload := blob[headerLen:]
	if c.aead != nil {
		if len(payload) < nonceLen+tagLen {
			return nil, fmt.Errorf("%w: truncated", ErrFormat)
		}
		nonce, ct := payload[:nonceLen], payload[nonceLen:]
		var err error
		if payload, err = c.aead.Open(nil, nonce, ct, aad(blob[:headerLen], key)); err != nil {
			return nil, ErrAuth
		}
	}

	if flags&FlagCompressed != 0 {
		var h zstd.Header
		if err := h.Decode(payload); err != nil {
			return nil, fmt.Errorf("%w: zstd header: %w", ErrFormat, err)
		}
		if h.HasFCS && h.FrameContentSize > uint64(maxPlain) {
			return nil, ErrTooLarge
		}
		if h.WindowSize > MaxWindowSize {
			return nil, ErrTooLarge
		}
		var err error
		if payload, err = decompress(payload, maxPlain); err != nil {
			return nil, err
		}
	}
	if int64(len(payload)) > maxPlain {
		return nil, ErrTooLarge
	}
	if c.Key(payload) != key {
		return nil, ErrHashMismatch
	}
	return payload, nil
}

// aad builds the additional authenticated data: header then storage key.
func aad(header []byte, key store.Key) []byte {
	out := make([]byte, 0, len(header)+store.KeySize)
	out = append(out, header...)
	return append(out, key[:]...)
}
