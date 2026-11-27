// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/stretchr/testify/require"
)

// TestColumnarDataBlockNonmonotoneValueOffsets writes a columnar sstable,
// replaces an intermediate offset of its data block's value column with a
// smaller one (recomputing the block checksum, as a writer bug or corruption
// before checksumming would), and tests that reading the block reports a
// corruption error rather than exposing values with inverted bounds.
func TestColumnarDataBlockNonmonotoneValueOffsets(t *testing.T) {
	const n = 20
	const valLen = 8
	obj := &objstorage.MemObj{}
	w := NewWriter(obj, WriterOptions{
		Comparer:    testkeys.Comparer,
		KeySchema:   &testkeysSchema,
		TableFormat: TableFormatMax,
		Compression: block.NoCompression,
		Checksum:    block.ChecksumTypeCRC32c,
	})
	for i := range n {
		require.NoError(t, w.Set(fmt.Appendf(nil, "key%02d", i), fmt.Appendf(nil, "value-%02d", i)))
	}
	require.NoError(t, w.Close())
	sst := obj.Data()
	ropts := ReaderOptions{
		Comparer:   testkeys.Comparer,
		KeySchemas: KeySchemas{testkeysSchema.Name: &testkeysSchema},
	}

	read := func(sst []byte) (*base.InternalKV, error) {
		r, err := NewMemReader(sst, ropts)
		require.NoError(t, err)
		defer func() { require.NoError(t, r.Close()) }()
		it, err := r.NewIter(NoTransforms, nil /* lower */, nil /* upper */, AssertNoBlobHandles)
		require.NoError(t, err)
		kv := it.First()
		if kv != nil {
			kv = &base.InternalKV{K: kv.K.Clone()}
		}
		err = it.Error()
		_ = it.Close()
		return kv, err
	}
	kv, err := read(sst)
	require.NoError(t, err)
	require.Equal(t, "key00", string(kv.K.UserKey))

	r, err := NewMemReader(sst, ropts)
	require.NoError(t, err)
	l, err := r.Layout()
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Len(t, l.Data, 1)
	bh := l.Data[0].Handle

	corrupt := slices.Clone(sst)
	require.Equal(t, byte(block.NoCompressionIndicator), corrupt[bh.Offset+bh.Length])
	blk := corrupt[bh.Offset : bh.Offset+bh.Length]
	// In-place values are stored without a prefix, so the value column's data
	// is the concatenated values, immediately preceded by n+1 one-byte offsets.
	dataOff := bytes.Index(blk, []byte("value-00value-01"))
	require.Greater(t, dataOff, n)
	table := blk[dataOff-(n+1) : dataOff]
	for i := range n + 1 {
		require.Equal(t, byte(i*valLen), table[i])
	}
	table[n/2] = table[n/2-1] - 1
	var cs block.Checksummer
	cs.Init(block.ChecksumTypeCRC32c)
	binary.LittleEndian.PutUint32(corrupt[bh.Offset+bh.Length+1:],
		cs.Checksum(blk, byte(block.NoCompressionIndicator)))

	kv, err = read(corrupt)
	require.Nil(t, kv)
	require.Error(t, err)
	require.True(t, base.IsCorruptionError(err), "expected corruption error; got %v", err)
	require.Contains(t, err.Error(), "error initializing data block metadata")
	require.Contains(t, err.Error(), "RawBytes offset")
}
