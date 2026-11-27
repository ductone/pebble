// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
	"unsafe"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/blockiter"
)

// corruptOffset changes one encoded offset without changing the final offset.
// A final-offset check alone therefore cannot detect these corruptions.
func corruptOffset(t *testing.T, data []byte, col, count, index int, prefix bool) {
	t.Helper()
	d := DecodeBlock(data, 0)
	start := uint64(d.pageStart(col))
	if prefix {
		start++ // PrefixBytes begins with its bundle shift.
	}
	offsets, _ := DecodeUnsafeOffsets(data, start, uint32(count+1))
	pos := int(uintptr(offsets.ptr)-uintptr(unsafe.Pointer(unsafe.SliceData(data)))) + index*int(offsets.width)
	switch offsets.width {
	case 1:
		data[pos] = 100
	case 2:
		binary.LittleEndian.PutUint16(data[pos:], 100)
	case 4:
		binary.LittleEndian.PutUint32(data[pos:], 100)
	default:
		t.Fatalf("test requires nonzero-width offsets, got %d", offsets.width)
	}
}

func TestMalformedColumnarBlockBounds(t *testing.T) {
	t.Run("page starts beyond block", func(t *testing.T) {
		var w RawBytesBuilder
		w.Reset()
		w.Put([]byte("key"))
		data := FinishBlock(1, []ColumnWriter{&w})
		binary.LittleEndian.PutUint32(data[blockHeaderBaseSize+1:], uint32(len(data)+1))
		assertDecodePanics(t, func() { DecodeBlock(data, 0) })
	})
	t.Run("out-of-range column", func(t *testing.T) {
		var w UintBuilder
		w.Init()
		w.Set(0, 1)
		d := DecodeBlock(FinishBlock(1, []ColumnWriter{&w}), 0)
		for _, col := range []int{-1 << 16, 1 << 16} {
			assertDecodePanics(t, func() { d.DataType(col) })
		}
	})

	t.Run("intermediate raw bytes offset", func(t *testing.T) {
		var w RawBytesBuilder
		w.Reset()
		for _, v := range []string{"a", "b", "c"} {
			w.Put([]byte(v))
		}
		data := FinishBlock(3, []ColumnWriter{&w})
		corruptOffset(t, data, 0, 3, 2, false)
		assertDecodePanics(t, func() { d := DecodeBlock(data, 0); d.RawBytes(0) })
	})

	t.Run("intermediate prefix bytes offset", func(t *testing.T) {
		var w PrefixBytesBuilder
		w.Init(2)
		for i, v := range []string{"a", "ab", "ac"} {
			shared := 0
			if i > 0 {
				shared = 1
			}
			w.Put([]byte(v), shared)
		}
		data := FinishBlock(3, []ColumnWriter{&w})
		corruptOffset(t, data, 0, 5, 2, true) // Three rows and two bundles.
		assertDecodePanics(t, func() { d := DecodeBlock(data, 0); d.PrefixBytes(0) })
	})

	t.Run("integer column truncated by row count", func(t *testing.T) {
		var w UintBuilder
		w.Init()
		for i, v := range []uint64{1, 2, 3} {
			w.Set(i, v)
		}
		data := FinishBlock(3, []ColumnWriter{&w})
		binary.LittleEndian.PutUint32(data[3:], 100)
		assertDecodePanics(t, func() { d := DecodeBlock(data, 0); d.Uints(0) })
	})

	t.Run("bitmap summary points outside data words", func(t *testing.T) {
		var w BitmapBuilder
		w.Reset()
		w.Set(0)
		data := FinishBlock(3, []ColumnWriter{&w})
		d := DecodeBlock(data, 0)
		wordStart := align(uint64(d.pageStart(0))+1, align64)
		data[wordStart+8] = 0x80 // The summary claims word 7 is nonzero.
		assertDecodePanics(t, func() { d := DecodeBlock(data, 0); d.Bitmap(0) })
	})
}

func TestMalformedKeyspanBoundaryIndex(t *testing.T) {
	var w KeyspanBlockWriter
	w.Init(bytes.Equal)
	w.AddSpan(keyspan.ParseSpan("a-c:{(#100,RANGEDEL)}"))
	data := w.Finish()
	var d KeyspanDecoder
	d.Init(data)
	if d.boundaryKeyIndices.width != 1 {
		t.Fatalf("expected one-byte boundary indices, got width %d", d.boundaryKeyIndices.width)
	}
	pos := int(uintptr(d.boundaryKeyIndices.ptr)-uintptr(unsafe.Pointer(unsafe.SliceData(data)))) + 1
	data[pos] = 100 // There is only one key, but the next span index is 100.
	assertDecodePanics(t, func() { d.Init(data) })
}

func TestDataBlockMaximumKeyLengthBound(t *testing.T) {
	var w DataBlockEncoder
	w.Init(&testKeysSchema, NoTieringColumns())
	short := base.ParseInternalKey("a#1,SET")
	cmp := w.KeyWriter.ComparePrev(short.UserKey)
	w.Add(short, []byte("v"), block.InPlaceValuePrefix(cmp.PrefixEqual()), cmp, false, base.KVMeta{})
	size := w.Size()
	long := base.ParseInternalKey(string(bytes.Repeat([]byte("z"), 4096)) + "#1,SET")
	cmp = w.KeyWriter.ComparePrev(long.UserKey)
	w.Add(long, []byte("v"), block.InPlaceValuePrefix(cmp.PrefixEqual()), cmp, false, base.KVMeta{})
	data, _ := w.Finish(w.Rows()-1, size)
	if len(data) >= len(long.UserKey) {
		t.Fatalf("expected a block shorter than its excluded key: %d >= %d", len(data), len(long.UserKey))
	}
	recorded := binary.LittleEndian.Uint32(data[testKeysSchema.HeaderSize:])
	if recorded != uint32(len(long.UserKey)) {
		t.Fatalf("writer recorded key length %d, want %d", recorded, len(long.UserKey))
	}
	for _, keyLen := range []uint32{recorded, math.MaxUint32} {
		binary.LittleEndian.PutUint32(data[testKeysSchema.HeaderSize:], keyLen)
		var metadata block.Metadata
		if err := InitDataBlockMetadata(&testKeysSchema, &metadata, data); err != nil {
			t.Fatalf("recorded key length %d: %v", keyLen, err)
		}
		var d DataBlockDecoder
		bd := d.Init(&testKeysSchema, data)
		if d.maximumKeyLength != uint32(len(data)) {
			t.Fatalf("recorded key length %d: bounded length = %d, want %d",
				keyLen, d.maximumKeyLength, len(data))
		}
		var iter DataBlockIter
		iter.InitOnce(&testKeysSchema, testkeys.Comparer, nil, NoTieringColumns())
		if err := iter.Init(&d, bd, blockiter.Transforms{}, NoTieringColumns()); err != nil {
			t.Fatal(err)
		}
		if cap(iter.keyIter.Buf) > len(data)+1 {
			t.Fatalf("key buffer capacity %d exceeds block size %d", cap(iter.keyIter.Buf), len(data))
		}
		if kv := iter.First(); kv == nil || !bytes.Equal(kv.K.UserKey, short.UserKey) {
			t.Fatal("first key was not materialized from the block")
		}
		if iter.Next() != nil {
			t.Fatal("excluded key appeared in the block")
		}
		var validator DataBlockValidator
		if err := validator.Validate(data, testkeys.Comparer, &testKeysSchema); err != nil {
			t.Fatal(err)
		}
	}
}

func assertDecodePanics(t *testing.T, decode func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("malformed columnar block accepted")
		}
	}()
	decode()
}

// Fuzz all four column encodings together. The seed is writer-generated; the
// fuzzer changes framing, counts, offsets, summaries and payloads independently.
// Panics on malformed input are permitted, but instrumented runs can catch
// unsafe memory access even when a recovered assertion would be harmless.
func FuzzColumnarBlockDecode(f *testing.F) {
	seed := buildBlock([]testColumnSpec{
		{DataType: DataTypeBool},
		{DataType: DataTypeUint},
		{DataType: DataTypeBytes},
		{DataType: DataTypePrefixBytes, BundleSize: 2},
	}, 3, []interface{}{
		[]bool{true, false, true},
		[]uint64{1, 2, 3},
		[][]byte{[]byte("one"), []byte("two"), []byte("three")},
		[][]byte{[]byte("a"), []byte("ab"), []byte("ac")},
	})
	f.Add(seed)
	// Exercise zero-width integers, empty byte slices and the zero-bitmap
	// encoding in addition to the nonempty mixed-column seed.
	f.Add(buildBlock([]testColumnSpec{{DataType: DataTypeBool}}, 3,
		[]interface{}{[]bool{false, false, false}}))
	f.Add(buildBlock([]testColumnSpec{{DataType: DataTypeUint}}, 3,
		[]interface{}{[]uint64{0, 0, 0}}))
	f.Add(buildBlock([]testColumnSpec{{DataType: DataTypeBytes}}, 3,
		[]interface{}{[][]byte{nil, nil, nil}}))
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(error); !ok {
					t.Fatalf("unexpected non-error panic: %v", r)
				}
			}
		}()
		d := DecodeBlock(data, 0)
		if d.Rows() > 32 || d.header.Columns > 4 {
			return
		}
		for col := range int(d.header.Columns) {
			switch d.DataType(col) {
			case DataTypeBool:
				b := d.Bitmap(col)
				for row := range int(d.Rows()) {
					b.At(row)
					b.SeekSetBitGE(row)
				}
			case DataTypeUint:
				b := d.Uints(col)
				for row := range int(d.Rows()) {
					b.At(row)
				}
			case DataTypeBytes:
				b := d.RawBytes(col)
				for row := range int(d.Rows()) {
					b.At(row)
				}
			case DataTypePrefixBytes:
				b := d.PrefixBytes(col)
				for row := range int(d.Rows()) {
					b.At(row)
				}
			default:
				return
			}
		}
	})
}

// FuzzDataBlockDecode additionally enters through the production metadata
// initializer, then materializes keys through the iterator. This covers the
// data-block custom header and key-schema-specific subformats.
func FuzzDataBlockDecode(f *testing.F) {
	var w DataBlockEncoder
	w.Init(&testKeysSchema, NoTieringColumns())
	for _, key := range []string{"a#2,SET", "ab#1,SET", "ac#1,SET"} {
		k := base.ParseInternalKey(key)
		cmp := w.KeyWriter.ComparePrev(k.UserKey)
		w.Add(k, []byte("v"), block.InPlaceValuePrefix(cmp.PrefixEqual()), cmp, false, base.KVMeta{})
	}
	seed, _ := w.Finish(w.Rows(), w.Size())
	f.Add(seed)
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(error); !ok {
					t.Fatalf("unexpected non-error panic: %v", r)
				}
			}
		}()
		var md block.Metadata
		if err := InitDataBlockMetadata(&testKeysSchema, &md, data); err != nil {
			return
		}
		var decoder DataBlockDecoder
		bd := decoder.Init(&testKeysSchema, data)
		if bd.Rows() > 32 {
			return
		}
		var iter DataBlockIter
		iter.InitOnce(&testKeysSchema, testkeys.Comparer, nil, NoTieringColumns())
		if err := iter.Init(&decoder, bd, blockiter.Transforms{}, NoTieringColumns()); err != nil {
			t.Fatal(err)
		}
		for kv, row := iter.First(), 0; kv != nil && row < 32; kv, row = iter.Next(), row+1 {
			_ = kv.K.UserKey
		}
	})
}

// FuzzSpecializedColumnarBlocks covers index, keyspan, key-value and
// reference-liveness blocks, whose headers and row counts differ.
func FuzzSpecializedColumnarBlocks(f *testing.F) {
	var indexWriter IndexBlockWriter
	indexWriter.Init()
	indexWriter.AddBlockHandle([]byte("a"), block.Handle{Offset: 8, Length: 32}, nil)
	indexWriter.AddBlockHandle([]byte("b"), block.Handle{Offset: 40, Length: 32}, nil)
	f.Add(uint8(0), indexWriter.Finish(indexWriter.Rows()))

	var spanWriter KeyspanBlockWriter
	spanWriter.Init(bytes.Equal)
	spanWriter.AddSpan(keyspan.ParseSpan("a-c:{(#100,RANGEDEL)}"))
	f.Add(uint8(1), spanWriter.Finish())

	var kvWriter KeyValueBlockWriter
	kvWriter.Init()
	kvWriter.AddKV([]byte("key"), []byte("value"))
	f.Add(uint8(2), kvWriter.Finish(kvWriter.Rows()))

	var liveWriter ReferenceLivenessBlockEncoder
	liveWriter.Init()
	liveWriter.AddReferenceLiveness(0, []byte{1, 2})
	f.Add(uint8(3), liveWriter.Finish())
	f.Fuzz(func(t *testing.T, format uint8, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(error); !ok {
					t.Fatalf("unexpected non-error panic: %v", r)
				}
			}
		}()
		var md block.Metadata
		switch format % 4 {
		case 0:
			if InitIndexBlockMetadata(&md, data) != nil {
				return
			}
			var d IndexBlockDecoder
			d.Init(data)
			if d.bd.Rows() > 32 {
				return
			}
			for row := range int(d.bd.Rows()) {
				d.separators.At(row)
				d.offsets.At(row)
				d.lengths.At(row)
				d.blockProps.At(row)
			}
		case 1:
			if InitKeyspanBlockMetadata(&md, data) != nil {
				return
			}
			var d KeyspanDecoder
			d.Init(data)
			if d.boundaryKeysCount > 32 || d.blockDecoder.Rows() > 32 {
				return
			}
			var iter keyspanIter
			iter.init(base.DefaultComparer.Compare, &d, blockiter.FragmentTransforms{})
			span, err := iter.First()
			for n := 0; span != nil && err == nil && n < 32; n++ {
				_, _ = span.Start, span.End
				span, err = iter.Next()
			}
		case 2:
			var d KeyValueBlockDecoder
			d.Init(data)
			if d.bd.Rows() > 32 {
				return
			}
			for row := range int(d.bd.Rows()) {
				d.KeyAt(row)
				d.ValueAt(row)
			}
		case 3:
			var d ReferenceLivenessBlockDecoder
			d.Init(data)
			if d.bd.Rows() > 32 {
				return
			}
			for row := range int(d.bd.Rows()) {
				d.LivenessAtReference(row)
			}
		}
	})
}
