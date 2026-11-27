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

	"github.com/cockroachdb/crlib/crbytes"
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
	// init initializes it over blk. The returned release function frees
	// resources backing the block; it must run after the iterator is closed.
	init func(t *testing.T, it *DataBlockIter, blk []byte, transforms blockiter.Transforms) (release func(), _ error)
}

func dataBlockIterInitializers() []dataBlockIterInitializer {
	return []dataBlockIterInitializer{
		{
			name: "Init",
			init: func(t *testing.T, it *DataBlockIter, blk []byte, transforms blockiter.Transforms) (func(), error) {
				d := new(DataBlockDecoder)
				bd := d.Init(&testKeysSchema, blk)
				return func() {}, it.Init(d, bd, transforms, NoTieringColumns())
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
				release := func() {
					ch.Close()
					c.Unref()
				}
				return release, it.InitHandle(testkeys.Comparer, block.CacheBufferHandle(cv), transforms)
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
					release, err := ini.init(t, it, blk, tc.transforms)
					defer release()
					defer func() { _ = it.Close() }()
					require.NoError(t, err)

					ps := tc.transforms.SyntheticPrefixAndSuffix
					maxKeyLen := 0
					for _, k := range keys {
						maxKeyLen = max(maxKeyLen, len(k))
					}
					wantCap := int(ps.PrefixLen()) + maxKeyLen + int(ps.SuffixLen()) + 1
					require.GreaterOrEqual(t, cap(it.keyIter.Buf), wantCap)
					requireTransformedKeys(t, it, keys, ps)
				})
			}
		})
	}
}

// requireTransformedKeys iterates it and requires that it yields keys with the
// synthetic prefix and suffix applied.
func requireTransformedKeys(
	t *testing.T, it *DataBlockIter, keys []string, ps blockiter.SyntheticPrefixAndSuffix,
) {
	t.Helper()
	i := 0
	for kv := it.First(); kv != nil; kv = it.Next() {
		require.Less(t, i, len(keys))
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
}

// TestDataBlockIterMaterializedKeyCapacityOverflow verifies that both
// DataBlockIter initializers reject a recorded maximum key length that, with
// the synthetic prefix and suffix, wraps the uint32 sum and cannot be addressed
// by an int, instead of sizing the buffer for the wrapped or narrowed value.
// The iterator is reinitialized from a valid block, then the corrupt block,
// then a valid block again, and is then closed, to exercise reuse after a
// failed initialization.
//
// The same operands are addressable on 64-bit platforms, where
// TestMaterializedKeyCapacity verifies that the exact sum is computed; the
// buffer would be several gigabytes, so this test only runs on 32-bit
// platforms.
func TestDataBlockIterMaterializedKeyCapacityOverflow(t *testing.T) {
	if strconv.IntSize != 32 {
		t.Skip("requires a 32-bit platform")
	}
	keys := []string{"a@1", "b@2"}
	valid := buildCapacityTestBlock(t, keys)
	// With a 4-byte prefix and a 10-byte suffix, the uint32 sum wraps to 11.
	corrupt := withMaximumKeyLength(valid, math.MaxUint32-2)
	transforms := blockiter.Transforms{
		SyntheticPrefixAndSuffix: blockiter.MakeSyntheticPrefixAndSuffix([]byte("pfx_"), []byte("@123456789")),
	}
	ps := transforms.SyntheticPrefixAndSuffix
	for _, ini := range dataBlockIterInitializers() {
		t.Run(ini.name, func(t *testing.T) {
			it := newCapacityTestIter()
			var releases []func()
			defer func() {
				_ = it.Close()
				for _, release := range releases {
					release()
				}
			}()

			release, err := ini.init(t, it, valid, transforms)
			releases = append(releases, release)
			require.NoError(t, err)
			requireTransformedKeys(t, it, keys, ps)

			release, err = ini.init(t, it, corrupt, transforms)
			releases = append(releases, release)
			require.Error(t, err)
			require.True(t, base.IsCorruptionError(err), "%v", err)
			require.True(t, it.IsDataInvalidated())
			require.False(t, it.Valid())
			require.Nil(t, it.First())

			release, err = ini.init(t, it, valid, transforms)
			releases = append(releases, release)
			require.NoError(t, err)
			requireTransformedKeys(t, it, keys, ps)
		})
	}
}

// buildCapacityTestPrefixBytes returns a PrefixBytes holding keys in bundles of
// two.
func buildCapacityTestPrefixBytes(keys []string) PrefixBytes {
	var pbb PrefixBytesBuilder
	pbb.Init(2)
	for i, k := range keys {
		shared := 0
		if i > 0 {
			shared = crbytes.CommonPrefix([]byte(keys[i-1]), []byte(k))
		}
		pbb.Put([]byte(k), shared)
	}
	buf := make([]byte, pbb.Size(len(keys), 0)+1 /* +1 to avoid checkptr issues */)
	pbb.Finish(0, len(keys), 0, buf)
	pb, _ := DecodePrefixBytes(buf, 0, uint32(len(keys)))
	return pb
}

// TestPrefixBytesIterLengthWidth verifies that SetAt and both SetNext paths
// compute the materialized key length without wrapping, and check it against
// the buffer's capacity before copying any bytes.
//
// A synthetic prefix and key whose lengths sum past math.MaxUint32 would need
// gigabytes of memory, so the oversized components are simulated by setting
// the iterator's length fields directly. Computed in uint32, those lengths wrap
// to a small value that fits the buffer while the copies land gigabytes past it.
func TestPrefixBytesIterLengthWidth(t *testing.T) {
	keys := []string{"apple1", "apple2", "banana1", "banana2"}
	pb := buildCapacityTestPrefixBytes(keys)
	const capacity = 32

	newIter := func(syntheticPrefix string) (*PrefixBytesIter, []byte) {
		backing := bytes.Repeat([]byte{0xA5}, capacity+32)
		it := &PrefixBytesIter{Buf: backing[:0:capacity]}
		it.Init(capacity-1, blockiter.SyntheticPrefix(syntheticPrefix))
		require.Equal(t, capacity, cap(it.Buf), "Init must reuse the restricted buffer")
		return it, backing
	}
	requireCanaryIntact := func(t *testing.T, backing []byte) {
		t.Helper()
		for i := capacity; i < len(backing); i++ {
			require.Equal(t, byte(0xA5), backing[i], "byte %d beyond the capacity was overwritten", i)
		}
	}
	panics := func(fn func()) (panicked bool) {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		fn()
		return false
	}

	t.Run("ordinary", func(t *testing.T) {
		it, backing := newIter("pfx_")
		pb.SetAt(it, 0)
		require.Equal(t, "pfx_apple1", string(it.Buf))
		for i := 1; i < len(keys); i++ {
			pb.SetNext(it)
			require.Equal(t, "pfx_"+keys[i], string(it.Buf))
		}
		pb.SetAt(it, 2)
		require.Equal(t, "pfx_banana1", string(it.Buf))
		requireCanaryIntact(t, backing)
	})
	t.Run("SetAt", func(t *testing.T) {
		it, backing := newIter("")
		it.syntheticPrefixLen = math.MaxUint32 - 2
		require.True(t, panics(func() { pb.SetAt(it, 0) }))
		requireCanaryIntact(t, backing)
	})
	t.Run("SetNext/same-bundle", func(t *testing.T) {
		it, backing := newIter("")
		pb.SetAt(it, 0)
		it.sharedAndBundlePrefixLen = math.MaxUint32
		require.True(t, panics(func() { pb.SetNext(it) }))
		requireCanaryIntact(t, backing)
	})
	t.Run("SetNext/new-bundle", func(t *testing.T) {
		it, backing := newIter("")
		pb.SetAt(it, 1)
		it.syntheticPrefixLen = math.MaxUint32 - 2
		require.True(t, panics(func() { pb.SetNext(it) }))
		requireCanaryIntact(t, backing)
	})
}
