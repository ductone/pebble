// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"encoding/binary"
	"iter"
	"maps"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/testutils"
	"github.com/cockroachdb/pebble/sstable/blob"
	"github.com/cockroachdb/pebble/sstable/colblk"
	"github.com/stretchr/testify/require"
)

// TestBlobRefValueLivenessWriter tests functions around the
// blobRefValueLivenessWriter.
func TestBlobRefValueLivenessWriter(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		w := &blobRefValueLivenessWriter{}
		w.init()
		refID := base.BlobReferenceID(0)
		blockID := blob.BlockID(0)

		require.NoError(t, w.addLiveValue(refID, blockID, 0 /* valueID */, 24 /* valueSize */))
		require.NoError(t, w.addLiveValue(refID, blockID, 1 /* valueID */, 10 /* valueSize */))
		require.NoError(t, w.addLiveValue(refID, blockID, 2 /* valueID */, 14 /* valueSize */))
		// Add a gap between valueIDs so that we get some dead values.
		require.NoError(t, w.addLiveValue(refID, blockID, 5 /* valueID */, 12 /* valueSize */))

		// Add values to a different block.
		blockID++
		require.NoError(t, w.addLiveValue(refID, blockID, 2 /* valueID */, 20 /* valueSize */))

		encodings := maps.Collect(w.finish())
		require.Len(t, encodings, 1)
		blocks, err := DecodeBlobRefLivenessEncoding(encodings[0])
		require.NoError(t, err)
		require.Len(t, blocks, 2)

		// Verify first block (refID=0, blockID=0).
		require.Equal(t, blob.BlockID(0), blocks[0].BlockID)
		require.Equal(t, 60, blocks[0].ValuesSize)
		// We only have 1 byte worth of value liveness encoding.
		require.Equal(t, 1, blocks[0].BitmapSize)
		// Verify bitmap: 111001 (27 in hex).
		require.Equal(t, uint8(0x27), blocks[0].Bitmap[0])

		// Verify second block (refID=0, blockID=1).
		require.Equal(t, blob.BlockID(1), blocks[1].BlockID)
		require.Equal(t, 20, blocks[1].ValuesSize)
		// We only have 1 byte worth of value liveness encoding.
		require.Equal(t, 1, blocks[1].BitmapSize)
		// Verify bitmap: 001 (4 in hex).
		require.Equal(t, uint8(0x4), blocks[1].Bitmap[0])
	})

	t.Run("all-ones", func(t *testing.T) {
		w := &blobRefValueLivenessWriter{}
		w.init()
		refID := base.BlobReferenceID(0)
		blockID := blob.BlockID(0)

		// Add only live values.
		require.NoError(t, w.addLiveValue(refID, blockID, 0 /* valueID */, 100 /* valueSize */))
		require.NoError(t, w.addLiveValue(refID, blockID, 1 /* valueID */, 200 /* valueSize */))
		require.NoError(t, w.addLiveValue(refID, blockID, 2 /* valueID */, 300 /* valueSize */))

		encodings := maps.Collect(w.finish())
		require.Len(t, encodings, 1)
		blocks, err := DecodeBlobRefLivenessEncoding(encodings[0])
		require.NoError(t, err)
		require.Len(t, blocks, 1)

		require.Equal(t, blob.BlockID(0), blocks[0].BlockID)
		require.Equal(t, 600, blocks[0].ValuesSize)
		// We only have 1 byte worth of value liveness encoding.
		require.Equal(t, 1, blocks[0].BitmapSize)
		// Verify bitmap: 111 (7 in hex).
		require.Equal(t, uint8(0x7), blocks[0].Bitmap[0])
	})
}

func TestBlobRefLivenessEncoding_Randomized(t *testing.T) {
	const valueSize = uint64(10)
	prng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))
	w := blobRefValueLivenessWriter{}

	collectSlice := func(i iter.Seq2[base.BlobReferenceID, []byte]) [][]byte {
		var s [][]byte
		for _, enc := range i {
			s = append(s, enc)
		}
		return s
	}

	for range 20 {
		w.init()
		numRefs := testutils.RandIntInRange(prng, 1, 10)
		for refID := range numRefs {
			numBlocks := testutils.RandIntInRange(prng, 1, 6)
			currentBlockID := blob.BlockID(testutils.RandIntInRange(prng, 0, 4))

			for range numBlocks {
				// Generate blockIDs that are increasing -- with occasional
				// duplicates. Allow a 70% chance to repeat the previous block
				// ID.
				if prng.Float64() < 0.3 {
					currentBlockID += blob.BlockID(testutils.RandIntInRange(prng, 1, 4))
				}
				numValues := testutils.RandIntInRange(prng, 10, 101)
				currentValueID := blob.BlockValueID(testutils.RandIntInRange(prng, 0, 4))
				for range numValues {
					require.NoError(t, w.addLiveValue(
						base.BlobReferenceID(refID),
						currentBlockID,
						currentValueID,
						valueSize,
					))
					currentValueID += blob.BlockValueID(testutils.RandIntInRange(prng, 1, 4))
				}
			}
		}
		encoded := collectSlice(w.finish())

		// Test the encoding/decoding roundtrip to ensure idempotence.
		// Reinitialize the writer before reconstructing values.
		w.init()
		for refID, blockEnc := range encoded {
			blocks, err := DecodeBlobRefLivenessEncoding(blockEnc)
			if err != nil {
				t.Fatalf("cannot decode blob reference liveness encoding for refID %d: %v", refID, err)
			}
			for _, block := range blocks {
				// Reconstruct the live values from the bitmap and add them to
				// the writer.
				for valueID := range IterSetBitsInRunLengthBitmap(block.Bitmap) {
					require.NoError(t, w.addLiveValue(
						base.BlobReferenceID(refID),
						block.BlockID,
						blob.BlockValueID(valueID),
						valueSize,
					))
				}
			}
		}

		reencoded := collectSlice(w.finish())
		require.Equal(t, encoded, reencoded)
	}
}

// TestDecodeBlobRefLivenessEncodingMalformed checks that malformed liveness
// encodings are rejected with corruption errors instead of panicking, looping
// or describing more bits than a blob block can hold.
func TestDecodeBlobRefLivenessEncodingMalformed(t *testing.T) {
	uv := func(vs ...uint64) []byte {
		var b []byte
		for _, v := range vs {
			b = binary.AppendUvarint(b, v)
		}
		return b
	}
	// encoding builds <block ID> <values size> <len(bitmap)> <bitmap>.
	encoding := func(blockID, valuesSize uint64, bitmap []byte) []byte {
		return append(uv(blockID, valuesSize, uint64(len(bitmap))), bitmap...)
	}
	// overflowRun is a zero run whose varint length overflows: preceded by ten
	// literal bytes, the unchecked iterator stepped back to offset 0 and looped
	// forever.
	overflowRun := append(bytes.Repeat([]byte{0x01}, 10), 0x00)
	overflowRun = append(overflowRun, bytes.Repeat([]byte{0x80}, 10)...)
	overflowRun = append(overflowRun, 0x01)
	// tooManyBits describes one byte more than maxRunLengthBitmapBits allows.
	tooManyBits := append(append([]byte{0x00}, uv(maxRunLengthBitmapBits/8)...), 0x01)

	testCases := []struct {
		name string
		buf  []byte
	}{
		{name: "truncated-block-id", buf: []byte{0x80}},
		{name: "truncated-values-size", buf: append(uv(0), 0x80)},
		{name: "truncated-bitmap-size", buf: append(uv(0, 1), 0x80)},
		{name: "block-id-overflow", buf: encoding(math.MaxUint32+1, 1, []byte{0x01})},
		{name: "values-size-overflow", buf: encoding(0, uint64(math.MaxInt)+1, []byte{0x01})},
		{name: "bitmap-beyond-buffer", buf: append(uv(0, 1, 2), 0x01)},
		{name: "bitmap-size-overflow", buf: append(uv(0, 1, math.MaxUint64), 0x01)},
		{name: "bitmap-truncated-run", buf: encoding(0, 1, []byte{0x01, 0xFF})},
		{name: "bitmap-overflowing-run", buf: encoding(0, 1, overflowRun)},
		{name: "bitmap-too-many-bits", buf: encoding(0, 1, tooManyBits)},
		{name: "second-encoding-malformed", buf: append(encoding(0, 1, []byte{0x01}), encoding(1, 1, []byte{0x00})...)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encs, err := DecodeBlobRefLivenessEncoding(tc.buf)
			require.Error(t, err)
			require.True(t, base.IsCorruptionError(err), "%+v", err)
			require.Nil(t, encs)
		})
	}

	t.Run("iterator-stops-at-malformed-run", func(t *testing.T) {
		// The iterator must terminate on an unvalidated bitmap; the literal
		// bytes before the bad run yield bits 0, 8, ..., 72.
		var bits []int
		for i := range IterSetBitsInRunLengthBitmap(overflowRun) {
			bits = append(bits, i)
			require.Less(t, len(bits), 100)
		}
		require.Equal(t, []int{0, 8, 16, 24, 32, 40, 48, 56, 64, 72}, bits)
	})

	t.Run("maximum-zero-run", func(t *testing.T) {
		// A zero run covering every representable bit followed by nothing is
		// accepted, and neither validation nor iteration is proportional to
		// the run length.
		encs, err := DecodeBlobRefLivenessEncoding(
			encoding(math.MaxUint32, 1, append([]byte{0x00}, uv(maxRunLengthBitmapBits/8)...)))
		require.NoError(t, err)
		require.Len(t, encs, 1)
		require.Equal(t, blob.BlockID(math.MaxUint32), encs[0].BlockID)
		for range IterSetBitsInRunLengthBitmap(encs[0].Bitmap) {
			t.Fatal("unexpected set bit")
		}
	})

	t.Run("empty-bitmap", func(t *testing.T) {
		encs, err := DecodeBlobRefLivenessEncoding(encoding(3, 7, nil))
		require.NoError(t, err)
		require.Equal(t, []BlobRefLivenessEncoding{{BlockID: 3, ValuesSize: 7, Bitmap: []byte{}}}, encs)
	})
}

// TestBlobRefLivenessBlockRoundTrip decodes a liveness block built the way the
// table writer builds it: blobRefValueLivenessWriter encodings stored in a
// colblk.ReferenceLivenessBlockEncoder.
func TestBlobRefLivenessBlockRoundTrip(t *testing.T) {
	var w blobRefValueLivenessWriter
	w.init()
	type live struct {
		blockID blob.BlockID
		valueID blob.BlockValueID
	}
	want := make([][]live, 3)
	for refID := range want {
		for blockID := blob.BlockID(refID); blockID < blob.BlockID(refID+3); blockID++ {
			for valueID := blob.BlockValueID(0); valueID < 200; valueID += blob.BlockValueID(1 + int(valueID/50)*refID) {
				require.NoError(t, w.addLiveValue(base.BlobReferenceID(refID), blockID, valueID, 10))
				want[refID] = append(want[refID], live{blockID, valueID})
			}
		}
	}
	var enc colblk.ReferenceLivenessBlockEncoder
	enc.Init()
	for refID, buf := range w.finish() {
		enc.AddReferenceLiveness(int(refID), buf)
	}

	var d colblk.ReferenceLivenessBlockDecoder
	require.NoError(t, d.InitChecked(enc.Finish()))
	require.Equal(t, len(want), d.BlockDecoder().Rows())
	for refID := range want {
		encs, err := DecodeBlobRefLivenessEncoding(d.LivenessAtReference(refID))
		require.NoError(t, err)
		var got []live
		for _, e := range encs {
			require.Equal(t, len(e.Bitmap), e.BitmapSize)
			for valueID := range IterSetBitsInRunLengthBitmap(e.Bitmap) {
				got = append(got, live{e.BlockID, blob.BlockValueID(valueID)})
			}
		}
		require.Equal(t, want[refID], got)
	}
}
