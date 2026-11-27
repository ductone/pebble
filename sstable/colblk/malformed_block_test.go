// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/cockroachdb/crlib/crbytes"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/binfmt"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/treeprinter"
	"github.com/stretchr/testify/require"
)

// requireAssertionPanic requires fn to fail an explicit check with an assertion
// failure, rather than not panicking or panicking with a runtime error after
// having gone past the check.
func requireAssertionPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		err, ok := r.(error)
		require.True(t, ok && errors.HasAssertionFailure(err), "expected an assertion failure; got %v", r)
	}()
	fn()
}

func uintsBlock(cols ...[]uint64) []byte {
	writers := make([]ColumnWriter, len(cols))
	for i, vals := range cols {
		var w UintBuilder
		w.Init()
		for row, v := range vals {
			w.Set(row, v)
		}
		writers[i] = &w
	}
	return FinishBlock(len(cols[0]), writers)
}

func TestBlockDecoderMalformedHeader(t *testing.T) {
	// pageStartOffset returns the offset of the col'th column's page start
	// within the block header.
	pageStartOffset := func(col int) int { return blockHeaderBaseSize + col*columnHeaderSize + 1 }
	for _, tc := range []struct {
		name    string
		corrupt func(data []byte) []byte
	}{
		{"truncated header", func(data []byte) []byte { return data[:blockHeaderBaseSize-1] }},
		{"unknown version", func(data []byte) []byte {
			data[0] = 2
			return data
		}},
		{"column headers beyond block", func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[1:], math.MaxUint16)
			return data
		}},
		{"page within column headers", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[pageStartOffset(0):], blockHeaderBaseSize)
			return data
		}},
		{"pages out of order", func(data []byte) []byte {
			start := binary.LittleEndian.Uint32(data[pageStartOffset(0):])
			binary.LittleEndian.PutUint32(data[pageStartOffset(1):], start-1)
			return data
		}},
		{"page beyond block", func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[pageStartOffset(1):], uint32(len(data)))
			return data
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.corrupt(uintsBlock([]uint64{1, 2, 3}, []uint64{4, 5, 6}))
			requireAssertionPanic(t, func() { DecodeBlock(data, 0) })
		})
	}
	t.Run("custom header beyond block", func(t *testing.T) {
		data := uintsBlock([]uint64{1, 2, 3})
		requireAssertionPanic(t, func() { DecodeBlock(data, uint32(len(data))) })
	})
}

func TestBlockDecoderColumnIndex(t *testing.T) {
	data := uintsBlock([]uint64{1})
	d := DecodeBlock(data, 0)
	// Column indexes must not wrap into range when truncated to 16 bits. The
	// formatter reaches the unchecked pageStart only after DataType checks col.
	for _, col := range []int{-1, 1, 1 << 16, -1 << 16} {
		requireAssertionPanic(t, func() { d.DataType(col) })
		requireAssertionPanic(t, func() { d.Uints(col) })
		requireAssertionPanic(t, func() {
			d.ColumnToBinFormatter(binfmt.New(data), treeprinter.New().Child("block"), col, 1)
		})
	}
}

func TestDecodeColumnConfinesDecoder(t *testing.T) {
	data := uintsBlock([]uint64{1, 2, 3}, []uint64{4, 5, 6})
	d := DecodeBlock(data, 0)
	for col := range 2 {
		end := d.pageStart(col + 1)
		DecodeColumn(&d, col, 3, DataTypeUint, func(b []byte, off uint64, rows uint32) (UnsafeUints, uint64) {
			// The decoder may address the byte following its page, but no more.
			require.Equal(t, end+1, uint64(len(b)))
			require.Equal(t, end+1, uint64(cap(b)))
			return DecodeUnsafeUints(b, off, rows)
		})
	}
	// Widening column 0 makes it claim bytes from column 1's page. Those
	// bytes are within the block, but not within column 0's page.
	data[d.pageStart(0)] = byte(makeUintEncoding(8, false))
	requireAssertionPanic(t, func() { d.Uints(0) })
}

func TestDecodeUnsafeUintsExtent(t *testing.T) {
	var w UintBuilder
	w.Init()
	for row, v := range []uint64{1, 2, 3} {
		w.Set(row, v)
	}
	buf := crbytes.AllocAligned(int(w.Size(3, 0)) + 1 /* trailing padding byte */)
	w.Finish(0, 3, 0, buf)
	require.Equal(t, makeUintEncoding(1, false), UintEncoding(buf[0]))
	for _, rows := range []uint32{5, math.MaxUint32} {
		requireAssertionPanic(t, func() { DecodeUnsafeUints(buf, 0, rows) })
		requireAssertionPanic(t, func() { DecodeUnsafeOffsets(buf, 0, rows) })
	}
	// A constant column has no per-row bytes, but its pointer must still
	// address a byte of the buffer.
	requireAssertionPanic(t, func() { DecodeUnsafeUints([]byte{byte(makeUintEncoding(0, false))}, 0, 1) })
}

// TestDecodeNestedOffsetsExtent tests that RawBytes and PrefixBytes columns
// reject a count exceeding their encoded offsets before reading the final
// offset through an unchecked pointer.
func TestDecodeNestedOffsetsExtent(t *testing.T) {
	var rb RawBytesBuilder
	rb.Init()
	rb.Put([]byte("a"))
	rb.Put([]byte("bc"))
	var pb PrefixBytesBuilder
	pb.Init(4)
	pb.Put([]byte("a"), 0)
	pb.Put([]byte("ab"), 1)
	d := DecodeBlock(FinishBlock(2, []ColumnWriter{&rb, &pb}), 0)
	for _, count := range []uint32{1 << 20, math.MaxUint32 - 1} {
		requireAssertionPanic(t, func() { DecodeColumn(&d, 0, count, DataTypeBytes, DecodeRawBytes) })
		requireAssertionPanic(t, func() { DecodeColumn(&d, 1, count, DataTypePrefixBytes, DecodePrefixBytes) })
	}

	// Keyspan blocks encode the boundary key count in their custom header.
	var w KeyspanBlockWriter
	w.Init(bytes.Equal)
	w.AddSpan(keyspan.ParseSpan("a-c:{(#100,RANGEDEL)}"))
	data := w.Finish()
	binary.LittleEndian.PutUint32(data, 1<<20)
	var kd KeyspanDecoder
	requireAssertionPanic(t, func() { kd.Init(data) })
}

func TestBlockDecoderValidEdgeCases(t *testing.T) {
	t.Run("zero rows", func(t *testing.T) {
		var ub UintBuilder
		ub.Init()
		var rb RawBytesBuilder
		rb.Init()
		var bb BitmapBuilder
		bb.Reset()
		// Each column type is the final, empty column of a block in turn.
		for _, w := range []ColumnWriter{&ub, &rb, &bb} {
			d := DecodeBlock(FinishBlock(0, []ColumnWriter{&ub, w}), 0)
			switch d.DataType(1) {
			case DataTypeUint:
				d.Uints(1)
			case DataTypeBytes:
				raw := d.RawBytes(1)
				require.Equal(t, 0, raw.Slices())
			case DataTypeBool:
				d.Bitmap(1)
			}
		}
	})
	t.Run("constant column with many rows", func(t *testing.T) {
		var w UintBuilder
		w.InitWithDefault()
		const rows = 1 << 20
		data := FinishBlock(rows, []ColumnWriter{&w})
		require.Less(t, len(data), 64)
		d := DecodeBlock(data, 0)
		require.Equal(t, uint64(0), d.Uints(0).At(rows-1))
	})
	t.Run("delta encoding", func(t *testing.T) {
		vals := []uint64{1 << 40, 1<<40 + 1, 1<<40 + 255}
		d := DecodeBlock(uintsBlock(vals), 0)
		require.Equal(t, makeUintEncoding(1, true), UintEncoding(d.data[d.pageStart(0)]))
		u := d.Uints(0)
		for row, v := range vals {
			require.Equal(t, v, u.At(row))
		}
	})
	t.Run("large values", func(t *testing.T) {
		vals := [][]byte{[]byte("a"), bytes.Repeat([]byte("b"), 1<<17)}
		var w RawBytesBuilder
		w.Init()
		for _, v := range vals {
			w.Put(v)
		}
		d := DecodeBlock(FinishBlock(len(vals), []ColumnWriter{&w}), 0)
		rb := d.RawBytes(0)
		require.Equal(t, uint8(4), rb.offsets.width)
		for row, v := range vals {
			require.Equal(t, v, rb.At(row))
		}
	})
}

// FuzzDecodeBlock decodes every column of a block. Decoding may panic with an
// error, but must not access memory outside of the block.
func FuzzDecodeBlock(f *testing.F) {
	var ub UintBuilder
	ub.Init()
	ub.Set(0, 1)
	ub.Set(1, 1<<40)
	var rb RawBytesBuilder
	rb.Init()
	rb.Put([]byte("a"))
	rb.Put(nil)
	var pb PrefixBytesBuilder
	pb.Init(2)
	pb.Put([]byte("a"), 0)
	pb.Put([]byte("ab"), 1)
	var bb BitmapBuilder
	bb.Reset()
	bb.Set(1)
	f.Add(FinishBlock(2, []ColumnWriter{&ub, &rb, &pb, &bb}))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Production blocks are aligned; see DataBlockDecoder.Init.
		buf := crbytes.CopyAligned(data)
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(error); !ok {
					t.Fatalf("unexpected panic: %v", r)
				}
			}
		}()
		d := DecodeBlock(buf, 0)
		for col := range int(d.header.Columns) {
			switch d.DataType(col) {
			case DataTypeBool:
				d.Bitmap(col)
			case DataTypeUint:
				u := d.Uints(col)
				for row := range min(d.Rows(), 1024) {
					u.At(row)
				}
			case DataTypeBytes:
				d.RawBytes(col)
			case DataTypePrefixBytes:
				d.PrefixBytes(col)
			}
		}
	})
}
