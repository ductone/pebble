// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"math"
	"unsafe"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/binfmt"
	"github.com/cockroachdb/pebble/internal/treeprinter"
	"github.com/cockroachdb/pebble/sstable/block"
)

const (
	valueLivenessBlockCustomHeaderSize = 0
	valueLivenessBlockColumnCount      = 1
	valueLivenessBlockColumnIndex      = 0
)

// ReferenceLivenessBlockEncoder encodes a reference liveness block.
// A reference liveness block is a columnar block that contains a single column:
// an array of bytes encoding the liveness of values within a sstable's blob
// references. The indexes into this array are the base.BlobReferenceIDs contained
// within the sstable.
type ReferenceLivenessBlockEncoder struct {
	values RawBytesBuilder
	enc    BlockEncoder
}

// Init initializes the reference liveness block encoder.
func (e *ReferenceLivenessBlockEncoder) Init() {
	e.values.Init()
}

// Reset resets the reference liveness block encoder.
func (e *ReferenceLivenessBlockEncoder) Reset() {
	e.values.Reset()
	e.enc.Reset()
}

// AddReferenceLiveness adds a value to the reference liveness block.
func (e *ReferenceLivenessBlockEncoder) AddReferenceLiveness(referenceID int, v []byte) {
	if e.values.Rows() != referenceID {
		panic(base.AssertionFailedf("referenceID %d does not match number of rows %d", referenceID, e.values.Rows()))
	}
	e.values.Put(v)
}

// Count returns the number of values in the reference liveness block.
func (e *ReferenceLivenessBlockEncoder) Count() int {
	return e.values.Rows()
}

func (e *ReferenceLivenessBlockEncoder) size() int {
	rows := e.values.Rows()
	if rows == 0 {
		return 0
	}
	off := HeaderSize(valueLivenessBlockColumnCount, valueLivenessBlockCustomHeaderSize)
	off = e.values.Size(rows, off)
	// Add a padding byte at the end to allow the block's end to be represented
	// as a pointer to allocated memory.
	off++
	return int(off)
}

// Finish serializes the pending reference liveness block.
func (e *ReferenceLivenessBlockEncoder) Finish() []byte {
	e.enc.Init(e.size(), Header{
		Version: Version1,
		Columns: valueLivenessBlockColumnCount,
		Rows:    uint32(e.values.Rows()),
	}, valueLivenessBlockCustomHeaderSize)
	e.enc.Encode(e.values.Rows(), &e.values)
	return e.enc.Finish()
}

// ReferenceLivenessBlockDecoder reads columnar reference liveness blocks.
type ReferenceLivenessBlockDecoder struct {
	values RawBytes
	bd     BlockDecoder
}

// Init initializes the decoder with the given serialized reference liveness
// block. It panics with the corruption error returned by InitChecked if the
// block is malformed; callers decoding blocks read from disk should use
// InitChecked.
func (d *ReferenceLivenessBlockDecoder) Init(data []byte) {
	if err := d.InitChecked(data); err != nil {
		panic(err)
	}
}

// InitChecked initializes the decoder with the given serialized reference
// liveness block. It returns a corruption error, and leaves the decoder empty,
// if the block is too small for its header, has an unexpected version, column
// count or row count, or fails DecodeRawBytes' checks of the column.
//
// BlockDecoder reads the block header and the column's header through unchecked
// pointers, so InitChecked bounds those bytes itself before decoding. The column
// is then decoded by DecodeRawBytes, which reads the encoding, the offsets table
// and the data section only through bounds-checked slices. It asserts that the
// table and data lie within the block and that the offsets are nondecreasing,
// so every row LivenessAtReference returns lies within data. InitChecked
// converts those assertion panics, and DecodeColumn's end-offset assertion,
// into corruption errors.
func (d *ReferenceLivenessBlockDecoder) InitChecked(data []byte) (err error) {
	*d = ReferenceLivenessBlockDecoder{}
	// Convert assertion panics from the column decoders to corruption errors
	// so that callers reading untrusted tables (e.g. during ingestion) can
	// report them.
	defer func() {
		if r := recover(); r != nil {
			*d = ReferenceLivenessBlockDecoder{}
			err = base.CorruptionErrorf("pebble: error initializing reference liveness block: %v", r)
		}
	}()
	// The block must hold the header, the single column's header and the
	// trailing padding byte before any of them are read.
	minSize := uint64(HeaderSize(valueLivenessBlockColumnCount, valueLivenessBlockCustomHeaderSize)) + 1
	if uint64(len(data)) < minSize {
		return base.CorruptionErrorf("pebble: reference liveness block is %d bytes; expected at least %d",
			len(data), minSize)
	}
	d.bd.Init(data, valueLivenessBlockCustomHeaderSize)
	if h := d.bd.header; h.Version != Version1 || h.Columns != valueLivenessBlockColumnCount ||
		uint64(h.Rows) > math.MaxInt {
		*d = ReferenceLivenessBlockDecoder{}
		return base.CorruptionErrorf("pebble: reference liveness block has version %d, %d columns and %d rows; "+
			"expected version %d and %d column", h.Version, h.Columns, h.Rows, Version1, valueLivenessBlockColumnCount)
	}
	d.values = d.bd.RawBytes(valueLivenessBlockColumnIndex)
	return nil
}

// DebugString prints a human-readable explanation of the reference liveness
// block's binary representation.
func (d *ReferenceLivenessBlockDecoder) DebugString() string {
	f := binfmt.New(d.bd.Data()).LineWidth(20)
	tp := treeprinter.New()
	d.Describe(f, tp.Child("reference-liveness-block-decoder"))
	return tp.String()
}

// Describe describes the binary format of the reference liveness block, assuming
// f.Offset() is positioned at the beginning of the same reference liveness
// block described by d.
func (d *ReferenceLivenessBlockDecoder) Describe(f *binfmt.Formatter, tp treeprinter.Node) {
	// Set the relative offset. When loaded into memory, the beginning of blocks
	// are aligned. Padding that ensures alignment is done relative to the
	// current offset. Setting the relative offset ensures that if we're
	// describing this block within a larger structure (eg, f.Offset()>0), we
	// compute padding appropriately assuming the current byte f.Offset() is
	// aligned.
	f.SetAnchorOffset()

	n := tp.Child("reference liveness block header")
	d.bd.HeaderToBinFormatter(f, n)
	d.bd.ColumnToBinFormatter(f, n, 0, d.bd.Rows())
	f.HexBytesln(1, "block padding byte")
	f.ToTreePrinter(n)
}

// BlockDecoder returns the block decoder for the reference liveness block.
func (d *ReferenceLivenessBlockDecoder) BlockDecoder() *BlockDecoder {
	return &d.bd
}

// LivenessAtReference returns the liveness of the reference (the given index).
// The index must be in [0, BlockDecoder().Rows()); callers that take the index
// from table metadata must check it first.
func (d *ReferenceLivenessBlockDecoder) LivenessAtReference(i int) []byte {
	return d.values.At(i)
}

// Assert that an ReferenceLivenessBlockDecoder can fit inside block.Metadata.
const _ uint = block.MetadataSize - uint(unsafe.Sizeof(ReferenceLivenessBlockDecoder{}))
