// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/blockiter"
	"github.com/stretchr/testify/require"
)

func writeKeyspanTestBlock(spans ...keyspan.Span) []byte {
	var w KeyspanBlockWriter
	w.Init(base.DefaultComparer.Equal)
	for _, s := range spans {
		w.AddSpan(s)
	}
	return slices.Clone(w.Finish())
}

func parseKeyspanTestSpans(spans ...string) []keyspan.Span {
	parsed := make([]keyspan.Span, len(spans))
	for i, s := range spans {
		parsed[i] = keyspan.ParseSpan(s)
	}
	return parsed
}

// initKeyspanTestBlock initializes a block's metadata as the sstable reader
// does when loading a keyspan block into the cache.
func initKeyspanTestBlock(data []byte) (block.Value, error) {
	v := block.Alloc(len(data), nil)
	copy(v.BlockData(), data)
	if err := InitKeyspanBlockMetadata(v.BlockMetadata(), v.BlockData()); err != nil {
		v.Release()
		return block.Value{}, err
	}
	return v, nil
}

// requireKeyspanBlockRejected checks that KeyspanDecoder.Init panics with a
// corruption error and metadata initialization returns one.
func requireKeyspanBlockRejected(t *testing.T, data []byte, msg string) {
	t.Helper()
	func() {
		defer func() {
			err, ok := recover().(error)
			require.True(t, ok, "KeyspanDecoder.Init did not panic with an error")
			require.True(t, base.IsCorruptionError(err), "%+v", err)
			require.ErrorContains(t, err, msg)
		}()
		var d KeyspanDecoder
		d.Init(data)
	}()
	_, err := initKeyspanTestBlock(data)
	require.True(t, base.IsCorruptionError(err), "%+v", err)
	require.ErrorContains(t, err, "error initializing keyspan block metadata: "+msg)
}

// traceKeyspanIter describes the spans returned by full forward and reverse
// scans and by a seek in each direction to each probe key.
func traceKeyspanIter(it keyspan.FragmentIterator, probes []string) string {
	var b strings.Builder
	emit := func(op string, s *keyspan.Span, err error) bool {
		fmt.Fprintf(&b, "%s: %s %v\n", op, spanStr(s), err)
		return s != nil && err == nil
	}
	for s, err := it.First(); emit("First/Next", s, err); s, err = it.Next() {
	}
	for s, err := it.Last(); emit("Last/Prev", s, err); s, err = it.Prev() {
	}
	for _, p := range probes {
		s, err := it.SeekGE([]byte(p))
		emit("SeekGE "+p, s, err)
		s, err = it.SeekLT([]byte(p))
		emit("SeekLT "+p, s, err)
	}
	return b.String()
}

// TestKeyspanDecoderInitWriterBlocks checks that validation accepts blocks
// written by KeyspanBlockWriter, which then iterate as written through both a
// directly initialized decoder and a cached block.
func TestKeyspanDecoderInitWriterBlocks(t *testing.T) {
	cmp := base.DefaultComparer.Compare
	probes := []string{"", "a", "b", "c", "d", "e", "f", "g", "h", "k", "z"}
	for _, spans := range [][]keyspan.Span{
		// Abutting multi-key spans, then a gap (an empty span) before g.
		parseKeyspanTestSpans(
			"a-c:{(#9,RANGEDEL) (#3,RANGEDEL)}",
			"c-e:{(#4,RANGEDEL)}",
			"g-k:{(#8,RANGEDEL) (#7,RANGEDEL)}",
		),
		parseKeyspanTestSpans(
			"a-c:{(#9,RANGEKEYSET,@5,v1) (#8,RANGEKEYUNSET,@3)}",
			"c-d:{(#7,RANGEKEYDEL)}",
			"f-k:{(#6,RANGEKEYSET,@1,v2)}",
		),
	} {
		data := writeKeyspanTestBlock(spans...)
		want := traceKeyspanIter(keyspan.NewIter(cmp, spans), probes)

		var d KeyspanDecoder
		d.Init(data)
		var it keyspanIter
		it.init(cmp, &d, blockiter.NoFragmentTransforms)
		require.Equal(t, want, traceKeyspanIter(&it, probes))

		v, err := initKeyspanTestBlock(data)
		require.NoError(t, err)
		cached := NewKeyspanIter(cmp, v.MakeHandle(), blockiter.NoFragmentTransforms)
		require.Equal(t, want, traceKeyspanIter(cached, probes))
		cached.Close()
	}

	// An empty block, and a block holding a span without keys (the sstable
	// writer elides these, but KeyspanBlockWriter accepts them).
	for _, spans := range [][]keyspan.Span{nil, parseKeyspanTestSpans("a-b:{}")} {
		var d KeyspanDecoder
		d.Init(writeKeyspanTestBlock(spans...))
		v, err := initKeyspanTestBlock(writeKeyspanTestBlock(spans...))
		require.NoError(t, err)
		v.Release()
	}
}

// TestKeyspanDecoderInitBoundaryIndices rewrites the boundary key indices of a
// writer-produced block in place. The columns still decode, but the indices no
// longer describe rows of the key columns.
func TestKeyspanDecoderInitBoundaryIndices(t *testing.T) {
	// Boundaries a, c, e, g, k; the span [e, g) is a gap.
	data := writeKeyspanTestBlock(parseKeyspanTestSpans(
		"a-c:{(#9,RANGEDEL) (#3,RANGEDEL)}",
		"c-e:{(#4,RANGEDEL)}",
		"g-k:{(#8,RANGEDEL)}",
	)...)
	// The second column's page offset follows the keyspan header, the block
	// header and the first column's header.
	off := int(binary.LittleEndian.Uint32(data[keyspanHeaderSize+blockHeaderBaseSize+columnHeaderSize+1:]))
	require.Equal(t, byte(1), data[off], "expected a 1-byte encoding without a delta base")
	require.Equal(t, []byte{0, 2, 3, 3, 4}, data[off+1:off+6])

	for _, tc := range []struct {
		name    string
		indices []byte
		msg     string
	}{
		{"first-nonzero", []byte{1, 2, 3, 3, 4}, "keyspan block boundary key indices span [1, 4]; expected [0, 4]"},
		{"last-short", []byte{0, 2, 3, 3, 3}, "keyspan block boundary key indices span [0, 3]; expected [0, 4]"},
		{"last-past-rows", []byte{0, 2, 3, 3, 200}, "keyspan block boundary key indices span [0, 200]; expected [0, 4]"},
		{"descending", []byte{0, 2, 3, 1, 4}, "keyspan block boundary key index 3 decreases: 1 < 3"},
		{"interior-past-rows", []byte{0, 9, 3, 3, 4}, "keyspan block boundary key index 2 decreases: 3 < 9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := slices.Clone(data)
			copy(mutated[off+1:], tc.indices)
			requireKeyspanBlockRejected(t, mutated, tc.msg)
		})
	}
	t.Run("extra-column", func(t *testing.T) {
		mutated := slices.Clone(data)
		binary.LittleEndian.PutUint16(mutated[keyspanHeaderSize+1:], keyspanColumnCount+1)
		requireKeyspanBlockRejected(t, mutated, "keyspan block has 6 columns; expected 5")
	})
	t.Run("keys-without-boundaries", func(t *testing.T) {
		// Encode key rows without adding a span, so the boundary columns are
		// empty but every column is physically well formed.
		var w KeyspanBlockWriter
		w.Init(base.DefaultComparer.Equal)
		for i := range 2 {
			w.trailers.Set(i, uint64(base.MakeTrailer(base.SeqNum(9-i), base.InternalKeyKindRangeDelete)))
			w.suffixes.Put(nil)
			w.values.Put(nil)
			w.keyCount++
		}
		requireKeyspanBlockRejected(t, slices.Clone(w.Finish()), "keyspan block has 2 keys but no boundary keys")
	})
}

// TestKeyspanDecoderInitZeroWidthIndices checks a block whose boundary columns
// are zero-width constants, so its boundary count can be raised without
// encoding more bytes. Validation must not loop over that logical count, and
// rejects it only if it does not fit in an int.
func TestKeyspanDecoderInitZeroWidthIndices(t *testing.T) {
	data := writeKeyspanTestBlock(keyspan.Span{Start: []byte{}, End: []byte{}})
	var d KeyspanDecoder
	d.Init(data)
	require.Equal(t, uint8(0), d.boundaryKeyIndices.width)
	require.Equal(t, uint8(0), d.boundaryKeys.offsets.width)

	for _, n := range []uint32{math.MaxInt32, math.MaxInt32 + 1, math.MaxUint32 - 1} {
		mutated := slices.Clone(data)
		binary.LittleEndian.PutUint32(mutated, n)
		if uint64(n) > math.MaxInt {
			requireKeyspanBlockRejected(t, mutated, "keyspan block counts overflow int")
			continue
		}
		d.Init(mutated)
		require.Equal(t, n, d.boundaryKeysCount)
	}
}

func BenchmarkKeyspanDecoderInit(b *testing.B) {
	run := func(name string, data []byte) {
		b.Run(name, func(b *testing.B) {
			var d KeyspanDecoder
			b.SetBytes(int64(len(data)))
			for range b.N {
				d.Init(data)
			}
		})
	}
	for _, n := range []int{10, 1000} {
		// Two keys per span; every third span is followed by a gap.
		spans := make([]string, n)
		for i := range spans {
			end := 2*i + 2
			if i%3 == 2 {
				end--
			}
			spans[i] = fmt.Sprintf("k%05d-k%05d:{(#%d,RANGEDEL) (#%d,RANGEDEL)}", 2*i, end, 2*i+2, 2*i+1)
		}
		run(fmt.Sprintf("spans=%d", n), writeKeyspanTestBlock(parseKeyspanTestSpans(spans...)...))
	}
	zeroWidth := writeKeyspanTestBlock(keyspan.Span{Start: []byte{}, End: []byte{}})
	binary.LittleEndian.PutUint32(zeroWidth, math.MaxInt32)
	run("zero-width-boundaries=MaxInt32", zeroWidth)
}
