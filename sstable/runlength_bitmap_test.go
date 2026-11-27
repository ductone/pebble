// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/pebble/internal/binfmt"
	"github.com/cockroachdb/pebble/internal/testutils"
	"github.com/stretchr/testify/require"
)

func TestRunLengthBitmap(t *testing.T) {
	var enc BitmapRunLengthEncoder
	var encodedBuf []byte
	var roundtripBuf []byte
	var buf bytes.Buffer
	datadriven.RunTest(t, "testdata/runlength_bitmap", func(t *testing.T, td *datadriven.TestData) string {
		buf.Reset()
		switch td.Cmd {
		case "encode":
			enc.Init()
			var i int
			for _, r := range td.Input {
				if r == '1' {
					enc.Set(i)
					i++
				} else if r == '0' {
					i++
				}
			}
			fmt.Fprintf(&buf, "Size: %d\n", enc.Size())
			encodedBuf = enc.FinishAndAppend(encodedBuf[:0])
			fmt.Fprintln(&buf, "Binary encoding:")
			binfmt.FHexDump(&buf, encodedBuf, 20, false)

			if err := validateRunLengthBitmap(encodedBuf); err != nil {
				t.Fatalf("writer-produced bitmap failed validation: %v", err)
			}
			// Validate that decoding the bitmap and re-encoding it yields the
			// same encoding.
			enc.Init()
			for i := range IterSetBitsInRunLengthBitmap(encodedBuf) {
				enc.Set(i)
			}
			roundtripBuf = enc.FinishAndAppend(roundtripBuf[:0])
			if !bytes.Equal(encodedBuf, roundtripBuf) {
				t.Fatalf("encodedBuf != roundtripBuf")
			}
			return buf.String()
		default:
			return fmt.Sprintf("unknown command: %s", td.Cmd)
		}
	})
}

func TestRunLengthBitmap_Randomized(t *testing.T) {
	prng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))

	for range 20 {
		n := testutils.RandIntInRange(prng, 1, 100000)
		enc := BitmapRunLengthEncoder{}
		enc.Init()
		// Generate a random bitmap with a random density.
		// A cutoff is chosen from an exponential distribution.
		// Then randomly it's decided whether values less than the cutoff should
		// be set, or values greater than the cutoff should be set.
		// Then random values are chosen and compared to the cutoff.
		cutoff := prng.ExpFloat64()
		ltCutoff := prng.IntN(2) == 1

		for i := 0; i < n; i++ {
			if prng.Float64() < cutoff {
				if ltCutoff {
					enc.Set(i)
				}
			} else if !ltCutoff {
				enc.Set(i)
			}
		}

		// Ensure that the encoded bitmap can be decoded and re-encoded,
		// yielding exactly the same encoding.
		encoded := enc.FinishAndAppend(nil)
		if err := validateRunLengthBitmap(encoded); err != nil {
			t.Fatalf("writer-produced bitmap %x failed validation: %v", encoded, err)
		}
		enc = BitmapRunLengthEncoder{}
		enc.Init()
		for i := range IterSetBitsInRunLengthBitmap(encoded) {
			enc.Set(i)
		}
		reencoded := enc.FinishAndAppend(nil)
		if !bytes.Equal(encoded, reencoded) {
			t.Fatalf("encoded %x != reencoded %x", encoded, reencoded)
		}
	}
}

// TestRunLengthBitmapLargestIndex checks that bitmaps holding the largest bit
// index the encoder accepts (an int that is also a uint32) validate and
// round-trip, and that bitmaps describing more bits are rejected by validation
// and stop the iterator. None of this work is proportional to the index.
func TestRunLengthBitmapLargestIndex(t *testing.T) {
	const maxIndex = int(min(uint64(math.MaxInt), uint64(math.MaxUint32)))
	require.Equal(t, uint64(maxIndex)+1, maxRunLengthBitmapBits)

	collect := func(bitmap []byte) []int {
		var got []int
		for i := range IterSetBitsInRunLengthBitmap(bitmap) {
			got = append(got, i)
			require.LessOrEqual(t, len(got), 16)
		}
		return got
	}
	for _, indices := range [][]int{
		{maxIndex},
		{maxIndex - 1},
		{0, maxIndex},
		// The last byte is all set, so it is encoded as a run of set bits.
		{maxIndex - 7, maxIndex - 6, maxIndex - 5, maxIndex - 4, maxIndex - 3, maxIndex - 2, maxIndex - 1, maxIndex},
	} {
		var enc BitmapRunLengthEncoder
		enc.Init()
		for _, i := range indices {
			enc.Set(i)
		}
		encoded := enc.FinishAndAppend(nil)
		require.NoError(t, validateRunLengthBitmap(encoded), "%x", encoded)
		require.Equal(t, indices, collect(encoded))

		// Appending one more byte describes too many bits.
		tooLong := append(encoded, 0x01)
		require.Error(t, validateRunLengthBitmap(tooLong))
		require.Equal(t, indices, collect(tooLong))
	}

	// Unvalidated runs claiming more than maxRunLengthBitmapBits stop the
	// iterator before it yields anything from them.
	for _, bitmap := range [][]byte{
		binary.AppendUvarint([]byte{0xFF}, math.MaxUint64/8),
		binary.AppendUvarint([]byte{0xFF}, maxRunLengthBitmapBits/8+1),
		binary.AppendUvarint([]byte{0x01, 0x00}, maxRunLengthBitmapBits/8),
	} {
		require.Error(t, validateRunLengthBitmap(bitmap))
		got := collect(bitmap)
		if bitmap[0] == 0x01 {
			require.Equal(t, []int{0}, got)
		} else {
			require.Empty(t, got)
		}
	}
}
