// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"encoding/binary"
	"unsafe"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/v2/internal/invariants"
)

// UnsafeUints exposes a read-only view of integers from a column, transparently
// decoding data based on the UintEncoding.
//
// See UintEncoding and UintBuilder.
//
// The At() method is defined in endian_little.go and endian_big.go.
type UnsafeUints struct {
	base  uint64
	ptr   unsafe.Pointer
	width uint8
}

// Assert that UnsafeIntegerSlice implements Array.
var _ Array[uint64] = UnsafeUints{}

// DecodeUnsafeUints decodes the structure of a slice of uints from a
// byte slice.
func DecodeUnsafeUints(b []byte, off uint32, rows int) (_ UnsafeUints, endOffset uint32) {
	if rows < 0 || uint64(off) >= uint64(len(b)) {
		panic(errors.AssertionFailedf("uint column has invalid start %d or row count %d for %d-byte buffer",
			off, rows, len(b)))
	}
	if rows == 0 {
		// The final padding byte makes even an empty column's pointer valid.
		return makeUnsafeUints(0, unsafe.Pointer(&b[off]), 0), off
	}
	encoding := UintEncoding(b[off])
	if !encoding.IsValid() {
		panic(errors.AssertionFailedf("invalid encoding 0x%x", b[off:off+1]))
	}
	pos := uint64(off) + 1
	var base uint64
	if encoding.IsDelta() {
		base = binary.LittleEndian.Uint64(b[pos:])
		pos += 8
	}
	w := encoding.Width()
	if w > 0 {
		pos = align(pos, uint64(w))
	}
	// Accessors use unchecked pointer arithmetic. Prove the entire column
	// resides within this page before constructing an accessor.
	if pos >= uint64(len(b)) || pos > uint64(^uint32(0)) ||
		(w > 0 && uint64(rows) > (uint64(len(b))-pos)/uint64(w)) {
		panic(errors.AssertionFailedf("uint column exceeds its buffer: offset %d, rows %d, width %d, buffer %d",
			pos, rows, w, len(b)))
	}
	end := pos + uint64(rows)*uint64(w)
	if end > uint64(^uint32(0)) {
		panic(errors.AssertionFailedf("uint column end exceeds encoded offset limit: %d", end))
	}
	return makeUnsafeUints(base, unsafe.Pointer(&b[pos]), w), uint32(end)
}

// Assert that DecodeUnsafeIntegerSlice implements DecodeFunc.
var _ DecodeFunc[UnsafeUints] = DecodeUnsafeUints

func makeUnsafeUints(base uint64, ptr unsafe.Pointer, width int) UnsafeUints {
	switch width {
	case 0, 1, 2, 4, 8:
	default:
		panic("invalid width")
	}
	return UnsafeUints{
		base:  base,
		ptr:   ptr,
		width: uint8(width),
	}
}

// UnsafeOffsets is a specialization of UnsafeInts (providing the same
// functionality) which is optimized when the integers are offsets inside a
// column block. It can only be used with 0, 1, 2, or 4 byte encoding without
// delta.
//
// The At() and At2() methods are defined in endian_little.go and endian_big.go.
type UnsafeOffsets struct {
	ptr   unsafe.Pointer
	width uint8
}

// DecodeUnsafeOffsets decodes the structure of a slice of offsets from a byte
// slice.
func DecodeUnsafeOffsets(b []byte, off uint32, rows int) (_ UnsafeOffsets, endOffset uint32) {
	ints, endOffset := DecodeUnsafeUints(b, off, rows)
	if ints.base != 0 || ints.width == 8 {
		panic(errors.AssertionFailedf("unexpected offsets encoding (base=%d, width=%d)", ints.base, ints.width))
	}
	return UnsafeOffsets{
		ptr:   ints.ptr,
		width: ints.width,
	}, endOffset
}

// unsafeGetUint32 is just like slice[idx] but without bounds checking.
//
//gcassert:inline
func unsafeGetUint32(slice []uint32, idx int) uint32 {
	if invariants.Enabled {
		_ = slice[idx]
	}
	return *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slice)), uintptr(idx)<<align32Shift))
}

// unsafeSetUint32 is just like assigning to slice[idx] but without bounds
// checking.
//
//gcassert:inline
func unsafeSetUint32(slice []uint32, idx int, value uint32) {
	if invariants.Enabled {
		_ = slice[idx]
	}
	*(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slice)), uintptr(idx)<<align32Shift)) = value
}

// unsafeGetUint64 is just like slice[idx] but without bounds checking.
//
//gcassert:inline
func unsafeGetUint64(slice []uint64, idx int) uint64 {
	if invariants.Enabled {
		_ = slice[idx]
	}
	return *(*uint64)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slice)), uintptr(idx)<<align64Shift))
}
