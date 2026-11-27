// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unsafe"

	"github.com/cockroachdb/crlib/crbytes"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/stretchr/testify/require"
)

// requireOffsetsAssertion requires fn to fail an explicit decoder check with
// an assertion failure, rather than returning or failing with a runtime error
// after reading through an invalid offset.
func requireOffsetsAssertion(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		err, ok := r.(error)
		require.True(t, ok && errors.HasAssertionFailure(err), "expected an assertion failure; got %v", r)
	}()
	fn()
}

// offsetsTable locates the encoded offsets table of a RawBytes (or, if prefix
// is set, PrefixBytes) column encoding slices byte slices, within a block
// produced by a writer. It returns the table's bytes, aliasing d's data, and
// the width of each offset.
func offsetsTable(t *testing.T, d *BlockDecoder, col, slices int, prefix bool) ([]byte, int) {
	t.Helper()
	off := d.pageStart(col)
	if prefix {
		off++ // Skip the bundle size.
	}
	enc := UintEncoding(d.data[off])
	require.True(t, enc.IsValid())
	require.False(t, enc.IsDelta())
	w := enc.Width()
	off++
	if w > 0 {
		off = align(off, uint64(w))
	}
	return d.data[off : off+uint64(slices+1)*uint64(w)], w
}

func getOffset(table []byte, w, i int) uint32 {
	switch w {
	case 1:
		return uint32(table[i])
	case 2:
		return uint32(binary.LittleEndian.Uint16(table[2*i:]))
	case 4:
		return binary.LittleEndian.Uint32(table[4*i:])
	}
	panic("unreachable")
}

func setOffset(table []byte, w, i int, v uint32) {
	switch w {
	case 1:
		table[i] = byte(v)
	case 2:
		binary.LittleEndian.PutUint16(table[2*i:], uint16(v))
	case 4:
		binary.LittleEndian.PutUint32(table[4*i:], v)
	default:
		panic("unreachable")
	}
}

// offsetMutation describes a corruption of a valid offsets table encoding n
// slices. Each mutation leaves the final offset within the buffer, so it is
// not caught by bounding the final offset alone.
type offsetMutation struct {
	name   string
	mutate func(table []byte, w, n int)
}

var intermediateOffsetMutations = []offsetMutation{
	{"intermediate decrease", func(table []byte, w, n int) {
		k := n / 2
		setOffset(table, w, k, getOffset(table, w, k-1)-1)
	}},
	{"intermediate past final", func(table []byte, w, n int) {
		setOffset(table, w, n/2, getOffset(table, w, n)+1)
	}},
	{"first past final", func(table []byte, w, n int) {
		setOffset(table, w, 0, getOffset(table, w, n)+1)
	}},
	{"intermediate to zero", func(table []byte, w, n int) {
		setOffset(table, w, n-1, 0)
	}},
}

func rawBytesBlock(vals [][]byte) []byte {
	var rb RawBytesBuilder
	rb.Init()
	for _, v := range vals {
		rb.Put(v)
	}
	// A following column ensures the RawBytes page is not the last in the
	// block, so a corrupt offset could address another column's bytes.
	var ub UintBuilder
	ub.Init()
	for i := range vals {
		ub.Set(i, uint64(i))
	}
	return FinishBlock(len(vals), []ColumnWriter{&rb, &ub})
}

// TestRawBytesIntermediateOffsets tests that decoding a RawBytes column
// rejects offsets that are not nondecreasing, for each offset width, through
// both BlockDecoder and a direct DecodeRawBytes call. Before the fix, these
// blocks decoded successfully because only the final offset was bounded.
func TestRawBytesIntermediateOffsets(t *testing.T) {
	for _, tc := range []struct {
		width  int
		valLen int
	}{
		{width: 1, valLen: 5},
		{width: 2, valLen: 100},
		{width: 4, valLen: 10000},
	} {
		const n = 8
		vals := make([][]byte, n)
		for i := range vals {
			vals[i] = bytes.Repeat([]byte{byte('a' + i)}, tc.valLen+i)
		}
		valid := rawBytesBlock(vals)
		t.Run(fmt.Sprintf("width=%d/valid", tc.width), func(t *testing.T) {
			d := DecodeBlock(valid, 0)
			_, w := offsetsTable(t, &d, 0, n, false)
			require.Equal(t, tc.width, w)
			rb := d.RawBytes(0)
			require.Equal(t, n, rb.Slices())
			for i, v := range vals {
				require.Equal(t, v, rb.At(i))
			}
		})
		for _, m := range intermediateOffsetMutations {
			t.Run(fmt.Sprintf("width=%d/%s", tc.width, m.name), func(t *testing.T) {
				data := crbytes.CopyAligned(valid)
				d := DecodeBlock(data, 0)
				table, w := offsetsTable(t, &d, 0, n, false)
				m.mutate(table, w, n)
				requireOffsetsAssertion(t, func() { d.RawBytes(0) })
				requireOffsetsAssertion(t, func() { DecodeRawBytes(data, d.pageStart(0), n) })
			})
		}
		t.Run(fmt.Sprintf("width=%d/final beyond slice", tc.width), func(t *testing.T) {
			data := crbytes.CopyAligned(valid)
			d := DecodeBlock(data, 0)
			table, w := offsetsTable(t, &d, 0, n, false)
			// The largest offset of this width exceeds the small block.
			setOffset(table, w, n, uint32(math.MaxUint32>>(32-8*w)))
			requireOffsetsAssertion(t, func() { DecodeRawBytes(data, d.pageStart(0), n) })
		})
	}
}

// TestPrefixBytesIntermediateOffsets tests that the shared RawBytes
// constructor also validates the offsets of a PrefixBytes column, whose first
// offset is the (nonzero) shared prefix length.
func TestPrefixBytesIntermediateOffsets(t *testing.T) {
	var pb PrefixBytesBuilder
	pb.Init(4)
	keys := []string{"apple", "applesauce", "apricot", "banana", "bandana", "bandanas", "cherry", "date", "dates"}
	for i, k := range keys {
		var sharedWithPrev int
		if i > 0 {
			sharedWithPrev = crbytes.CommonPrefix([]byte(keys[i-1]), []byte(k))
		}
		pb.Put([]byte(k), sharedWithPrev)
	}
	n := len(keys)
	valid := FinishBlock(n, []ColumnWriter{&pb})
	d := DecodeBlock(valid, 0)
	p := d.PrefixBytes(0)
	slices := p.rawBytes.Slices()
	table, w := offsetsTable(t, &d, 0, slices, true)
	require.Equal(t, uint32(p.sharedPrefixLen), getOffset(table, w, 0))
	for i, k := range keys {
		require.Equal(t, k, string(p.At(i)))
	}
	for _, m := range intermediateOffsetMutations {
		t.Run(m.name, func(t *testing.T) {
			data := crbytes.CopyAligned(valid)
			d := DecodeBlock(data, 0)
			table, w := offsetsTable(t, &d, 0, slices, true)
			m.mutate(table, w, slices)
			requireOffsetsAssertion(t, func() { d.PrefixBytes(0) })
		})
	}
}

// TestPrefixBytesRowSuffixEndBelowStart decreases each row's encoded suffix end
// offset to one below its start offset, so that end-start would wrap to
// math.MaxUint32 in PrefixBytesIter materialization (SetAt, and SetNext within
// and across bundles) and in RowSuffix. Decoding must reject the column as
// corrupt before any of those paths can read through the offsets.
func TestPrefixBytesRowSuffixEndBelowStart(t *testing.T) {
	var pb PrefixBytesBuilder
	pb.Init(2)
	// The block prefix "key" makes every start offset positive, and no row's
	// suffix is empty.
	keys := []string{"keyaa", "keyab", "keyba", "keybb", "keyca", "keycb"}
	for i, k := range keys {
		var sharedWithPrev int
		if i > 0 {
			sharedWithPrev = crbytes.CommonPrefix([]byte(keys[i-1]), []byte(k))
		}
		pb.Put([]byte(k), sharedWithPrev)
	}
	valid := FinishBlock(len(keys), []ColumnWriter{&pb})
	d := DecodeBlock(valid, 0)
	p := d.PrefixBytes(0)
	slices := p.rawBytes.Slices()
	for row := range keys {
		t.Run(fmt.Sprintf("row=%d", row), func(t *testing.T) {
			data := crbytes.CopyAligned(valid)
			d := DecodeBlock(data, 0)
			table, w := offsetsTable(t, &d, 0, slices, true)
			i := p.rowSuffixIndex(row)
			start, end := getOffset(table, w, i), getOffset(table, w, i+1)
			require.Positive(t, start)
			require.Less(t, start, end)
			setOffset(table, w, i+1, start-1)
			requireOffsetsAssertion(t, func() { d.PrefixBytes(0) })
			requireOffsetsAssertion(t, func() { DecodePrefixBytes(data, d.pageStart(0), uint32(len(keys))) })
		})
	}
}

// TestRawBytesValidOffsets round-trips randomly generated RawBytes columns,
// including empty and repeated slices, through the writer and decoder.
func TestRawBytesValidOffsets(t *testing.T) {
	seed := rand.Uint64()
	t.Logf("seed: %d", seed)
	rng := rand.New(rand.NewPCG(0, seed))
	for _, maxLen := range []int{0, 1, 30, 1000, 70000} {
		for range 20 {
			n := 1 + rng.IntN(64)
			vals := make([][]byte, n)
			for i := range vals {
				switch {
				case i > 0 && rng.IntN(4) == 0:
					vals[i] = vals[i-1]
				case maxLen > 0:
					vals[i] = bytes.Repeat([]byte{byte(rng.Uint32())}, rng.IntN(maxLen+1))
				}
			}
			data := rawBytesBlock(vals)
			d := DecodeBlock(data, 0)
			rb := d.RawBytes(0)
			require.Equal(t, n, rb.Slices())
			for i, v := range vals {
				require.Equal(t, len(v), len(rb.At(i)))
				require.True(t, bytes.Equal(v, rb.At(i)))
				start, end := rb.Offsets(i)
				require.Equal(t, rb.At(i), rb.Slice(start, end))
			}
			// The direct decoder reports the exact end of the column.
			_, end := DecodeRawBytes(data, d.pageStart(0), uint32(n))
			require.Equal(t, d.pageStart(1), end)
		}
	}
}

// TestRawBytesWidthZeroLargeCount tests that a RawBytes column whose offsets
// are all zero, and so encodes no per-slice bytes, decodes in constant time for
// a large logical count. Width-zero offsets are not rejected merely for the
// number of (empty) slices they represent.
func TestRawBytesWidthZeroLargeCount(t *testing.T) {
	data := rawBytesBlock([][]byte{nil, nil, nil})
	d := DecodeBlock(data, 0)
	table, w := offsetsTable(t, &d, 0, 3, false)
	require.Equal(t, 0, w)
	require.Empty(t, table)
	counts := []uint32{3, 1 << 30, math.MaxUint32 - 1}
	// On 32-bit platforms, the largest count an accessor can represent is
	// math.MaxInt; larger counts are rejected rather than stored as a negative
	// number of slices.
	if maxInt := uint64(math.MaxInt); maxInt < math.MaxUint32 {
		counts = append(counts, uint32(maxInt), uint32(maxInt)+1)
	}
	for _, count := range counts {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			if uint64(count) > math.MaxInt {
				requireOffsetsAssertion(t, func() { DecodeColumn(&d, 0, count, DataTypeBytes, DecodeRawBytes) })
				return
			}
			rb := DecodeColumn(&d, 0, count, DataTypeBytes, DecodeRawBytes)
			require.Equal(t, int(count), rb.Slices())
			require.Positive(t, rb.Slices())
			v := rb.At(int(count - 1))
			require.NotNil(t, v)
			require.Empty(t, v)
		})
	}
	requireOffsetsAssertion(t, func() { DecodeColumn(&d, 0, math.MaxUint32, DataTypeBytes, DecodeRawBytes) })
}

// TestRawBytesExactBufferEmptyLastSlice decodes RawBytesBuilder output from a
// buffer of exactly the encoded size, whose final slice is empty, so that the
// data section ends at the end of the buffer. Empty slices, including the
// final one, must be non-nil and must not point past the buffer.
func TestRawBytesExactBufferEmptyLastSlice(t *testing.T) {
	for _, vals := range [][][]byte{
		{[]byte("abc"), nil},
		{[]byte("abc"), {}, []byte("de"), nil},
		{[]byte("abc"), {}, {}},
	} {
		var w RawBytesBuilder
		w.Init()
		for _, v := range vals {
			w.Put(v)
		}
		n := len(vals)
		size := int(w.Size(n, 0))
		buf := crbytes.AllocAligned(size)[:size:size]
		require.Equal(t, uint32(size), w.Finish(0, n, 0, buf))
		rb, end := DecodeRawBytes(buf, 0, uint32(n))
		// The data section ends exactly at the end of the buffer.
		require.Equal(t, uint64(len(buf)), end)
		lo := uintptr(unsafe.Pointer(unsafe.SliceData(buf)))
		hi := lo + uintptr(len(buf))
		for i, v := range vals {
			got := rb.At(i)
			require.NotNil(t, got)
			require.Equal(t, len(v), len(got))
			require.Equal(t, len(v), cap(got))
			require.True(t, bytes.Equal(v, got))
			p := uintptr(unsafe.Pointer(unsafe.SliceData(got)))
			require.True(t, lo <= p && p < hi, "slice %d points outside the buffer", i)
			start, end := rb.Offsets(i)
			require.Equal(t, got, rb.Slice(start, end))
		}
	}
}

func requireCorruption(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	require.True(t, base.IsCorruptionError(err), "expected corruption error; got %v", err)
	require.Contains(t, err.Error(), "RawBytes offset")
}

// TestRawBytesOffsetsCachedMetadata tests that nonmonotone RawBytes offsets in
// writer-produced data, index and keyspan blocks are reported as corruption
// errors by the block metadata initializers used when loading a block into the
// block cache, and fail the corresponding direct decoders with an assertion.
func TestRawBytesOffsetsCachedMetadata(t *testing.T) {
	decrease := intermediateOffsetMutations[0].mutate

	t.Run("data block", func(t *testing.T) {
		var w DataBlockEncoder
		w.Init(&testKeysSchema, NoTieringColumns())
		const n = 10
		for i := range n {
			ik := base.MakeInternalKey([]byte(fmt.Sprintf("key%03d@%d", i, i+1)), base.SeqNum(i), base.InternalKeyKindSet)
			kcmp := w.KeyWriter.ComparePrev(ik.UserKey)
			w.Add(ik, []byte(strings.Repeat("v", i+1)), block.InPlaceValuePrefix(kcmp.PrefixEqual()), kcmp, false, base.KVMeta{})
		}
		valid, _ := w.Finish(w.Rows(), w.Size())
		customHeaderSize := DataBlockCustomHeaderSize + testKeysSchema.HeaderSize
		var md block.Metadata
		require.NoError(t, InitDataBlockMetadata(&testKeysSchema, &md, valid))

		for _, tc := range []struct {
			name   string
			col    int
			prefix bool
		}{
			{"values", len(testKeysSchema.ColumnTypes) + dataBlockColumnValue, false},
			{"key suffixes", defaultKeySchemaColumnSuffix, false},
			{"key prefixes", defaultKeySchemaColumnPrefix, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				data := crbytes.CopyAligned(valid)
				bd := DecodeBlock(data, customHeaderSize)
				slices := n
				if tc.prefix {
					pb := bd.PrefixBytes(tc.col)
					slices = pb.rawBytes.Slices()
				}
				table, w := offsetsTable(t, &bd, tc.col, slices, tc.prefix)
				require.NotZero(t, w)
				decrease(table, w, slices)
				var md block.Metadata
				err := InitDataBlockMetadata(&testKeysSchema, &md, data)
				requireCorruption(t, err)
				require.Contains(t, err.Error(), "data block metadata")
				requireOffsetsAssertion(t, func() {
					var dec DataBlockDecoder
					bd := dec.Init(&testKeysSchema, data)
					var meta KeySeekerMetadata
					testKeysSchema.InitKeySeekerMetadata(&meta, &dec, bd)
				})
			})
		}
	})

	t.Run("index block", func(t *testing.T) {
		var w IndexBlockWriter
		w.Init()
		const n = 6
		for i := range n {
			w.AddBlockHandle([]byte(fmt.Sprintf("sep%02d", i)), block.Handle{Offset: uint64(i) * 100, Length: 90}, []byte(strings.Repeat("p", i)))
		}
		valid := w.Finish(n)
		var md block.Metadata
		require.NoError(t, InitIndexBlockMetadata(&md, valid))
		for name, col := range map[string]int{
			"separators":       indexBlockColumnSeparator,
			"block properties": indexBlockColumnBlockProperties,
		} {
			t.Run(name, func(t *testing.T) {
				data := crbytes.CopyAligned(valid)
				bd := DecodeBlock(data, indexBlockCustomHeaderSize)
				table, w := offsetsTable(t, &bd, col, n, false)
				require.NotZero(t, w)
				decrease(table, w, n)
				err := InitIndexBlockMetadata(&md, data)
				requireCorruption(t, err)
				requireOffsetsAssertion(t, func() {
					var d IndexBlockDecoder
					d.Init(data)
				})
			})
		}
	})

	t.Run("keyspan block", func(t *testing.T) {
		var w KeyspanBlockWriter
		w.Init(bytes.Equal)
		for _, s := range []string{"a-bb:{(#3,RANGEDEL)}", "bb-ccc:{(#2,RANGEDEL)}", "ccc-dddd:{(#1,RANGEDEL)}", "dddd-eeeee:{(#1,RANGEDEL)}"} {
			w.AddSpan(keyspan.ParseSpan(s))
		}
		valid := w.Finish()
		var md block.Metadata
		require.NoError(t, InitKeyspanBlockMetadata(&md, valid))
		data := crbytes.CopyAligned(valid)
		bd := DecodeBlock(data, keyspanHeaderSize)
		n := int(binary.LittleEndian.Uint32(data))
		table, tw := offsetsTable(t, &bd, keyspanColBoundaryUserKeys, n, false)
		require.NotZero(t, tw)
		decrease(table, tw, n)
		requireCorruption(t, InitKeyspanBlockMetadata(&md, data))
		requireOffsetsAssertion(t, func() {
			var d KeyspanDecoder
			d.Init(data)
		})
	})
}
