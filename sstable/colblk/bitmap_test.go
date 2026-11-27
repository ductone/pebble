// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
	"unicode"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/binfmt"
	"github.com/cockroachdb/pebble/internal/treeprinter"
	"github.com/stretchr/testify/require"
)

func TestBitmapFixed(t *testing.T) {
	var bitmap Bitmap
	datadriven.RunTest(t, "testdata/bitmap", func(t *testing.T, td *datadriven.TestData) string {
		var buf bytes.Buffer
		switch td.Cmd {
		case "build":
			var builder BitmapBuilder
			var n int
			var off uint32
			for _, r := range td.Input {
				if unicode.IsSpace(r) {
					continue
				}
				if r == '1' {
					builder.Set(n)
				}
				n++
			}
			td.MaybeScanArgs(t, "rows", &n)
			td.MaybeScanArgs(t, "offset", &off)

			size := builder.Size(n, off)
			if td.HasArg("invert") {
				size = builder.InvertedSize(n, off)
				builder.Invert(n)
				if newSize := builder.Size(n, off); size != newSize {
					td.Fatalf(t, "InvertedSize=%d, after invert Size=%d", size, newSize)
				}
			}
			data := make([]byte, size)

			endOffset := builder.Finish(0, n, off, data)
			if endOffset != size {
				td.Fatalf(t, "endOffset=%d size=%d", endOffset, size)
			}
			bitmap, _ = DecodeBitmap(data, uint64(off), uint32(n))
			dumpBitmap(&buf, bitmap)
			fmt.Fprint(&buf, "\nBinary representation:\n")
			f := binfmt.New(data)
			if off > 0 {
				f.HexBytesln(int(off), "initial offset")
			}
			tp := treeprinter.New()
			bitmapToBinFormatter(f, tp.Child("bitmap"), n)
			fmt.Fprint(&buf, tp.String())

		case "seek-set-ge":
			var indexes []int
			td.ScanArgs(t, "indexes", &indexes)
			for _, idx := range indexes {
				fmt.Fprintf(&buf, "bitmap.SeekSetBitGE(%d) = %d\n", idx, bitmap.SeekSetBitGE(idx))
			}

		case "seek-set-le":
			var indexes []int
			td.ScanArgs(t, "indexes", &indexes)
			for _, idx := range indexes {
				fmt.Fprintf(&buf, "bitmap.SeekSetBitLE(%d) = %d\n", idx, bitmap.SeekSetBitLE(idx))
			}

		case "seek-unset-ge":
			var indexes []int
			td.ScanArgs(t, "indexes", &indexes)
			for _, idx := range indexes {
				fmt.Fprintf(&buf, "bitmap.SeekUnsetBitGE(%d) = %d\n", idx, bitmap.SeekUnsetBitGE(idx))
			}

		case "seek-unset-le":
			var indexes []int
			td.ScanArgs(t, "indexes", &indexes)
			for _, idx := range indexes {
				fmt.Fprintf(&buf, "bitmap.SeekUnsetBitLE(%d) = %d\n", idx, bitmap.SeekUnsetBitLE(idx))
			}

		default:
			t.Fatalf("unknown command: %s", td.Cmd)
		}
		return buf.String()
	})
}

func TestNextPrevBitInWord(t *testing.T) {
	words := []uint64{0, math.MaxUint64}
	for i := 0; i < 1000; i++ {
		words = append(words, rand.Uint64())
	}
	for _, w := range words {
		// Check that we can reconstruct the word if we jump from set bit to set
		// bit.
		var val uint64
		for i := 0; i < 64; i++ {
			i = nextBitInWord(w, uint(i))
			if i == 64 {
				break
			}
			val |= 1 << i
		}
		require.Equal(t, w, val)
		val = 0
		for i := 63; i >= 0; i-- {
			i = prevBitInWord(w, uint(i))
			if i == -1 {
				break
			}
			val |= 1 << i
		}
		require.Equal(t, w, val)
	}
}

func dumpBitmap(w io.Writer, b Bitmap) {
	for i := 0; i < b.bitCount; i++ {
		if i > 0 && i%64 == 0 {
			w.Write([]byte{'\n'})
		}
		if b.At(i) {
			w.Write([]byte{'1'})
		} else {
			w.Write([]byte{'0'})
		}
	}
}

func TestBitmapRandom(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	t.Logf("seed: %d", seed)
	rng := rand.New(rand.NewPCG(0, seed))

	var builder BitmapBuilder
	testWithProbability := func(t *testing.T, p float64, size int, invert bool) {
		defer builder.Reset()
		rng := rand.New(rand.NewPCG(0, seed))

		// Sometimes Set +1 bit to test the common pattern of excluding the last
		// row from a data block.
		buildSize := size + rng.IntN(5)
		v := make([]bool, buildSize)
		for i := range v {
			v[i] = rng.Float64() < p
			if v[i] {
				builder.Set(i)
			}
		}

		// Sometimes invert the bitmap.
		if invert {
			builder.Invert(size)
			for i := 0; i < size; i++ {
				v[i] = !v[i]
			}
		}

		data := make([]byte, builder.Size(size, 0))
		_ = builder.Finish(0, size, 0, data)
		bitmap, endOffset := DecodeBitmap(data, 0, uint32(size))
		require.Equal(t, uint64(len(data)), endOffset)
		for i := 0; i < size; i++ {
			if got := bitmap.At(i); got != v[i] {
				t.Fatalf("b.Get(%d) = %t; want %t", i, got, v[i])
			}
		}
		for i := 0; i < size; i++ {
			succ := bitmap.SeekSetBitGE(i)
			// Ensure that SeekSetBitGE always returns the index of a set bit.
			if succ != size && !bitmap.At(succ) {
				t.Fatalf("b.SeekSetBitGE(%d) = %d; bit at index %d is not set", i, succ, succ)
			}
			pred := bitmap.SeekSetBitLE(i)
			// Ensure that SeekSetBitLE always returns the index of a set bit.
			if pred >= 0 && !bitmap.At(pred) {
				t.Fatalf("b.SeekSetBitLE(%d) = %d; bit at index %d is not set", i, pred, pred)
			}

			// Ensure there are no set bits between i and succ.
			for j := i; j < succ; j++ {
				if bitmap.At(j) {
					t.Fatalf("b.SeekSetBitGE(%d) = %d; bit at index %d is set", i, succ, j)
				}
			}
			// Ensure there are no set bits between pred and i.
			for j := pred + 1; j < i; j++ {
				if bitmap.At(j) {
					t.Fatalf("b.SeekSetBitLE(%d) = %d; bit at index %d is set", i, pred, j)
				}
			}
		}
		for i := 0; i < size; i++ {
			succ := bitmap.SeekUnsetBitGE(i)
			// Ensure that SeekUnsetBitGE always returns the index of an unset bit.
			if succ != size && bitmap.At(succ) {
				t.Fatalf("b.SeekUnsetBitGE(%d) = %d; bit at index %d is set", i, succ, succ)
			}
			pred := bitmap.SeekUnsetBitLE(i)
			// Ensure that SeekUnsetBitLE always returns the index of an unset bit.
			if pred >= 0 && bitmap.At(pred) {
				t.Fatalf("b.SeekUnsetBitLE(%d) = %d; bit at index %d is set", i, pred, pred)
			}

			// Ensure there are only set bits between i and succ.
			for j := i; j < succ; j++ {
				if !bitmap.At(j) {
					t.Fatalf("b.SeekUnsetBitGE(%d) = %d; bit at index %d is unset", i, succ, j)
				}
			}
			// Ensure there are only set bits between pred and i.
			for j := pred + 1; j < i; j++ {
				if !bitmap.At(j) {
					t.Fatalf("b.SeekUnsetBitLE(%d) = %d; bit at index %d is unset", i, pred, j)
				}
			}
		}
	}

	fixedSizes := []int{1, 2, 3, 4, 16, 63, 64, 65, 128, 129, 256, 257, 1024, 1025, 4096, 4097, 8012, 8200}
	fixedProbabilities := []float64{0.00001, 0.0001, 0.001, 0.1, 0.5, 0.9999}
	for _, p := range fixedProbabilities {
		t.Run(fmt.Sprintf("p=%05f", p), func(t *testing.T) {
			for _, sz := range fixedSizes {
				t.Run(fmt.Sprintf("size=%d", sz), func(t *testing.T) {
					t.Run("invert", func(t *testing.T) {
						testWithProbability(t, p, sz, true /* invert */)
					})
					t.Run("no-invert", func(t *testing.T) {
						testWithProbability(t, p, sz, false /* invert */)
					})
				})
			}
		})
	}
	for i := 0; i < 10; i++ {
		p := rng.ExpFloat64() * 0.1
		t.Run(fmt.Sprintf("p=%05f", p), func(t *testing.T) {
			testWithProbability(t, p, rng.IntN(8200)+1, rng.IntN(2) == 1)
		})
	}
}

func BenchmarkBitmapBuilder(b *testing.B) {
	seed := uint64(10024282523)
	rng := rand.New(rand.NewPCG(0, seed))
	size := rng.IntN(4096) + 1
	v := make([]bool, size)
	for i := 0; i < size; i++ {
		v[i] = rng.IntN(2) == 0
	}
	data := make([]byte, bitmapRequiredSize(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var builder BitmapBuilder
		for i := 0; i < size; i++ {
			if v[i] {
				builder.Set(i)
			}
		}
		_ = builder.Finish(0, size, 0, data)
	}
}

// encodeTestBitmap encodes a bitmap of rows bits at offset 0 using a
// BitmapBuilder. Each index in set is set (Finish excludes indexes ≥ rows)
// before the optional Invert. It returns the encoding and the logical bits the
// decoded bitmap must hold.
func encodeTestBitmap(rows int, set []int, invert bool) (data []byte, want []bool) {
	var bb BitmapBuilder
	want = make([]bool, rows)
	for _, i := range set {
		bb.Set(i)
		if i < rows {
			want[i] = true
		}
	}
	if invert {
		bb.Invert(rows)
		for i := range want {
			want[i] = !want[i]
		}
	}
	data = make([]byte, bb.Size(rows, 0))
	if end := bb.Finish(0, rows, 0, data); int(end) != len(data) {
		panic(fmt.Sprintf("Finish returned %d; Size returned %d", end, len(data)))
	}
	return data, want
}

// tryDecodeBitmap decodes a bitmap encoded at offset 0 of data, converting a
// panic into an error.
func tryDecodeBitmap(data []byte, bitCount uint32) (bm Bitmap, err error) {
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			if err, ok = r.(error); !ok {
				err = errors.Errorf("%v", r)
			}
		}
	}()
	bm, end := DecodeBitmap(data, 0, bitCount)
	if end != uint64(len(data)) {
		return bm, errors.Errorf("DecodeBitmap returned endOffset %d; encoding is %d bytes", end, len(data))
	}
	return bm, nil
}

func rowRange(start, end int) []int {
	s := make([]int, 0, end-start)
	for i := start; i < end; i++ {
		s = append(s, i)
	}
	return s
}

// TestBitmapDecodeWriterRoundTrip verifies that DecodeBitmap accepts the
// encodings BitmapBuilder produces. TestBitmapRandom covers the seek methods
// over writer-produced bitmaps.
func TestBitmapDecodeWriterRoundTrip(t *testing.T) {
	sets := []struct {
		name string
		set  func(rows int) []int
	}{
		{"none", func(int) []int { return nil }},
		// Inverted, every primary word is zero but the default encoding is used.
		{"all", func(rows int) []int { return rowRange(0, rows) }},
		// Only the first word is materialized; Finish zero-fills the rest.
		{"first", func(int) []int { return []int{0} }},
		// Every other word is zero, so the summary has unset bits.
		{"alternate-words", func(rows int) []int {
			var s []int
			for i := 0; i < rows; i += 7 {
				if (i>>6)%2 == 0 {
					s = append(s, i)
				}
			}
			return s
		}},
		// Finish with fewer rows than were set.
		{"set-beyond-rows", func(rows int) []int { return rowRange(0, rows+130) }},
		{"set-only-beyond-rows", func(rows int) []int { return rowRange(rows, rows+130) }},
	}
	// Partial and full last words, and both sides of the 64-word boundary of a
	// summary word.
	for _, rows := range []int{1, 63, 64, 65, 4096, 4097} {
		for _, s := range sets {
			for _, invert := range []bool{false, true} {
				t.Run(fmt.Sprintf("rows=%d/%s/invert=%t", rows, s.name, invert), func(t *testing.T) {
					data, want := encodeTestBitmap(rows, s.set(rows), invert)
					bm, err := tryDecodeBitmap(data, uint32(rows))
					require.NoError(t, err)
					for i := range want {
						if got := bm.At(i); got != want[i] {
							t.Fatalf("At(%d) = %t; want %t", i, got, want[i])
						}
					}
				})
			}
		}
	}
}

// TestBitmapDecodeRejectsMalformedEncoding verifies that DecodeBitmap rejects
// well-sized encodings that violate the invariants the seek methods rely on.
func TestBitmapDecodeRejectsMalformedEncoding(t *testing.T) {
	// flip flips a bit of the idx'th word (primary words followed by summary
	// words) of a default-encoded bitmap at offset 0.
	flip := func(idx, bit int) func([]byte) {
		return func(data []byte) {
			off := align64 + idx<<align64Shift
			binary.LittleEndian.PutUint64(data[off:], binary.LittleEndian.Uint64(data[off:])^(1<<bit))
		}
	}
	testCases := []struct {
		name   string
		rows   int
		set    []int
		mutate func([]byte)
	}{
		{"unknown encoding", 65, []int{0}, func(data []byte) { data[0] = 2 }},
		{"primary padding", 65, []int{0}, flip(1, 1)},
		// A seek would read primary word 63 of a 2-word bitmap.
		{"summary beyond last word", 65, []int{0}, flip(2, 63)},
		// The second summary word of a 65-word bitmap summarizes one word.
		{"second summary word beyond last word", 4097, []int{0}, flip(66, 1)},
		{"stale summary set", 200, []int{0}, flip(4, 2)},
		{"stale summary clear", 200, []int{0, 130}, flip(4, 2)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := encodeTestBitmap(tc.rows, tc.set, false /* invert */)
			_, err := tryDecodeBitmap(data, uint32(tc.rows))
			require.NoError(t, err)
			tc.mutate(data)
			_, err = tryDecodeBitmap(data, uint32(tc.rows))
			require.True(t, errors.HasAssertionFailure(err), "%v", err)
		})
	}
}

// TestBitmapDecodeLargeBitCounts verifies the word arithmetic and decoding at
// the limits of int without allocating large buffers.
func TestBitmapDecodeLargeBitCounts(t *testing.T) {
	require.Equal(t, 0, bitmapWords(0))
	require.Equal(t, 1, bitmapWords(64))
	require.Equal(t, 2, bitmapWords(65))
	require.Equal(t, 1<<25, bitmapWords(math.MaxInt32))
	require.Equal(t, math.MaxInt/64+1, bitmapWords(math.MaxInt))
	require.Equal(t, (1<<25+1<<19)<<align64Shift, bitmapRequiredSize(math.MaxInt32))
	start, end := Bitmap{bitCount: math.MaxInt32}.summaryTableBounds()
	require.Equal(t, [2]int{1 << 25, 1<<25 + 1<<19}, [2]int{start, end})

	// The zero encoding is one byte for any bit count that fits in an int.
	zero := []byte{byte(zeroBitmapEncoding)}
	bm, err := tryDecodeBitmap(zero, math.MaxInt32)
	require.NoError(t, err)
	require.Equal(t, math.MaxInt32, bm.SeekSetBitGE(0))
	require.Equal(t, -1, bm.SeekSetBitLE(math.MaxInt32-1))
	for _, bitCount := range []uint32{math.MaxInt32 + 1, math.MaxUint32} {
		bm, err := tryDecodeBitmap(zero, bitCount)
		if strconv.IntSize == 32 {
			require.True(t, errors.HasAssertionFailure(err), "%v", err)
		} else {
			require.NoError(t, err)
			require.Equal(t, int(bitCount), bm.SeekSetBitGE(0))
		}
	}

	// The default encoding of a large bit count is rejected by the size check.
	small := make([]byte, 16)
	small[0] = byte(defaultBitmapEncoding)
	for _, bitCount := range []uint32{math.MaxInt32, math.MaxUint32} {
		_, err := tryDecodeBitmap(small, bitCount)
		require.True(t, errors.HasAssertionFailure(err), "%v", err)
	}
}

var bitmapBenchSink int

// randomBits returns each index in [0, rows) with probability p.
func randomBits(rng *rand.Rand, rows int, p float64) []int {
	var s []int
	for i := range rows {
		if rng.Float64() < p {
			s = append(s, i)
		}
	}
	return s
}

// bitmapBenchDensities are the bit probabilities of the benchmarked bitmaps.
// With p=0.01 about half of the primary words are zero.
var bitmapBenchDensities = []struct {
	name string
	p    float64
}{{"sparse", 0.01}, {"dense", 0.5}}

func BenchmarkBitmapDecode(b *testing.B) {
	for _, d := range bitmapBenchDensities {
		for _, rows := range []int{64, 1024, 4096, 65536} {
			rng := rand.New(rand.NewPCG(0, uint64(rows)))
			data, _ := encodeTestBitmap(rows, randomBits(rng, rows, d.p), false /* invert */)
			b.Run(fmt.Sprintf("%s/rows=%d", d.name, rows), func(b *testing.B) {
				for range b.N {
					bm, _ := DecodeBitmap(data, 0, uint32(rows))
					bitmapBenchSink += bm.bitCount
				}
			})
		}
	}
}

func BenchmarkBitmapSeek(b *testing.B) {
	const rows = 4096
	for _, d := range bitmapBenchDensities {
		rng := rand.New(rand.NewPCG(0, rows))
		data, _ := encodeTestBitmap(rows, randomBits(rng, rows, d.p), false /* invert */)
		bm, _ := DecodeBitmap(data, 0, rows)
		b.Run(fmt.Sprintf("%s/SeekSetBitGE", d.name), func(b *testing.B) {
			for i := range b.N {
				bitmapBenchSink += bm.SeekSetBitGE(i & (rows - 1))
			}
		})
		b.Run(fmt.Sprintf("%s/SeekSetBitLE", d.name), func(b *testing.B) {
			for i := range b.N {
				bitmapBenchSink += bm.SeekSetBitLE(i & (rows - 1))
			}
		})
	}
}
