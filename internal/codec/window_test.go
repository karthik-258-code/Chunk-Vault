package codec

import (
	"bytes"
	"errors"
	"testing"
)

// hostileWindowFrame returns a zstd frame without a content size whose window
// descriptor declares a 512 MiB window, followed by one raw block holding "x".
func hostileWindowFrame() []byte {
	return []byte{
		// Magic number.
		0x28, 0xb5, 0x2f, 0xfd,
		// Frame header descriptor: no FCS, not single segment, no checksum,
		// no dictionary.
		0x00,
		// Window descriptor: exponent 19, mantissa 0, so 2^29 bytes.
		0x98,
		// Block header: last block, raw, size 1.
		0x09, 0x00, 0x00,
		'x',
	}
}

// compressedBlob wraps a zstd payload in an unencrypted, compressed blob.
func compressedBlob(payload []byte) []byte {
	blob := make([]byte, 0, headerLen+len(payload))
	blob = append(blob, Magic...)
	blob = append(blob, Version, FlagCompressed)
	return append(blob, payload...)
}

func TestDecodeRejectsOversizedWindow(t *testing.T) {
	c, err := New(Options{Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Decode(c.Key([]byte("x")), compressedBlob(hostileWindowFrame()), 1<<20)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Decode = %v, want ErrTooLarge", err)
	}
}

func TestDecodeRejectsOversizedWindowInLaterFrame(t *testing.T) {
	c, err := New(Options{Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	first := sharedEncoder().EncodeAll([]byte("a"), nil)
	hostile := hostileWindowFrame()
	payload := make([]byte, 0, len(first)+len(hostile))
	payload = append(payload, first...)
	payload = append(payload, hostile...)

	_, err = c.Decode(c.Key([]byte("ax")), compressedBlob(payload), 1<<20)
	if err == nil {
		t.Fatal("Decode accepted a later frame with an oversized window")
	}
	if !errors.Is(err, ErrTooLarge) && !errors.Is(err, ErrFormat) {
		t.Fatalf("Decode = %v, want ErrTooLarge or ErrFormat", err)
	}
}

func TestRoundTripLargerThanWindow(t *testing.T) {
	c, err := New(Options{Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	// Larger than the window, so the encoder emits a windowed frame rather
	// than a single-segment one.
	data := make([]byte, MaxWindowSize+MaxWindowSize/2)
	for i := range data {
		data[i] = byte(i % 251)
	}
	key := c.Key(data)
	blob, err := c.Encode(key, data)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decode(key, blob, int64(len(data)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round trip changed the data")
	}
}
