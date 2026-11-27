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
	"unsafe"

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

// capacityTestKeys are the keys encoded by buildCapacityTestPrefixBytes. In
// bundles of two they share no column-wide prefix and have the bundle prefixes
// "apple" and "banana".
var capacityTestKeys = []string{"apple1", "apple2", "banana1", "banana2"}

// buildCapacityTestPrefixBytes encodes capacityTestKeys in bundles of two with
// a PrefixBytesBuilder and returns the encoded column and its decoding.
func buildCapacityTestPrefixBytes() ([]byte, PrefixBytes) {
	var pbb PrefixBytesBuilder
	pbb.Init(2)
	for i, k := range capacityTestKeys {
		shared := 0
		if i > 0 {
			shared = crbytes.CommonPrefix([]byte(capacityTestKeys[i-1]), []byte(k))
		}
		pbb.Put([]byte(k), shared)
	}
	n := len(capacityTestKeys)
	buf := make([]byte, pbb.Size(n, 0)+1 /* +1 to avoid checkptr issues */)
	pbb.Finish(0, n, 0, buf)
	pb, _ := DecodePrefixBytes(buf, 0, uint32(n))
	return buf, pb
}

// canaryIterCapacity is the capacity of the buffer newCanaryPrefixBytesIter
// hands to a PrefixBytesIter; the backing array continues past it with canary
// bytes.
const canaryIterCapacity = 32

// newCanaryPrefixBytesIter returns a PrefixBytesIter whose buffer has
// canaryIterCapacity bytes of capacity, and the canary-filled backing array the
// buffer was carved from.
func newCanaryPrefixBytesIter(
	t *testing.T, syntheticPrefix string,
) (*PrefixBytesIter, []byte) {
	backing := bytes.Repeat([]byte{0xA5}, canaryIterCapacity+32)
	it := &PrefixBytesIter{Buf: backing[:0:canaryIterCapacity]}
	it.Init(canaryIterCapacity-1, blockiter.SyntheticPrefix(syntheticPrefix))
	require.Equal(t, canaryIterCapacity, cap(it.Buf), "Init must reuse the restricted buffer")
	return it, backing
}

func requireCanaryIntact(t *testing.T, backing []byte) {
	t.Helper()
	for i := canaryIterCapacity; i < len(backing); i++ {
		require.Equal(t, byte(0xA5), backing[i], "byte %d beyond the capacity was overwritten", i)
	}
}

func panics(fn func()) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	fn()
	return false
}

// TestPrefixBytesIterLengthWidth verifies that SetAt and both SetNext paths
// compute the materialized key length without wrapping, and check it against
// the buffer's capacity before copying any bytes.
//
// Here the oversized components are simulated by setting the iterator's length
// fields directly, as a valid synthetic prefix and key summing past
// math.MaxUint32 would need gigabytes of memory. A malformed block reaches the
// same wrap without large inputs; see TestPrefixBytesIterDecreasingOffset.
// Computed in uint32, these lengths wrap to a small value that fits the buffer
// while the copies land gigabytes past it.
func TestPrefixBytesIterLengthWidth(t *testing.T) {
	keys := capacityTestKeys
	_, pb := buildCapacityTestPrefixBytes()

	t.Run("ordinary", func(t *testing.T) {
		it, backing := newCanaryPrefixBytesIter(t, "pfx_")
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
		it, backing := newCanaryPrefixBytesIter(t, "")
		it.syntheticPrefixLen = math.MaxUint32 - 2
		require.True(t, panics(func() { pb.SetAt(it, 0) }))
		requireCanaryIntact(t, backing)
	})
	t.Run("SetNext/same-bundle", func(t *testing.T) {
		it, backing := newCanaryPrefixBytesIter(t, "")
		pb.SetAt(it, 0)
		it.sharedAndBundlePrefixLen = math.MaxUint32
		require.True(t, panics(func() { pb.SetNext(it) }))
		requireCanaryIntact(t, backing)
	})
	t.Run("SetNext/new-bundle", func(t *testing.T) {
		it, backing := newCanaryPrefixBytesIter(t, "")
		pb.SetAt(it, 1)
		it.syntheticPrefixLen = math.MaxUint32 - 2
		require.True(t, panics(func() { pb.SetNext(it) }))
		requireCanaryIntact(t, backing)
	})
}

// setEncodedOffset overwrites, in the encoded column bytes, the i'th offset of
// pb's RawBytes, and requires that the decoded accessor observes the new value.
func setEncodedOffset(t *testing.T, pb PrefixBytes, i int, v uint32) {
	t.Helper()
	o := pb.rawBytes.offsets
	p := unsafe.Add(o.ptr, uintptr(i)*uintptr(o.width))
	switch o.width {
	case 1:
		require.LessOrEqual(t, v, uint32(math.MaxUint8))
		*(*uint8)(p) = uint8(v)
	case 2:
		require.LessOrEqual(t, v, uint32(math.MaxUint16))
		*(*uint16)(p) = uint16(v)
	case 4:
		*(*uint32)(p) = v
	default:
		t.Fatalf("unexpected offset width %d", o.width)
	}
	require.Equal(t, v, o.At(i))
}

// TestPrefixBytesIterDecreasingOffset verifies the destination bounds of
// SetAt and both SetNext paths against a malformed but small column: a
// PrefixBytesBuilder-encoded column in which one row's encoded suffix end
// offset is decreased to one below its start offset. The row suffix length
// end-start then wraps to math.MaxUint32. Summed in uint32 with the
// shared and bundle prefix lengths, the materialized length wraps again to a
// small value that fits the buffer, and the suffix copy then reads and writes
// about 4 GiB; summed in uint64, slicing the buffer panics before any copy.
//
// This test exercises only the destination (Buf) bounds. The source reads
// from the column's data are bounded by the column's offsets, whose validation
// is separate. The test depends on DecodePrefixBytes accepting non-monotonic
// offsets: once RawBytes decoding rejects them, the expected outcome here
// becomes a decode-time rejection, and this test should be changed to assert
// that instead.
func TestPrefixBytesIterDecreasingOffset(t *testing.T) {
	for _, tc := range []struct {
		name string
		// row is the row whose suffix end offset is decreased.
		row int
		// materialize positions a canary-backed iterator at row through the
		// path under test.
		materialize func(pb PrefixBytes, it *PrefixBytesIter)
	}{
		{name: "SetAt", row: 0, materialize: func(pb PrefixBytes, it *PrefixBytesIter) {
			pb.SetAt(it, 0)
		}},
		{name: "SetNext/same-bundle", row: 1, materialize: func(pb PrefixBytes, it *PrefixBytesIter) {
			pb.SetAt(it, 0)
			pb.SetNext(it)
		}},
		{name: "SetNext/new-bundle", row: 2, materialize: func(pb PrefixBytes, it *PrefixBytesIter) {
			pb.SetAt(it, 1)
			pb.SetNext(it)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, pb := buildCapacityTestPrefixBytes()
			i := pb.rowSuffixIndex(tc.row)
			start, end := pb.rawBytes.offsets.At2(i)
			require.Less(t, start, end)
			require.Positive(t, start)
			setEncodedOffset(t, pb, i+1, start-1)

			// Decode the mutated column afresh.
			pb, _ = DecodePrefixBytes(buf, 0, uint32(len(capacityTestKeys)))
			start, end = pb.rawBytes.offsets.At2(i)
			require.Equal(t, uint32(math.MaxUint32), end-start)

			it, backing := newCanaryPrefixBytesIter(t, "")
			require.True(t, panics(func() { tc.materialize(pb, it) }))
			requireCanaryIntact(t, backing)
		})
	}
}
