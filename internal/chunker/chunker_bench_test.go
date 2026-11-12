package chunker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// BenchmarkChunker measures chunking throughput; the MB/s column is reported
// by b.SetBytes.
func BenchmarkChunker(b *testing.B) {
	for _, mib := range []int{1, 16, 64} {
		data := randomBytes(42, mib<<20)
		b.Run(fmt.Sprintf("default/%dMiB", mib), func(b *testing.B) {
			benchChunker(b, data, DefaultConfig())
		})
	}
	data := randomBytes(43, 16<<20)
	b.Run("small/16MiB", func(b *testing.B) {
		benchChunker(b, data, Config{MinSize: 2048, AvgSize: 8192, MaxSize: 32768})
	})
}

func benchChunker(b *testing.B, data []byte, cfg Config) {
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := New(bytes.NewReader(data), cfg)
		if err != nil {
			b.Fatal(err)
		}
		for {
			_, err := c.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
