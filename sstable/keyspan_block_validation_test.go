// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/stretchr/testify/require"
)

// TestReaderKeyspanBlockBoundaryIndices rewrites the last boundary key index of
// a table's keyspan block past the block's key rows, updating the block
// checksum. Iterating the block would read rows beyond its key columns; the
// reader must instead report corruption when it loads the block.
func TestReaderKeyspanBlockBoundaryIndices(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		span    string
		handle  func(*Layout) block.Handle
		newIter func(*Reader) (keyspan.FragmentIterator, error)
	}{
		{
			span:   "a-c:{(#9,RANGEDEL) (#3,RANGEDEL)}",
			handle: func(l *Layout) block.Handle { return l.RangeDel },
			newIter: func(r *Reader) (keyspan.FragmentIterator, error) {
				return r.NewRawRangeDelIter(ctx, NoFragmentTransforms, NoReadEnv)
			},
		},
		{
			span:   "a-c:{(#9,RANGEKEYSET,@5,v1) (#8,RANGEKEYUNSET,@3)}",
			handle: func(l *Layout) block.Handle { return l.RangeKey },
			newIter: func(r *Reader) (keyspan.FragmentIterator, error) {
				return r.NewRawRangeKeyIter(ctx, NoFragmentTransforms, NoReadEnv)
			},
		},
	} {
		obj := &objstorage.MemObj{}
		w := NewRawWriter(obj, WriterOptions{
			TableFormat: TableFormatMax,
			Compression: block.NoCompression,
			Checksum:    block.ChecksumTypeCRC32c,
		})
		require.NoError(t, w.EncodeSpan(keyspan.ParseSpan(tc.span)))
		require.NoError(t, w.Close())

		open := func(file []byte) *Reader {
			obj := &objstorage.MemObj{}
			require.NoError(t, obj.Write(slices.Clone(file)))
			r, err := NewReader(ctx, obj, ReaderOptions{})
			require.NoError(t, err)
			return r
		}
		file := slices.Clone(obj.Data())
		r := open(file)
		l, err := r.Layout()
		require.NoError(t, err)
		bh := tc.handle(l)
		iter, err := tc.newIter(r)
		require.NoError(t, err)
		s, err := iter.First()
		require.NoError(t, err)
		require.Equal(t, tc.span, s.String())
		iter.Close()
		require.NoError(t, r.Close())

		// The block has boundary keys a and c, with indices 0 and 2 in its
		// second column, encoded in one byte each after the encoding byte.
		blk := file[bh.Offset : bh.Offset+bh.Length]
		off := binary.LittleEndian.Uint32(blk[4+7+5+1:])
		require.Equal(t, []byte{1, 0, 2}, blk[off:off+3])
		blk[off+2] = 200
		trailer := file[bh.Offset+bh.Length:]
		require.Equal(t, byte(block.NoCompressionIndicator), trailer[0])
		c := block.Checksummer{Type: block.ChecksumTypeCRC32c}
		binary.LittleEndian.PutUint32(trailer[1:], c.Checksum(blk, trailer[0]))

		r = open(file)
		iter, err = tc.newIter(r)
		require.Nil(t, iter)
		require.True(t, base.IsCorruptionError(err), "%+v", err)
		require.ErrorContains(t, err,
			"error initializing keyspan block metadata: keyspan block boundary key indices span [0, 200]; expected [0, 2]")
		require.NoError(t, r.Close())
	}
}
