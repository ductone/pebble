// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package cockroachkvs

import (
	"bytes"
	"fmt"
	"testing"
	"unsafe"

	"github.com/cockroachdb/crlib/testutils/leaktest"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/colblk"
	"github.com/stretchr/testify/require"
)

// canary fills the bytes of a materialization buffer's backing array beyond
// the capacity handed to the key seeker.
const canary = 0xA5

// singleKeySeeker returns a key seeker over a data block holding only key,
// along with the key as the key writer materializes it.
func singleKeySeeker(t *testing.T, key []byte) (colblk.KeySeeker, []byte) {
	var enc colblk.DataBlockEncoder
	enc.Init(&KeySchema, colblk.NoTieringColumns())
	kcmp := enc.KeyWriter.ComparePrev(key)
	ikey := base.InternalKey{
		UserKey: key,
		Trailer: pebble.MakeInternalKeyTrailer(0, base.InternalKeyKindSet),
	}
	enc.Add(ikey, key, block.InPlaceValuePrefix(false), kcmp, false /* isObsolete */, base.KVMeta{})
	writerKey := enc.KeyWriter.MaterializeKey(nil, 0)
	blk, _ := enc.Finish(1, enc.Size())

	var dec colblk.DataBlockDecoder
	bd := dec.Init(&KeySchema, blk)
	ksPointer := &cockroachKeySeeker{}
	KeySchema.InitKeySeekerMetadata((*colblk.KeySeekerMetadata)(unsafe.Pointer(ksPointer)), &dec, bd)
	return KeySchema.KeySeeker((*colblk.KeySeekerMetadata)(unsafe.Pointer(ksPointer))), writerKey
}

// restrictedIter returns a PrefixBytesIter whose buffer has the given capacity
// and the backing array it was carved from; the backing array extends past
// the capacity with canary bytes.
func restrictedIter(capacity int) (*colblk.PrefixBytesIter, []byte) {
	backing := bytes.Repeat([]byte{canary}, capacity+32)
	return &colblk.PrefixBytesIter{Buf: backing[:0:capacity]}, backing
}

func requireCanaryIntact(t *testing.T, backing []byte, capacity int) {
	t.Helper()
	for i := capacity; i < len(backing); i++ {
		if backing[i] != canary {
			t.Fatalf("byte %d beyond the %d-byte capacity was overwritten: %x",
				i, capacity, backing[capacity:])
		}
	}
}

func materializePanics(fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()
	fn()
	return false
}

// TestMaterializeUserKeyCapacity verifies that the cockroach key seeker
// materializes the writer's key into a buffer whose capacity fits it exactly,
// and that when the capacity is insufficient it checks the capacity before
// storing any byte beyond it. Untyped versions and synthetic suffixes are
// exercised alongside the MVCC wall-only and wall-and-logical encodings.
func TestMaterializeUserKeyCapacity(t *testing.T) {
	defer leaktest.AfterTest(t)()

	syntheticSuffix := NewTimestampSuffix(1_000_000_000_000, 7)
	for _, tc := range []struct {
		name    string
		key     string
		wantLen int // the materialized length beyond the roach key
	}{
		{name: "no-version", key: `"foo"`, wantLen: 1},
		{name: "untyped", key: `"foo" @ 0102030405060708090a0b0c0d0e0f10`, wantLen: 18},
		{name: "wall", key: `"foo" @ 3000000000,0`, wantLen: 10},
		{name: "wall-and-logical", key: `"foo" @ 3000000000,7`, wantLen: 14},
		{name: "logical-only", key: `"foo" @ 0,7`, wantLen: 14},
		{name: "max-wall-and-logical", key: `"foo" @ 18446744073709551615,4294967295`, wantLen: 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := parseUserKey(tc.key)
			ks, writerKey := singleKeySeeker(t, key)
			// Ordinary (non-synthetic) keys round-trip byte-for-byte through
			// the writer.
			require.Equal(t, key, writerKey)
			roachKeyLen := Split(key) - 1
			require.Equal(t, roachKeyLen+tc.wantLen, len(key))

			materializers := []struct {
				name string
				want []byte
				fn   func(ki *colblk.PrefixBytesIter) []byte
			}{
				{
					name: "MaterializeUserKey",
					want: key,
					fn: func(ki *colblk.PrefixBytesIter) []byte {
						return ks.MaterializeUserKey(ki, -1, 0)
					},
				},
				{
					name: "MaterializeUserKeyWithSyntheticSuffix",
					want: append(append(key[:roachKeyLen:roachKeyLen], 0), syntheticSuffix...),
					fn: func(ki *colblk.PrefixBytesIter) []byte {
						return ks.MaterializeUserKeyWithSyntheticSuffix(ki, syntheticSuffix, -1, 0)
					},
				},
			}
			for _, m := range materializers {
				t.Run(m.name, func(t *testing.T) {
					// Exact fit.
					ki, backing := restrictedIter(len(m.want))
					got := m.fn(ki)
					require.Equal(t, m.want, got)
					require.Equal(t, len(m.want), cap(got))
					requireCanaryIntact(t, backing, len(m.want))

					// Every insufficient capacity, including those too small
					// for the roach key itself, must panic without writing
					// past the capacity.
					for c := range len(m.want) {
						t.Run(fmt.Sprintf("cap=%d", c), func(t *testing.T) {
							ki, backing := restrictedIter(c)
							require.True(t, materializePanics(func() { m.fn(ki) }),
								"materializing %d bytes into a %d-byte buffer did not panic", len(m.want), c)
							requireCanaryIntact(t, backing, c)
						})
					}
				})
			}
		})
	}
}
