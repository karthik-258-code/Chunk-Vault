package codec

import (
	"bytes"
	"errors"
	"math/rand"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	"gitlab.com/gitlab-ai-hackathon/transcend-october-2026/43223301/chunkvault/internal/store"
)

func testKeys(seed byte) *Keys {
	enc, mac := make([]byte, KeySize), make([]byte, KeySize)
	for i := range enc {
		enc[i], mac[i] = seed+byte(i), seed^byte(i)+0x80
	}
	return &Keys{Enc: enc, Mac: mac}
}

// fataler is the subset of testing.TB shared by *testing.T and *rapid.T.
type fataler interface {
	Helper()
	Fatal(args ...any)
}

func mustNew(t fataler, o Options) *Codec {
	t.Helper()
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func modes(t testing.TB) map[string]*Codec {
	return map[string]*Codec{
		"plain":           mustNew(t, Options{}),
		"compressed":      mustNew(t, Options{Compress: true}),
		"encrypted":       mustNew(t, Options{Keys: testKeys(1)}),
		"compressed+encr": mustNew(t, Options{Compress: true, Keys: testKeys(1)}),
	}
}

func roundTrip(t testing.TB, c *Codec, data []byte) []byte {
	t.Helper()
	key := c.Key(data)
	blob, err := c.Encode(key, data)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := c.Decode(key, blob, int64(len(data)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return got
}

func TestRoundTripAllModes(t *testing.T) {
	compressible := bytes.Repeat([]byte("chunkvault "), 5000)
	random := make([]byte, 20_000)
	rand.New(rand.NewSource(1)).Read(random)
	for name, c := range modes(t) {
		for dname, data := range map[string][]byte{"empty": {}, "one": {7}, "compressible": compressible, "random": random} {
			if got := roundTrip(t, c, data); !bytes.Equal(got, data) {
				t.Errorf("%s/%s: round trip mismatch", name, dname)
			}
		}
	}
}

func TestCompressionShrinksAndSkipsIncompressible(t *testing.T) {
	c := mustNew(t, Options{Compress: true})
	data := bytes.Repeat([]byte("a"), 10_000)
	blob, err := c.Encode(c.Key(data), data)
	if err != nil {
		t.Fatal(err)
	}
	if len(blob) >= len(data)/10 || blob[len(Magic)+1]&FlagCompressed == 0 {
		t.Fatalf("compressible data: blob %d bytes, flags %#x", len(blob), blob[len(Magic)+1])
	}

	random := make([]byte, 4096)
	rand.New(rand.NewSource(2)).Read(random)
	blob, err = c.Encode(c.Key(random), random)
	if err != nil {
		t.Fatal(err)
	}
	if blob[len(Magic)+1]&FlagCompressed != 0 || len(blob) != headerLen+len(random) {
		t.Fatalf("incompressible data should be stored raw: %d bytes, flags %#x", len(blob), blob[len(Magic)+1])
	}
}

func TestHeaderLayout(t *testing.T) {
	c := mustNew(t, Options{Keys: testKeys(1)})
	data := []byte("hello")
	blob, err := c.Encode(c.Key(data), data)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob[:4]) != Magic || blob[4] != Version || blob[5] != FlagEncrypted {
		t.Fatalf("header = % x", blob[:6])
	}
	if want := headerLen + nonceLen + len(data) + tagLen; len(blob) != want {
		t.Fatalf("len = %d, want %d", len(blob), want)
	}
	if int64(len(blob)) > c.MaxEncodedSize(int64(len(data))) {
		t.Fatal("MaxEncodedSize is not an upper bound")
	}
}

func TestNonceIsFreshPerEncode(t *testing.T) {
	c := mustNew(t, Options{Keys: testKeys(1)})
	data := []byte("same plaintext")
	key := c.Key(data)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		blob, err := c.Encode(key, data)
		if err != nil {
			t.Fatal(err)
		}
		n := string(blob[headerLen : headerLen+nonceLen])
		if seen[n] {
			t.Fatal("nonce reused")
		}
		seen[n] = true
	}
}

func TestStorageKey(t *testing.T) {
	data := []byte("data")
	plain := mustNew(t, Options{})
	if plain.Key(data) != store.KeyOf(data) {
		t.Fatal("unencrypted key must be SHA-256")
	}
	a := mustNew(t, Options{Keys: testKeys(1)})
	b := mustNew(t, Options{Keys: testKeys(2)})
	if a.Key(data) == store.KeyOf(data) {
		t.Fatal("encrypted key must not be the plain SHA-256")
	}
	if a.Key(data) == b.Key(data) {
		t.Fatal("different MAC keys must give different storage keys")
	}
	if k1, k2 := a.Key(data), a.Key(append([]byte(nil), data...)); k1 != k2 {
		t.Fatal("storage key must be deterministic")
	}
}

func TestNewRejectsBadKeys(t *testing.T) {
	good := testKeys(1)
	for name, k := range map[string]*Keys{
		"short enc": {Enc: good.Enc[:16], Mac: good.Mac},
		"short mac": {Enc: good.Enc, Mac: good.Mac[:5]},
		"empty":     {},
	} {
		if _, err := New(Options{Keys: k}); err == nil {
			t.Errorf("%s: New accepted bad keys", name)
		}
	}
	if mustNew(t, Options{}).Encrypted() || !mustNew(t, Options{Keys: good}).Encrypted() {
		t.Fatal("Encrypted() wrong")
	}
}

func TestTamperEveryByteFails(t *testing.T) {
	data := bytes.Repeat([]byte("tamper "), 40)
	for name, c := range modes(t) {
		key := c.Key(data)
		blob, err := c.Encode(key, data)
		if err != nil {
			t.Fatal(err)
		}
		for i := range blob {
			bad := append([]byte(nil), blob...)
			bad[i] ^= 0x01
			if got, err := c.Decode(key, bad, int64(len(data))); err == nil && !bytes.Equal(got, data) {
				t.Fatalf("%s: flipping byte %d returned wrong data without error", name, i)
			} else if err == nil && c.Encrypted() {
				t.Fatalf("%s: flipping byte %d of an encrypted blob went undetected", name, i)
			}
		}
	}
}

func TestFlippedByteErrors(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1000)
	c := mustNew(t, Options{Compress: true, Keys: testKeys(1)})
	key := c.Key(data)
	blob, _ := c.Encode(key, data)
	blob[len(blob)-1] ^= 0xff // in the GCM tag
	if _, err := c.Decode(key, blob, 1000); !errors.Is(err, ErrAuth) {
		t.Fatalf("flipped tag byte: %v, want ErrAuth", err)
	}
	blob, _ = c.Encode(key, data)
	blob[headerLen+nonceLen+2] ^= 0xff // in the ciphertext
	if _, err := c.Decode(key, blob, 1000); !errors.Is(err, ErrAuth) {
		t.Fatalf("flipped ciphertext byte: %v, want ErrAuth", err)
	}
	blob, _ = c.Encode(key, data)
	blob[headerLen] ^= 0xff // in the nonce
	if _, err := c.Decode(key, blob, 1000); !errors.Is(err, ErrAuth) {
		t.Fatalf("flipped nonce byte: %v, want ErrAuth", err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	data := []byte("secret chunk")
	a := mustNew(t, Options{Compress: true, Keys: testKeys(1)})
	b := mustNew(t, Options{Compress: true, Keys: testKeys(2)})
	key := a.Key(data)
	blob, _ := a.Encode(key, data)
	if _, err := b.Decode(key, blob, 100); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key: %v, want ErrAuth", err)
	}
	// Right encryption key but wrong MAC key: the tag verifies, but the
	// plaintext does not hash to the storage key.
	k := testKeys(1)
	k.Mac = testKeys(9).Mac
	c := mustNew(t, Options{Compress: true, Keys: k})
	if _, err := c.Decode(key, blob, 100); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("wrong mac key: %v, want ErrHashMismatch", err)
	}
}

func TestWrongAADFails(t *testing.T) {
	c := mustNew(t, Options{Keys: testKeys(1)})
	data := []byte("chunk A")
	other := []byte("chunk B")
	blob, _ := c.Encode(c.Key(data), data)
	// The blob for A offered under B's storage key must not decrypt.
	if _, err := c.Decode(c.Key(other), blob, 100); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong AAD: %v, want ErrAuth", err)
	}
	// A flag flip in the (authenticated) header is also caught.
	blob[5] ^= FlagCompressed
	if _, err := c.Decode(c.Key(data), blob, 100); !errors.Is(err, ErrAuth) {
		t.Fatalf("flag flip: %v, want ErrAuth", err)
	}
}

func TestHashMismatchWithoutEncryption(t *testing.T) {
	c := mustNew(t, Options{Compress: true})
	blob, _ := c.Encode(c.Key([]byte("one")), []byte("one"))
	if _, err := c.Decode(c.Key([]byte("two")), blob, 100); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Decode = %v, want ErrHashMismatch", err)
	}
}

func TestFormatErrors(t *testing.T) {
	c := mustNew(t, Options{})
	good, _ := c.Encode(c.Key([]byte("x")), []byte("x"))
	mutate := func(f func(b []byte)) []byte {
		b := append([]byte(nil), good...)
		f(b)
		return b
	}
	tests := map[string][]byte{
		"empty":         nil,
		"short":         good[:3],
		"bad magic":     mutate(func(b []byte) { b[0] = 'X' }),
		"bad version":   mutate(func(b []byte) { b[4] = 99 }),
		"unknown flags": mutate(func(b []byte) { b[5] = 0x80 }),
		"bad zstd":      mutate(func(b []byte) { b[5] = FlagCompressed }),
	}
	for name, blob := range tests {
		if _, err := c.Decode(c.Key([]byte("x")), blob, 10); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: err = %v, want ErrFormat", name, err)
		}
	}
	enc := mustNew(t, Options{Keys: testKeys(1)})
	eb, _ := enc.Encode(enc.Key([]byte("x")), []byte("x"))
	if _, err := enc.Decode(enc.Key([]byte("x")), eb[:headerLen+5], 10); !errors.Is(err, ErrFormat) {
		t.Errorf("truncated encrypted blob: %v, want ErrFormat", err)
	}
}

func TestModeMismatch(t *testing.T) {
	plain := mustNew(t, Options{})
	enc := mustNew(t, Options{Keys: testKeys(1)})
	data := []byte("data")
	pb, _ := plain.Encode(plain.Key(data), data)
	eb, _ := enc.Encode(enc.Key(data), data)
	if _, err := enc.Decode(enc.Key(data), pb, 10); !errors.Is(err, ErrMode) {
		t.Errorf("plain blob into encrypted codec: %v, want ErrMode", err)
	}
	if _, err := plain.Decode(plain.Key(data), eb, 10); !errors.Is(err, ErrMode) {
		t.Errorf("encrypted blob into plain codec: %v, want ErrMode", err)
	}
}

func TestDecodeSizeLimit(t *testing.T) {
	for name, c := range modes(t) {
		data := bytes.Repeat([]byte("z"), 5000)
		key := c.Key(data)
		blob, _ := c.Encode(key, data)
		if _, err := c.Decode(key, blob, 4999); !errors.Is(err, ErrTooLarge) {
			t.Errorf("%s: Decode = %v, want ErrTooLarge", name, err)
		}
		if _, err := c.Decode(key, blob, 5000); err != nil {
			t.Errorf("%s: Decode at exact limit: %v", name, err)
		}
	}
}

// zstdBlob wraps a raw zstd payload in the header of an unencrypted,
// compressed blob.
func zstdBlob(payload []byte) []byte {
	return append([]byte(Magic+"\x01\x01"), payload...)
}

// rleFrameNoFCS hand-builds a zstd frame that does not declare its content
// size: a 128 KiB window, then n run-length blocks that each expand 4 bytes to
// 128 KiB of zeros.
func rleFrameNoFCS(n int) []byte {
	const blockSize = 128 << 10
	// magic, frame header descriptor (no FCS, no checksum, not single
	// segment), window descriptor (128 KiB).
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38}
	for i := 0; i < n; i++ {
		h := uint32(blockSize<<3) | 1<<1 // RLE block
		if i == n-1 {
			h |= 1 // last block
		}
		frame = append(frame, byte(h), byte(h>>8), byte(h>>16), 0)
	}
	return frame
}

func TestDecodeSizeLimitWithoutFrameContentSize(t *testing.T) {
	frame := rleFrameNoFCS(8) // 1 MiB out of 38 bytes
	var h zstd.Header
	if err := h.Decode(frame); err != nil || h.HasFCS {
		t.Fatalf("test frame: err %v, HasFCS %v; want a valid frame without FCS", err, h.HasFCS)
	}
	c := mustNew(t, Options{Compress: true})
	if _, err := c.Decode(store.Key{}, zstdBlob(frame), 1000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Decode = %v, want ErrTooLarge", err)
	}

	// Within the limit the same frame decodes, so the frame is well formed.
	plain := make([]byte, 8*128<<10)
	if got, err := c.Decode(c.Key(plain), zstdBlob(frame), int64(len(plain))); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Decode at exact limit: %d bytes, err %v", len(got), err)
	}
	if _, err := c.Decode(c.Key(plain), zstdBlob(frame), int64(len(plain))-1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Decode one under the limit = %v, want ErrTooLarge", err)
	}
}

func TestDecodeSizeLimitMultiFrame(t *testing.T) {
	// Each frame declares a content size under the limit; only the total is over.
	part := bytes.Repeat([]byte("m"), 600)
	frame := sharedEncoder().EncodeAll(part, nil)
	payload := append(append([]byte(nil), frame...), frame...)

	c := mustNew(t, Options{Compress: true})
	if _, err := c.Decode(store.Key{}, zstdBlob(payload), 1000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Decode = %v, want ErrTooLarge", err)
	}
	whole := bytes.Repeat([]byte("m"), 1200)
	if got, err := c.Decode(c.Key(whole), zstdBlob(payload), 1200); err != nil || !bytes.Equal(got, whole) {
		t.Fatalf("Decode at exact limit: %d bytes, err %v", len(got), err)
	}
}

func TestConcurrentUse(t *testing.T) {
	c := mustNew(t, Options{Compress: true, Keys: testKeys(1)})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < 50; i++ {
				data := bytes.Repeat([]byte{byte(r.Intn(4))}, r.Intn(3000))
				if got := roundTrip(t, c, data); !bytes.Equal(got, data) {
					t.Error("concurrent round trip mismatch")
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
