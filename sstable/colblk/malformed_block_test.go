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

	"github.com/cockroachdb/pebble/v2/internal/base"
	"github.com/cockroachdb/pebble/v2/internal/keyspan"
	"github.com/cockroachdb/pebble/v2/internal/testkeys"
	"github.com/cockroachdb/pebble/v2/sstable/block"
)

func expectCorruptBlock(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("corrupt block was accepted")
		}
	}()
	fn()
}

func TestMalformedColumnarBlockBounds(t *testing.T) {
	t.Run("page offset", func(t *testing.T) {
		var w RawBytesBuilder
		w.Reset()
		w.Put([]byte("a"))
		b := FinishBlock(1, []ColumnWriter{&w})
		binary.LittleEndian.PutUint32(b[blockHeaderBaseSize+1:], uint32(len(b)+1))
		expectCorruptBlock(t, func() { DecodeBlock(b, 0) })
	})
	t.Run("intermediate bytes offset", func(t *testing.T) {
		var w RawBytesBuilder
		w.Reset()
		for _, s := range []string{"a", "b", "c"} {
			w.Put([]byte(s))
		}
		b := FinishBlock(3, []ColumnWriter{&w})
		d := DecodeBlock(b, 0)
		offsets, _ := DecodeUnsafeOffsets(b, d.pageStart(0), 4)
		if offsets.width != 1 {
			t.Fatalf("expected one-byte offsets, got %d", offsets.width)
		}
		pos := int(uintptr(offsets.ptr) - uintptr(unsafe.Pointer(unsafe.SliceData(b))))
		b[pos+2] = 100 // Larger than the unchanged final offset.
		expectCorruptBlock(t, func() { d.RawBytes(0) })
	})
	t.Run("bitmap summary", func(t *testing.T) {
		var w BitmapBuilder
		w.Reset()
		w.Set(0)
		b := FinishBlock(3, []ColumnWriter{&w})
		d := DecodeBlock(b, 0)
		wordStart := align(d.pageStart(0)+1, uint32(align64))
		b[wordStart+8] = 0x80 // Points past the last bitmap data word.
		expectCorruptBlock(t, func() { d.Bitmap(0) })
	})
	t.Run("truncated integer", func(t *testing.T) {
		var w UintBuilder
		w.Init()
		for i, v := range []uint64{1, 2, 3} {
			w.Set(i, v)
		}
		b := FinishBlock(3, []ColumnWriter{&w})
		binary.LittleEndian.PutUint32(b[3:], 100)
		expectCorruptBlock(t, func() { d := DecodeBlock(b, 0); d.Uints(0) })
	})
	t.Run("prefix bytes offset", func(t *testing.T) {
		var w PrefixBytesBuilder
		w.Init(2)
		for i, s := range []string{"a", "ab", "ac"} {
			shared := 0
			if i > 0 {
				shared = 1
			}
			w.Put([]byte(s), shared)
		}
		b := FinishBlock(3, []ColumnWriter{&w})
		d := DecodeBlock(b, 0)
		offsets, _ := DecodeUnsafeOffsets(b, d.pageStart(0)+1, 6)
		if offsets.width != 1 {
			t.Fatalf("expected one-byte offsets, got %d", offsets.width)
		}
		pos := int(uintptr(offsets.ptr) - uintptr(unsafe.Pointer(unsafe.SliceData(b))))
		b[pos+2] = 100
		expectCorruptBlock(t, func() { d.PrefixBytes(0) })
	})
}

func TestMalformedKeyspanBoundaryIndex(t *testing.T) {
	var w KeyspanBlockWriter
	w.Init(bytes.Equal)
	w.AddSpan(keyspan.ParseSpan("a-c:{(#100,RANGEDEL)}"))
	b := w.Finish()
	var d KeyspanDecoder
	d.Init(b)
	if d.boundaryKeyIndices.width != 1 {
		t.Fatalf("expected one-byte boundary indices, got %d", d.boundaryKeyIndices.width)
	}
	pos := int(uintptr(d.boundaryKeyIndices.ptr) - uintptr(unsafe.Pointer(unsafe.SliceData(b))))
	b[pos+1] = 100
	expectCorruptBlock(t, func() { d.Init(b) })
}

func TestMalformedDataBlockKeyLength(t *testing.T) {
	var w DataBlockEncoder
	w.Init(&testKeysSchema)
	k := base.ParseInternalKey("a#1,SET")
	cmp := w.KeyWriter.ComparePrev(k.UserKey)
	w.Add(k, []byte("v"), block.InPlaceValuePrefix(cmp.PrefixEqual()), cmp, false)
	b, _ := w.Finish(w.Rows(), w.Size())
	binary.LittleEndian.PutUint32(b[testKeysSchema.HeaderSize:], math.MaxUint32)
	var md block.Metadata
	if err := InitDataBlockMetadata(&testKeysSchema, &md, b); err == nil {
		t.Fatal("corrupt key length was accepted before iterator allocation")
	}
}

// FuzzColumnarBlockDecode exercises the generic column encodings and the
// production metadata paths for data, index and keyspan blocks.
func FuzzColumnarBlockDecode(f *testing.F) {
	mixed := buildBlock([]testColumnSpec{
		{DataType: DataTypeBool}, {DataType: DataTypeUint},
		{DataType: DataTypeBytes}, {DataType: DataTypePrefixBytes, BundleSize: 2},
	}, 3, []interface{}{
		[]bool{true, false, true}, []uint64{1, 2, 3},
		[][]byte{[]byte("one"), []byte("two"), []byte("three")},
		[][]byte{[]byte("a"), []byte("ab"), []byte("ac")},
	})
	f.Add(uint8(0), mixed)
	f.Add(uint8(0), buildBlock([]testColumnSpec{{DataType: DataTypeBool}}, 3,
		[]interface{}{[]bool{false, false, false}}))
	f.Add(uint8(0), buildBlock([]testColumnSpec{{DataType: DataTypeUint}}, 3,
		[]interface{}{[]uint64{0, 0, 0}}))

	var w DataBlockEncoder
	w.Init(&testKeysSchema)
	for _, key := range []string{"a#2,SET", "ab#1,SET", "ac#1,SET"} {
		k := base.ParseInternalKey(key)
		cmp := w.KeyWriter.ComparePrev(k.UserKey)
		w.Add(k, []byte("v"), block.InPlaceValuePrefix(cmp.PrefixEqual()), cmp, false)
	}
	b, _ := w.Finish(w.Rows(), w.Size())
	f.Add(uint8(1), b)

	var iw IndexBlockWriter
	iw.Init()
	iw.AddBlockHandle([]byte("a"), block.Handle{Offset: 8, Length: 32}, nil)
	f.Add(uint8(2), iw.Finish(iw.Rows()))

	var kw KeyspanBlockWriter
	kw.Init(bytes.Equal)
	kw.AddSpan(keyspan.ParseSpan("a-c:{(#100,RANGEDEL)}"))
	f.Add(uint8(3), kw.Finish())

	f.Fuzz(func(t *testing.T, format uint8, data []byte) {
		if len(data) > 64<<10 {
			return
		}
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(error); !ok {
					t.Fatalf("unexpected panic: %v", r)
				}
			}
		}()
		switch format % 4 {
		case 0:
			d := DecodeBlock(data, 0)
			if d.Rows() > 32 || d.header.Columns > 4 {
				return
			}
			for col := range int(d.header.Columns) {
				for row := range d.Rows() {
					switch d.DataType(col) {
					case DataTypeBool:
						d.Bitmap(col).SeekSetBitGE(row)
					case DataTypeUint:
						d.Uints(col).At(row)
					case DataTypeBytes:
						d.RawBytes(col).At(row)
					case DataTypePrefixBytes:
						d.PrefixBytes(col).At(row)
					}
				}
			}
		case 1:
			var md block.Metadata
			if InitDataBlockMetadata(&testKeysSchema, &md, data) != nil {
				return
			}
			var d DataBlockDecoder
			d.Init(&testKeysSchema, data)
			if d.d.Rows() > 32 {
				return
			}
			var it DataBlockIter
			it.InitOnce(&testKeysSchema, testkeys.Comparer, nil)
			if err := it.Init(&d, block.IterTransforms{}); err != nil {
				t.Fatal(err)
			}
			for kv, row := it.First(), 0; kv != nil && row < 32; kv, row = it.Next(), row+1 {
				_ = kv.K.UserKey
			}
		case 2:
			var md block.Metadata
			if InitIndexBlockMetadata(&md, data) != nil {
				return
			}
			var d IndexBlockDecoder
			d.Init(data)
			if d.bd.Rows() > 32 {
				return
			}
			for row := range d.bd.Rows() {
				d.separators.At(row)
				d.offsets.At(row)
				d.lengths.At(row)
				d.blockProps.At(row)
			}
		case 3:
			var md block.Metadata
			if InitKeyspanBlockMetadata(&md, data) != nil {
				return
			}
			var d KeyspanDecoder
			d.Init(data)
			if d.boundaryKeysCount > 32 || d.blockDecoder.Rows() > 32 {
				return
			}
			var it keyspanIter
			it.init(base.DefaultComparer.Compare, &d, block.FragmentIterTransforms{})
			span, err := it.First()
			for row := 0; span != nil && err == nil && row < 32; row++ {
				_, _ = span.Start, span.End
				span, err = it.Next()
			}
		}
	})
}
