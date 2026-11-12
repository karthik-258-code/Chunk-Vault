package chunker

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
	"testing/iotest"
)

func randomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// collect drains a chunker, copying each chunk's data.
func collect(t testing.TB, r io.Reader, cfg Config) [][]byte {
	t.Helper()
	c, err := New(r, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out [][]byte
	var off int64
	for {
		ch, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ch.Offset != off {
			t.Fatalf("chunk offset = %d, want %d", ch.Offset, off)
		}
		off += int64(len(ch.Data))
		out = append(out, append([]byte(nil), ch.Data...))
	}
}

func smallConfig() Config { return Config{MinSize: 256, AvgSize: 1024, MaxSize: 4096} }

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"default", DefaultConfig(), true},
		{"small", smallConfig(), true},
		{"min too small", Config{MinSize: 8, AvgSize: 1024, MaxSize: 4096}, false},
		{"avg not above min", Config{MinSize: 1024, AvgSize: 1024, MaxSize: 4096}, false},
		{"max not above avg", Config{MinSize: 256, AvgSize: 1024, MaxSize: 1024}, false},
		{"max too large", Config{MinSize: 256, AvgSize: 1024, MaxSize: 1 << 31}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.ok {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Validate() = %v, want ErrInvalidConfig", err)
			}
			if _, err := New(bytes.NewReader(nil), tc.cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestEmptyInput(t *testing.T) {
	if got := collect(t, bytes.NewReader(nil), DefaultConfig()); len(got) != 0 {
		t.Fatalf("got %d chunks for empty input", len(got))
	}
}

func TestSmallInputIsSingleChunk(t *testing.T) {
	data := randomBytes(1, 1000)
	chunks := collect(t, bytes.NewReader(data), DefaultConfig())
	if len(chunks) != 1 || !bytes.Equal(chunks[0], data) {
		t.Fatalf("got %d chunks, want exactly one equal to the input", len(chunks))
	}
}

func TestChunksReassembleAndRespectBounds(t *testing.T) {
	cfg := smallConfig()
	data := randomBytes(2, 1<<20)
	chunks := collect(t, bytes.NewReader(data), cfg)

	var joined []byte
	for i, ch := range chunks {
		joined = append(joined, ch...)
		if len(ch) > cfg.MaxSize {
			t.Errorf("chunk %d: %d bytes exceeds max %d", i, len(ch), cfg.MaxSize)
		}
		if i < len(chunks)-1 && len(ch) < cfg.MinSize {
			t.Errorf("chunk %d: %d bytes below min %d", i, len(ch), cfg.MinSize)
		}
	}
	if !bytes.Equal(data, joined) {
		t.Fatal("concatenated chunks differ from input")
	}
}

func TestAverageSizeNearTarget(t *testing.T) {
	cfg := smallConfig()
	data := randomBytes(3, 8<<20)
	chunks := collect(t, bytes.NewReader(data), cfg)
	avg := float64(len(data)) / float64(len(chunks))
	lo, hi := 0.5*float64(cfg.AvgSize), 1.5*float64(cfg.AvgSize)
	if avg < lo || avg > hi {
		t.Fatalf("average chunk size %.0f outside [%.0f, %.0f]", avg, lo, hi)
	}
}

func TestUniformDataRespectsBounds(t *testing.T) {
	// A constant stream is the degenerate case for a rolling hash.
	cfg := smallConfig()
	chunks := collect(t, bytes.NewReader(make([]byte, 100_000)), cfg)
	for i, ch := range chunks[:len(chunks)-1] {
		if len(ch) < cfg.MinSize || len(ch) > cfg.MaxSize {
			t.Errorf("chunk %d has size %d, want within [%d, %d]", i, len(ch), cfg.MinSize, cfg.MaxSize)
		}
	}
}

func TestReaderFragmentationDoesNotChangeBoundaries(t *testing.T) {
	cfg := smallConfig()
	data := randomBytes(4, 300_000)
	want := collect(t, bytes.NewReader(data), cfg)
	readers := map[string]io.Reader{
		"one byte": iotest.OneByteReader(bytes.NewReader(data)),
		"half":     iotest.HalfReader(bytes.NewReader(data)),
		"data err": iotest.DataErrReader(bytes.NewReader(data)),
	}
	for name, r := range readers {
		t.Run(name, func(t *testing.T) {
			got := collect(t, r, cfg)
			if len(got) != len(want) {
				t.Fatalf("got %d chunks, want %d", len(got), len(want))
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("chunk %d differs", i)
				}
			}
		})
	}
}

func TestReadErrorIsPropagated(t *testing.T) {
	boom := errors.New("boom")
	data := randomBytes(5, 10_000)
	c, err := New(io.MultiReader(bytes.NewReader(data), iotest.ErrReader(boom)), smallConfig())
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = c.Next(); err != nil {
			break
		}
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Next() = %v, want %v", err, boom)
	}
	if _, err = c.Next(); !errors.Is(err, boom) {
		t.Fatalf("error is not sticky: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, nil }

func TestNoProgressReader(t *testing.T) {
	c, err := New(zeroReader{}, smallConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Next(); !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("Next() = %v, want io.ErrNoProgress", err)
	}
}

// TestBoundariesArePinned guards against accidental changes to the gear table
// or cut logic: any change moves every boundary and silently defeats
// deduplication against previously stored data.
func TestBoundariesArePinned(t *testing.T) {
	seen := map[uint64]bool{}
	for _, v := range gear {
		if seen[v] {
			t.Fatal("duplicate gear table entry")
		}
		seen[v] = true
	}
	chunks := collect(t, bytes.NewReader(randomBytes(7, 1<<20)), DefaultConfig())
	var sizes []int
	for _, ch := range chunks {
		sizes = append(sizes, len(ch))
	}
	want := pinnedSizes
	if len(sizes) != len(want) {
		t.Fatalf("sizes = %v, want %v", sizes, want)
	}
	for i := range want {
		if sizes[i] != want[i] {
			t.Fatalf("sizes = %v, want %v", sizes, want)
		}
	}
}

func TestTopBitsMask(t *testing.T) {
	tests := []struct {
		n    int
		want uint64
	}{{0, 0}, {-3, 0}, {1, 1 << 63}, {64, ^uint64(0)}, {70, ^uint64(0)}}
	for _, tc := range tests {
		if got := topBitsMask(tc.n); got != tc.want {
			t.Errorf("topBitsMask(%d) = %#x, want %#x", tc.n, got, tc.want)
		}
	}
}

func TestAvgSizeRounding(t *testing.T) {
	// 50 KiB is closer to 64 KiB than to 32 KiB, so 16-bit masks are expected.
	c, err := New(bytes.NewReader(nil), Config{MinSize: 1024, AvgSize: 50 * 1024, MaxSize: 256 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if c.maskS != topBitsMask(16+normalization) || c.maskL != topBitsMask(16-normalization) {
		t.Fatalf("unexpected masks %#x / %#x", c.maskS, c.maskL)
	}
	// 40 KiB rounds down to 32 KiB.
	c, err = New(bytes.NewReader(nil), Config{MinSize: 1024, AvgSize: 40 * 1024, MaxSize: 256 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if c.maskS != topBitsMask(15+normalization) {
		t.Fatalf("unexpected strict mask %#x", c.maskS)
	}
}
