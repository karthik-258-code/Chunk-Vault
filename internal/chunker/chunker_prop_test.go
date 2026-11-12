package chunker

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"pgregory.net/rapid"
)

// propConfig draws a valid, small Config so properties exercise many chunks
// without needing megabytes of input.
func propConfig(t *rapid.T) Config {
	minSize := rapid.IntRange(64, 512).Draw(t, "min")
	avg := minSize + rapid.IntRange(1, 2048).Draw(t, "avgDelta")
	maxSize := avg + rapid.IntRange(1, 4096).Draw(t, "maxDelta")
	return Config{MinSize: minSize, AvgSize: avg, MaxSize: maxSize}
}

func chunkAll(t *rapid.T, data []byte, cfg Config) [][]byte {
	c, err := New(bytes.NewReader(data), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var out [][]byte
	for {
		ch, err := c.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("Next: %v", err)
			}
			return out
		}
		out = append(out, append([]byte(nil), ch.Data...))
	}
}

func TestPropConcatenationEqualsInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cfg := propConfig(t)
		data := rapid.SliceOfN(rapid.Byte(), 0, 50_000).Draw(t, "data")
		got := bytes.Join(chunkAll(t, data, cfg), nil)
		if !bytes.Equal(got, data) {
			t.Fatalf("reassembled stream differs from input (len %d vs %d)", len(got), len(data))
		}
	})
}

func TestPropChunkSizeBounds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cfg := propConfig(t)
		data := rapid.SliceOfN(rapid.Byte(), 0, 50_000).Draw(t, "data")
		chunks := chunkAll(t, data, cfg)
		for i, ch := range chunks {
			if len(ch) == 0 {
				t.Fatalf("chunk %d is empty", i)
			}
			if len(ch) > cfg.MaxSize {
				t.Fatalf("chunk %d has %d bytes, above max %d", i, len(ch), cfg.MaxSize)
			}
			if i < len(chunks)-1 && len(ch) < cfg.MinSize {
				t.Fatalf("non-final chunk %d has %d bytes, below min %d", i, len(ch), cfg.MinSize)
			}
		}
	})
}

func TestPropDeterministic(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cfg := propConfig(t)
		data := rapid.SliceOfN(rapid.Byte(), 0, 50_000).Draw(t, "data")
		a := chunkAll(t, data, cfg)
		b := chunkAll(t, data, cfg)
		if len(a) != len(b) {
			t.Fatalf("chunk count differs: %d vs %d", len(a), len(b))
		}
		for i := range a {
			if !bytes.Equal(a[i], b[i]) {
				t.Fatalf("chunk %d differs between runs", i)
			}
		}
	})
}

// TestPropBoundaryStability inserts bytes into the middle of a large random
// input and checks the locality guarantee of content-defined chunking:
//
//   - every chunk that ends before the edit is byte-identical, because a cut
//     point depends only on the bytes before it;
//   - once a boundary after the edit realigns with an original boundary, every
//     remaining chunk is byte-identical;
//   - realignment happens close to the edit.
//
// How many chunks an edit disturbs is a probability distribution with a long
// tail, so the test bounds the resynchronisation distance loosely instead of
// capping the disturbed chunk count, which made it flaky.
func TestPropBoundaryStability(t *testing.T) {
	cfg := Config{MinSize: 256, AvgSize: 1024, MaxSize: 4096}
	maxResync := 16 * cfg.MaxSize
	rapid.Check(t, func(t *rapid.T) {
		seed := rapid.Uint64().Draw(t, "seed")
		size := rapid.IntRange(200_000, 400_000).Draw(t, "size")
		data := make([]byte, size)
		fillPseudoRandom(data, seed)

		pos := rapid.IntRange(size/4, 3*size/4).Draw(t, "pos")
		insert := rapid.SliceOfN(rapid.Byte(), 1, 64).Draw(t, "insert")
		edited := make([]byte, 0, size+len(insert))
		edited = append(edited, data[:pos]...)
		edited = append(edited, insert...)
		edited = append(edited, data[pos:]...)

		orig := chunkAll(t, data, cfg)
		mod := chunkAll(t, edited, cfg)

		// Chunks ending at or before the edit must be identical.
		i, off := 0, 0
		for i < len(orig) && off+len(orig[i]) <= pos {
			if i >= len(mod) || !bytes.Equal(orig[i], mod[i]) {
				t.Fatalf("chunk %d ends before the edit at %d but differs", i, pos)
			}
			off += len(orig[i])
			i++
		}

		// Map each original chunk end offset to its chunk index.
		origEnds := make(map[int]int, len(orig))
		end := 0
		for k, c := range orig {
			end += len(c)
			origEnds[end] = k
		}

		// Walk the edited chunking past the edit until a boundary, shifted back
		// by the insert length, coincides with an original boundary. The final
		// boundary (end of input) always coincides, so this terminates.
		modOff := off
		for j := i; j < len(mod); j++ {
			modOff += len(mod[j])
			shifted := modOff - len(insert)
			if shifted < pos {
				continue // boundary falls inside the inserted bytes
			}
			if shifted-pos > maxResync {
				t.Fatalf("chunkings still disagree %d bytes after the edit at %d (bound %d)", shifted-pos, pos, maxResync)
			}
			k, ok := origEnds[shifted]
			if !ok {
				continue
			}
			origTail, modTail := orig[k+1:], mod[j+1:]
			if len(origTail) != len(modTail) {
				t.Fatalf("after resync at %d: %d original vs %d edited chunks remain", shifted, len(origTail), len(modTail))
			}
			for x := range origTail {
				if !bytes.Equal(origTail[x], modTail[x]) {
					t.Fatalf("after resync at %d: chunk %d differs", shifted, k+1+x)
				}
			}
			return
		}
		t.Fatalf("chunkings never resynchronised after the edit at %d", pos)
	})
}

// fillPseudoRandom fills b using xorshift64*, keeping each rapid iteration
// cheap and reproducible from a single drawn seed.
func fillPseudoRandom(b []byte, seed uint64) {
	x := seed | 1
	for i := range b {
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		b[i] = byte((x * 0x2545F4914F6CDD1D) >> 56)
	}
}
