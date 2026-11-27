// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/blockiter"
	"github.com/stretchr/testify/require"
)

// TestDataBlockIterWithMeta tests that the *WithMeta methods correctly
// extract and return KVMeta for keys with tiering metadata.
func TestDataBlockIterWithMeta(t *testing.T) {
	schema := DefaultKeySchema(testkeys.Comparer, 16)
	comparer := testkeys.Comparer.EnsureDefaults()
	tieringColConfig := WithTieringColumns()

	var w DataBlockEncoder
	w.Init(&schema, tieringColConfig)
	testKVs := []struct {
		key  string
		meta base.KVMeta
	}{
		{"a#1,SET", base.KVMeta{TieringSpanID: 42, TieringAttribute: 100}},
		{"b#2,SET", base.KVMeta{TieringSpanID: 43, TieringAttribute: 200}},
		{"c#3,SET", base.KVMeta{TieringSpanID: 0, TieringAttribute: 0}},
	}
	for _, kv := range testKVs {
		ikey := base.ParseInternalKey(kv.key)
		kcmp := w.KeyWriter.ComparePrev(ikey.UserKey)
		w.Add(ikey, []byte("value"), block.InPlaceValuePrefix(kcmp.PrefixEqual()), kcmp, false, kv.meta)
	}
	block, _ := w.Finish(w.Rows(), w.Size())

	var d DataBlockDecoder
	bd := d.Init(&schema, block)
	var it DataBlockIter
	it.InitOnce(&schema, comparer, nil, tieringColConfig)
	require.NoError(t, it.Init(&d, bd, blockiter.Transforms{}, tieringColConfig))

	// Test FirstWithMeta and NextWithMeta.
	for i, expected := range testKVs {
		var kv *base.InternalKV
		var meta base.KVMeta
		if i == 0 {
			kv, meta = it.FirstWithMeta()
		} else {
			kv, meta = it.NextWithMeta()
		}
		require.NotNil(t, kv)
		require.Equal(t, expected.meta, meta)
	}

	// Test exhaustion.
	kv, meta := it.NextWithMeta()
	require.Nil(t, kv)
	require.Equal(t, base.KVMeta{}, meta)

	// Test SeekGEWithMeta.
	kv, meta = it.SeekGEWithMeta([]byte("b"), base.SeekGEFlagsNone)
	require.NotNil(t, kv)
	require.Equal(t, testKVs[1].meta, meta)

	kv, meta = it.SeekGEWithMeta([]byte("z"), base.SeekGEFlagsNone)
	require.Nil(t, kv)
	require.Equal(t, base.KVMeta{}, meta)
}

// TestDataBlockIterMalformedTieringColumn tests a tiering column decoded
// lazily by the iterator, after the block's metadata was initialized.
func TestDataBlockIterMalformedTieringColumn(t *testing.T) {
	schema := DefaultKeySchema(testkeys.Comparer, 16)
	cfg := WithTieringColumns()
	build := func() []byte {
		var w DataBlockEncoder
		w.Init(&schema, cfg)
		for i, k := range []string{"a#1,SET", "b#2,SET"} {
			ikey := base.ParseInternalKey(k)
			kcmp := w.KeyWriter.ComparePrev(ikey.UserKey)
			w.Add(ikey, []byte("v"), block.InPlaceValuePrefix(kcmp.PrefixEqual()), kcmp, false, base.KVMeta{
				TieringSpanID:    base.TieringSpanID(i + 1),
				TieringAttribute: base.TieringAttribute(100 * (i + 1)),
			})
		}
		data, _ := w.Finish(w.Rows(), w.Size())
		return data
	}
	// Widen the second tiering column, so that only it fails to decode.
	corrupt := build()
	bd := DecodeBlock(corrupt, DataBlockCustomHeaderSize+schema.HeaderSize)
	corrupt[bd.pageStart(len(schema.ColumnTypes)+dataBlockColumnTieringAttribute)] = byte(makeUintEncoding(8, false))

	var d DataBlockDecoder
	var it DataBlockIter
	it.InitOnce(&schema, testkeys.Comparer.EnsureDefaults(), nil, cfg)
	require.NoError(t, it.Init(&d, d.Init(&schema, corrupt), blockiter.Transforms{}, cfg))
	require.NotNil(t, it.First())
	// A failed decode must not leave a partially initialized pair of columns.
	for range 2 {
		requireAssertionPanic(t, func() { it.decodeMeta() })
	}

	require.NoError(t, it.Init(&d, d.Init(&schema, build()), blockiter.Transforms{}, cfg))
	kv, meta := it.FirstWithMeta()
	require.NotNil(t, kv)
	require.Equal(t, base.KVMeta{TieringSpanID: 1, TieringAttribute: 100}, meta)
}
