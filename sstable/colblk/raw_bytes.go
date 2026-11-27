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
	"unsafe"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/binfmt"
	"github.com/cockroachdb/pebble/internal/invariants"
	"github.com/cockroachdb/pebble/internal/treeprinter"
)

// RawBytes holds an array of byte slices, stored as a concatenated data section
// and a series of offsets for each slice. Byte slices within RawBytes are
// stored in their entirety without any compression, ensuring stability without
// copying.
//
// # Representation
//
// An array of N byte slices encodes N+1 offsets. The beginning of the data
// representation holds an offsets table, in the same encoding as a
// DataTypeUint32 column. The integer offsets may be encoded using smaller width
// integers to save space if all offsets fit within an 8-bit or 16-bit uint.
// Each offset is relative to the beginning of the string data section (after
// the offset table).
//
// The use of UintEncoding conserves space in the common case. In the context of
// CockroachDB, the vast majority of offsets will fit in 16-bits when using 32
// KiB blocks (the size in use by CockroachDB). However, a single value larger
// than 65535 bytes requires an offset too large to fit within 16 bits, in which
// case offsets will be encoded as 32-bit integers.
//
//	+-------------------------------------------------------------------+
//	|        a uint offsets table, usually encoded with 16-bits,        |
//	|                possibly padded for alignment                      |
//	|                      (see UintEncoding)                           |
//	+-------------------------------------------------------------------+
//	|                           String Data                             |
//	|  abcabcada....                                                    |
//	+-------------------------------------------------------------------+
//
// The UintEncoding bits of the ColumnEncoding for a RawBytes column describes
// the encoding of the offset table.
type RawBytes struct {
	slices  int
	offsets UnsafeOffsets
	data    unsafe.Pointer
}

// Assert that RawBytes implements Array[[]byte].
var _ Array[[]byte] = RawBytes{}

// DecodeRawBytes decodes the structure of a RawBytes, constructing an accessor
// for an array of byte slices constructed by RawBytesBuilder. Count must be the
// number of byte slices within the array.
//
// The returned accessors compute slices from pairs of offsets using unchecked
// pointer arithmetic, so DecodeRawBytes validates the offsets table before
// returning: the table must lie within b, its offsets must be nondecreasing,
// and the final offset must not extend beyond b. For a positive offset width
// this is a single scan over the encoded offsets, bounded by len(b). A
// width-zero table encodes no per-row bytes (every offset is zero), so it is
// validated in constant time regardless of count.
func DecodeRawBytes(b []byte, offset uint64, count uint32) (rawBytes RawBytes, endOffset uint64) {
	if count == 0 {
		return RawBytes{}, offset
	}
	if count == math.MaxUint32 {
		panic(errors.AssertionFailedf("count overflow"))
	}
	offsets, dataOff := DecodeUnsafeOffsets(b, offset, count+1 /* +1 offset */)
	// The offsets table occupies the tableLen bytes immediately preceding
	// dataOff. Prove it lies within b before reading any offset; the data
	// pointer below also requires dataOff to address a byte of b.
	tableLen := (uint64(count) + 1) * uint64(offsets.width)
	if dataOff >= uint64(len(b)) {
		panic(errors.AssertionFailedf("RawBytes offsets table extends beyond slice: data offset %d, slice length %d",
			errors.Safe(dataOff), errors.Safe(len(b))))
	}
	last := checkRawBytesOffsets(b[dataOff-tableLen:dataOff], offsets.width)
	if uint64(last) > uint64(len(b))-dataOff {
		panic(errors.AssertionFailedf("RawBytes data extends beyond slice: end offset %d, slice length %d",
			errors.Safe(dataOff+uint64(last)), errors.Safe(len(b))))
	}
	return RawBytes{
		slices:  int(count),
		offsets: offsets,
		data:    unsafe.Pointer(&b[dataOff]),
	}, dataOff + uint64(last)
}

// checkRawBytesOffsets verifies that the little-endian offsets of the given
// width encoded in table are nondecreasing, and returns the final offset. A
// width-zero table is empty and every offset it represents is zero.
//
// The scan reads the encoded bytes through bounds-checked slices rather than
// UnsafeOffsets, so it is independent of the platform's byte order. It runs on
// every block cache miss, so it loads eight bytes at a time and accumulates the
// differences between adjacent offsets without branching: a decrease wraps a
// difference to a value with the high bit set.
func checkRawBytesOffsets(table []byte, width uint8) (last uint32) {
	orig := table
	var prev, bad uint64
	switch width {
	case 0:
		return 0
	case 1:
		for ; len(table) >= 8; table = table[8:] {
			x := binary.LittleEndian.Uint64(table)
			v0, v1, v2, v3 := x&0xff, x>>8&0xff, x>>16&0xff, x>>24&0xff
			v4, v5, v6, v7 := x>>32&0xff, x>>40&0xff, x>>48&0xff, x>>56
			bad |= (v0 - prev) | (v1 - v0) | (v2 - v1) | (v3 - v2) |
				(v4 - v3) | (v5 - v4) | (v6 - v5) | (v7 - v6)
			prev = v7
		}
		for _, b := range table {
			bad |= uint64(b) - prev
			prev = uint64(b)
		}
	case 2:
		for ; len(table) >= 8; table = table[8:] {
			x := binary.LittleEndian.Uint64(table)
			v0, v1, v2, v3 := x&0xffff, x>>16&0xffff, x>>32&0xffff, x>>48
			bad |= (v0 - prev) | (v1 - v0) | (v2 - v1) | (v3 - v2)
			prev = v3
		}
		for ; len(table) >= 2; table = table[2:] {
			v := uint64(binary.LittleEndian.Uint16(table))
			bad |= v - prev
			prev = v
		}
	case 4:
		for ; len(table) >= 8; table = table[8:] {
			x := binary.LittleEndian.Uint64(table)
			v0, v1 := x&0xffffffff, x>>32
			bad |= (v0 - prev) | (v1 - v0)
			prev = v1
		}
		if len(table) >= 4 {
			v := uint64(binary.LittleEndian.Uint32(table))
			bad |= v - prev
			prev = v
		}
	default:
		panic(errors.AssertionFailedf("invalid RawBytes offset width %d", errors.Safe(width)))
	}
	if bad>>63 != 0 {
		panicNonmonotoneOffsets(orig, width)
	}
	return uint32(prev)
}

// panicNonmonotoneOffsets panics, identifying the first offset in table that is
// less than its predecessor.
func panicNonmonotoneOffsets(table []byte, width uint8) {
	var prev uint32
	for i := range len(table) / int(width) {
		var v uint32
		switch width {
		case 1:
			v = uint32(table[i])
		case 2:
			v = uint32(binary.LittleEndian.Uint16(table[2*i:]))
		case 4:
			v = binary.LittleEndian.Uint32(table[4*i:])
		}
		if v < prev {
			panic(errors.AssertionFailedf("RawBytes offset %d (%d) is less than the preceding offset (%d)",
				errors.Safe(i), errors.Safe(v), errors.Safe(prev)))
		}
		prev = v
	}
	panic(errors.AssertionFailedf("RawBytes offsets are not nondecreasing"))
}

// Assert that DecodeRawBytes implements DecodeFunc.
var _ DecodeFunc[RawBytes] = DecodeRawBytes

func defaultSliceFormatter(x []byte) string {
	if bytes.ContainsFunc(x, func(r rune) bool { return r < 32 || r > 126 }) {
		return fmt.Sprintf("%q", x)
	}
	return string(x)
}

func rawBytesToBinFormatter(
	f *binfmt.Formatter, tp treeprinter.Node, count int, sliceFormatter func([]byte) string,
) {
	if count == 0 {
		return
	}
	if sliceFormatter == nil {
		sliceFormatter = defaultSliceFormatter
	}

	data := f.RelativeData()
	off := f.RelativeOffset()
	start := unsafe.Pointer(&data[off])
	rb, _ := DecodeRawBytes(data, uint64(off), uint32(count))
	dataOffset := uint64(off) + uint64(uintptr(rb.data)-uintptr(start))
	n := tp.Child("offsets table")
	uintsToBinFormatter(f, n, count+1, func(offset, base uint64) string {
		// NB: base is always zero for RawBytes columns.
		return fmt.Sprintf("%d [%d overall]", offset+base, offset+base+dataOffset)
	})
	n = tp.Child("data")
	for i := 0; i < rb.slices; i++ {
		s := rb.At(i)
		f.HexBytesln(len(s), "data[%d]: %s", i, sliceFormatter(s))
	}
	f.ToTreePrinter(n)
}

//gcassert:inline
func (b *RawBytes) ptr(offset uint32) unsafe.Pointer {
	return unsafe.Pointer(uintptr(b.data) + uintptr(offset))
}

// Slice returns the bytes of the data section in [start, end) without bounds
// checking. Callers must take start and end from this RawBytes' offsets table
// (indexes in [0, Slices()], start's index <= end's index); DecodeRawBytes
// proved those offsets are nondecreasing and within the data section.
//
//gcassert:inline
func (b *RawBytes) Slice(start, end uint32) []byte {
	return unsafe.Slice((*byte)(b.ptr(start)), end-start)
}

// Offsets returns the start and end offsets of the i'th slice. The index is
// only checked in invariants builds; callers must ensure 0 <= i < Slices().
//
//gcassert:inline
func (b *RawBytes) Offsets(i int) (start, end uint32) {
	invariants.CheckBounds(i, b.slices)
	return b.offsets.At2(i)
}

// At returns the []byte at index i. The returned slice should not be mutated.
// The index is only checked in invariants builds; callers must ensure
// 0 <= i < Slices().
func (b RawBytes) At(i int) []byte {
	invariants.CheckBounds(i, b.slices)
	return b.Slice(b.offsets.At2(i))
}

// Slices returns the number of []byte slices encoded within the RawBytes.
func (b *RawBytes) Slices() int {
	return b.slices
}

// RawBytesBuilder encodes a column of byte slices.
type RawBytesBuilder struct {
	rows    int
	data    []byte
	offsets UintBuilder
}

// Assert that *RawBytesBuilder implements ColumnWriter.
var _ ColumnWriter = (*RawBytesBuilder)(nil)

// Init initializes the builder for first-time use.
func (b *RawBytesBuilder) Init() {
	b.offsets.Init()
	b.Reset()
}

// Reset resets the builder to an empty state.
func (b *RawBytesBuilder) Reset() {
	b.rows = 0
	invariants.MaybeMangle(b.data)
	b.data = b.data[:0]
	b.offsets.Reset()
	// Add an initial offset of zero to streamline the logic in RawBytes.At() to
	// avoid needing a special case for row 0.
	b.offsets.Set(0, 0)
}

// NumColumns implements ColumnWriter.
func (b *RawBytesBuilder) NumColumns() int { return 1 }

// DataType implements ColumnWriter.
func (b *RawBytesBuilder) DataType(int) DataType { return DataTypeBytes }

// Put appends the provided byte slice to the builder.
func (b *RawBytesBuilder) Put(s []byte) {
	b.data = append(b.data, s...)
	b.rows++
	b.offsets.Set(b.rows, uint64(len(b.data)))
}

// PutConcat appends a single byte slice formed by the concatenation of the two
// byte slice arguments.
func (b *RawBytesBuilder) PutConcat(s1, s2 []byte) {
	b.data = append(append(b.data, s1...), s2...)
	b.rows++
	b.offsets.Set(b.rows, uint64(len(b.data)))
}

// Rows returns the count of slices that have been added to the builder.
func (b *RawBytesBuilder) Rows() int {
	return b.rows
}

// UnsafeGet returns the i'th slice added to the builder. The returned slice is
// owned by the builder and must not be mutated.
func (b *RawBytesBuilder) UnsafeGet(i int) []byte {
	if b.rows == 0 {
		return nil
	}
	start := unsafeGetUint64(b.offsets.elems, i)
	end := unsafeGetUint64(b.offsets.elems, i+1)
	return b.data[start:end]
}

// Finish writes the serialized byte slices to buf starting at offset. The buf
// slice must be sufficiently large to store the serialized output. The caller
// should use [Size] to size buf appropriately before calling Finish.
func (b *RawBytesBuilder) Finish(col, rows int, offset uint32, buf []byte) uint32 {
	if rows == 0 {
		return offset
	}
	dataLen := b.offsets.Get(rows)
	offset = b.offsets.Finish(0, rows+1, offset, buf)
	// Copy the data section.
	return offset + uint32(copy(buf[offset:], b.data[:dataLen]))
}

// Size computes the size required to encode the byte slices beginning in a
// buffer at the provided offset. The offset is required to ensure proper
// alignment. The returned uint32 is the offset of the first byte after the end
// of the encoded data. To compute the size in bytes, subtract the [offset]
// passed into Size from the returned offset.
func (b *RawBytesBuilder) Size(rows int, offset uint32) uint32 {
	if rows == 0 {
		return offset
	}
	// Get the size needed to encode the rows+1 offsets.
	offset = b.offsets.Size(rows+1, offset)
	// Add the value of offset[rows] since that is the accumulated size of the
	// first [rows] slices.
	return offset + uint32(b.offsets.Get(rows))
}

// WriteDebug implements Encoder.
func (b *RawBytesBuilder) WriteDebug(w io.Writer, rows int) {
	fmt.Fprintf(w, "bytes: %d rows set; %d bytes in data", b.rows, len(b.data))
}
