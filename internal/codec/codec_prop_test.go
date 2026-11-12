package codec

import (
	"bytes"
	"errors"
	"testing"

	"pgregory.net/rapid"
)

// payload draws data that is sometimes highly compressible and sometimes
// random, so both the compressed and raw paths are exercised.
func payload(t *rapid.T) []byte {
	if rapid.Bool().Draw(t, "repetitive") {
		unit := rapid.SliceOfN(rapid.Byte(), 1, 8).Draw(t, "unit")
		return bytes.Repeat(unit, rapid.IntRange(0, 2000).Draw(t, "reps"))
	}
	return rapid.SliceOfN(rapid.Byte(), 0, 4096).Draw(t, "data")
}

func propCodec(t *rapid.T) *Codec {
	o := Options{Compress: rapid.Bool().Draw(t, "compress")}
	if rapid.Bool().Draw(t, "encrypt") {
		o.Keys = &Keys{
			Enc: rapid.SliceOfN(rapid.Byte(), KeySize, KeySize).Draw(t, "enc"),
			Mac: rapid.SliceOfN(rapid.Byte(), KeySize, KeySize).Draw(t, "mac"),
		}
	}
	c, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestPropRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c, data := propCodec(t), payload(t)
		key := c.Key(data)
		blob, err := c.Encode(key, data)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if int64(len(blob)) > c.MaxEncodedSize(int64(len(data))) {
			t.Fatalf("blob of %d bytes exceeds MaxEncodedSize %d", len(blob), c.MaxEncodedSize(int64(len(data))))
		}
		got, err := c.Decode(key, blob, int64(len(data)))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("round trip changed the data")
		}
	})
}

// Any single bit flip in an encrypted blob must be rejected, and in no mode
// may a modified blob decode to different data.
func TestPropTamperDetected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c, data := propCodec(t), payload(t)
		key := c.Key(data)
		blob, err := c.Encode(key, data)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		i := rapid.IntRange(0, len(blob)-1).Draw(t, "index")
		bit := rapid.UintRange(0, 7).Draw(t, "bit")
		bad := append([]byte(nil), blob...)
		bad[i] ^= 1 << bit
		got, err := c.Decode(key, bad, int64(len(data)))
		if c.Encrypted() && err == nil {
			t.Fatalf("bit flip at byte %d went undetected in an encrypted blob", i)
		}
		if err == nil && !bytes.Equal(got, data) {
			t.Fatalf("bit flip at byte %d decoded to different data", i)
		}
	})
}

func TestPropWrongAADRejected(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := mustNew(t, Options{Compress: true, Keys: testKeys(3)})
		a, b := payload(t), payload(t)
		if bytes.Equal(a, b) {
			t.Skip("need two distinct plaintexts")
		}
		blob, err := c.Encode(c.Key(a), a)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if _, err := c.Decode(c.Key(b), blob, MaxPlaintextSize); !errors.Is(err, ErrAuth) {
			t.Fatalf("Decode under another key = %v, want ErrAuth", err)
		}
	})
}

func TestPropEncodingIsRandomised(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := mustNew(t, Options{Keys: testKeys(4)})
		data := payload(t)
		key := c.Key(data)
		x, err1 := c.Encode(key, data)
		y, err2 := c.Encode(key, data)
		if err1 != nil || err2 != nil {
			t.Fatalf("Encode: %v, %v", err1, err2)
		}
		if bytes.Equal(x, y) {
			t.Fatal("two encryptions of the same chunk are identical")
		}
	})
}
