// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"bytes"
	"encoding/binary"
	"math"
	"strconv"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/blockiter"
	"github.com/stretchr/testify/require"
)

// syntheticOfLen returns a SyntheticPrefixAndSuffix with a prefix of prefixLen
// bytes and a suffix of suffixLen bytes.
func syntheticOfLen(prefixLen, suffixLen int) blockiter.SyntheticPrefixAndSuffix {
	return blockiter.MakeSyntheticPrefixAndSuffix(
		bytes.Repeat([]byte{'p'}, prefixLen), bytes.Repeat([]byte{'s'}, suffixLen))
}

// TestMaterializedKeyCapacity exercises the arithmetic that sizes a
// DataBlockIter's key materialization buffer: the synthetic prefix, the block's
// recorded maximum key length and the synthetic suffix must be summed without
// wrapping the uint32 operands, and a sum that cannot be addressed must be
// reported rather than narrowed into a negative or insufficient int.
func TestMaterializedKeyCapacity(t *testing.T) {
	const limit32 = math.MaxInt32 - 1 // The limit on a 32-bit platform.

	type testCase struct {
		name      string
		prefixLen int
		suffixLen int
		maxKeyLen uint32
		limit     uint64
		want      uint64
		wantErr   bool
	}
	cases := []testCase{
		{name: "empty", limit: maxMaterializedKeyLength},
		{name: "no-transforms", maxKeyLen: 37, limit: maxMaterializedKeyLength, want: 37},
		{name: "prefix-and-suffix", prefixLen: 4, suffixLen: 9, maxKeyLen: 37,
			limit: maxMaterializedKeyLength, want: 50},
		// The uint32 sum of these operands wraps around to 12.
		{name: "uint32-wrap-32bit-limit", prefixLen: 4, suffixLen: 9, maxKeyLen: math.MaxUint32,
			limit: limit32, wantErr: true},
		// The uint32 sum fits but its int conversion is negative on a 32-bit
		// platform.
		{name: "int32-narrowing", prefixLen: 4, suffixLen: 9, maxKeyLen: math.MaxInt32,
			limit: limit32, wantErr: true},
		{name: "exactly-at-32bit-limit", prefixLen: 4, suffixLen: 9, maxKeyLen: limit32 - 13,
			limit: limit32, want: limit32},
		{name: "one-past-32bit-limit", prefixLen: 4, suffixLen: 9, maxKeyLen: limit32 - 12,
			limit: limit32, wantErr: true},
	}
	// The platform limit: on 64-bit platforms no uint32 operands can exceed it,
	// and the exact sum must be returned; on 32-bit platforms the same operands
	// must be rejected.
	cases = append(cases, testCase{
		name: "uint32-wrap-platform-limit", prefixLen: 4, suffixLen: 9, maxKeyLen: math.MaxUint32,
		limit: maxMaterializedKeyLength, want: math.MaxUint32 + 13, wantErr: strconv.IntSize == 32,
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := syntheticOfLen(tc.prefixLen, tc.suffixLen)
			n, err := materializedKeyCapacity(ps, tc.maxKeyLen, tc.limit)
			if tc.wantErr {
				require.Error(t, err)
				require.True(t, base.IsCorruptionError(err), "%v", err)
				return
			}
			require.NoError(t, err)
			require.GreaterOrEqual(t, n, 0)
			require.Equal(t, tc.want, uint64(n))
			// PrefixBytesIter.Init allocates n+1 bytes; that must not overflow.
			require.Greater(t, n+1, n)
		})
	}
}

// buildCapacityTestBlock returns a serialized data block containing keys.
func buildCapacityTestBlock(t *testing.T, keys []string) []byte {
	var w DataBlockEncoder
	w.Init(&testKeysSchema, NoTieringColumns())
	for _, k := range keys {
		ik := base.MakeInternalKey([]byte(k), 1, base.InternalKeyKindSet)
		kcmp := w.KeyWriter.ComparePrev(ik.UserKey)
		w.Add(ik, []byte(k), block.InPlaceValuePrefix(kcmp.PrefixEqual()), kcmp, false /* isObsolete */, base.KVMeta{})
	}
	blk, _ := w.Finish(w.Rows(), w.Size())
	return blk
}

// withMaximumKeyLength returns a copy of the serialized data block with its
// recorded maximum key length replaced.
func withMaximumKeyLength(blk []byte, maxKeyLen uint32) []byte {
	blk = bytes.Clone(blk)
	binary.LittleEndian.PutUint32(blk[testKeysSchema.HeaderSize:], maxKeyLen)
	return blk
}

// dataBlockIterInitializer initializes a DataBlockIter from a serialized
// block through one of its two initialization paths.
type dataBlockIterInitializer struct {
	name string
	// init initializes it over blk and returns a cleanup function.
	init func(t *testing.T, it *DataBlockIter, blk []byte, transforms blockiter.Transforms) (func(), error)
}

func dataBlockIterInitializers() []dataBlockIterInitializer {
	return []dataBlockIterInitializer{
		{
			name: "Init",
			init: func(t *testing.T, it *DataBlockIter, blk []byte, transforms blockiter.Transforms) (func(), error) {
				d := new(DataBlockDecoder)
				bd := d.Init(&testKeysSchema, blk)
				err := it.Init(d, bd, transforms, NoTieringColumns())
				return func() { _ = it.Close() }, err
			},
		},
		{
			name: "InitHandle",
			init: func(t *testing.T, it *DataBlockIter, blk []byte, transforms blockiter.Transforms) (func(), error) {
				c := cache.New(1 << 20)
				ch := c.NewHandle()
				v := block.Alloc(len(blk), nil)
				copy(v.BlockData(), blk)
				require.NoError(t, InitDataBlockMetadata(&testKeysSchema, v.BlockMetadata(), v.BlockData()))
				v.SetInCacheForTesting(ch, base.DiskFileNum(1), 0)
				cv := ch.Get(base.DiskFileNum(1), 0, base.MakeLevel(0), cache.CategorySSTableData)
				require.NotNil(t, cv)
				err := it.InitHandle(testkeys.Comparer, block.CacheBufferHandle(cv), transforms)
				return func() {
					_ = it.Close()
					ch.Close()
					c.Unref()
				}, err
			},
		},
	}
}

func newCapacityTestIter() *DataBlockIter {
	it := new(DataBlockIter)
	it.InitOnce(&testKeysSchema, testkeys.Comparer,
		getInternalValuer(func([]byte) base.InternalValue {
			return base.MakeInPlaceValue([]byte("mock external value"))
		}), NoTieringColumns())
	return it
}

// TestDataBlockIterMaterializedKeyCapacity verifies that both DataBlockIter
// initializers size the key materialization buffer for the synthetic prefix,
// the block's longest key and the synthetic suffix, and that the materialized
// keys are those the writer wrote with the transforms applied.
func TestDataBlockIterMaterializedKeyCapacity(t *testing.T) {
	keys := []string{"a@3", "bbbbbbbbbbbbbbbbbbbbbbbb@2", "c@1"}
	blk := buildCapacityTestBlock(t, keys)
	prefix := []byte("synthetic_prefix_")
	suffix := []byte("@123456789")

	for _, ini := range dataBlockIterInitializers() {
		t.Run(ini.name, func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				transforms blockiter.Transforms
			}{
				{name: "none"},
				{name: "prefix", transforms: blockiter.Transforms{
					SyntheticPrefixAndSuffix: blockiter.MakeSyntheticPrefixAndSuffix(prefix, nil)}},
				{name: "suffix", transforms: blockiter.Transforms{
					SyntheticPrefixAndSuffix: blockiter.MakeSyntheticPrefixAndSuffix(nil, suffix)}},
				{name: "prefix-and-suffix", transforms: blockiter.Transforms{
					SyntheticPrefixAndSuffix: blockiter.MakeSyntheticPrefixAndSuffix(prefix, suffix)}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					it := newCapacityTestIter()
					cleanup, err := ini.init(t, it, blk, tc.transforms)
					defer cleanup()
					require.NoError(t, err)

					ps := tc.transforms.SyntheticPrefixAndSuffix
					maxKeyLen := 0
					for _, k := range keys {
						maxKeyLen = max(maxKeyLen, len(k))
					}
					wantCap := int(ps.PrefixLen()) + maxKeyLen + int(ps.SuffixLen()) + 1
					require.GreaterOrEqual(t, cap(it.keyIter.Buf), wantCap)

					i := 0
					for kv := it.First(); kv != nil; kv = it.Next() {
						k := []byte(keys[i])
						want := append([]byte(nil), ps.Prefix()...)
						if ps.HasSuffix() {
							want = append(want, k[:testkeys.Comparer.Split(k)]...)
							want = append(want, ps.Suffix()...)
						} else {
							want = append(want, k...)
						}
						require.Equal(t, string(want), string(kv.K.UserKey))
						i++
					}
					require.Equal(t, len(keys), i)
				})
			}
		})
	}
}

// TestDataBlockIterMaterializedKeyCapacityOverflow verifies that both
// DataBlockIter initializers reject a recorded maximum key length that, with
// the synthetic prefix and suffix, wraps the uint32 sum and cannot be addressed
// by an int, instead of sizing the buffer for the wrapped or narrowed value.
//
// The same operands are addressable on 64-bit platforms, where
// TestMaterializedKeyCapacity verifies that the exact sum is computed; the
// buffer would be several gigabytes, so this test only runs on 32-bit
// platforms.
func TestDataBlockIterMaterializedKeyCapacityOverflow(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("requires a 32-bit platform")
	}
	// With a 4-byte prefix and a 10-byte suffix, the uint32 sum wraps to 11.
	blk := withMaximumKeyLength(buildCapacityTestBlock(t, []string{"a@1", "b@2"}), math.MaxUint32-2)
	transforms := blockiter.Transforms{
		SyntheticPrefixAndSuffix: blockiter.MakeSyntheticPrefixAndSuffix([]byte("pfx_"), []byte("@123456789")),
	}
	for _, ini := range dataBlockIterInitializers() {
		t.Run(ini.name, func(t *testing.T) {
			it := newCapacityTestIter()
			cleanup, err := ini.init(t, it, blk, transforms)
			defer cleanup()
			require.Error(t, err)
			require.True(t, base.IsCorruptionError(err), "%v", err)
			require.True(t, it.IsDataInvalidated())
		})
	}
}
