// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/blockiter"
)

// coldLoadBenchBlock is a writer-produced data block, along with keys that
// SeekGE may target within it.
type coldLoadBenchBlock struct {
	name     string
	data     []byte
	seekKeys [][]byte
}

func coldLoadBenchBlocks() []coldLoadBenchBlock {
	const targetBlockSize = 32 << 10
	build := func(name string, add func(w *DataBlockEncoder, i int) []byte) coldLoadBenchBlock {
		var w DataBlockEncoder
		w.Init(&testKeysSchema, NoTieringColumns())
		var prefixes [][]byte
		for i := 0; w.Size() < targetBlockSize; i++ {
			if p := add(&w, i); p != nil {
				prefixes = append(prefixes, p)
			}
		}
		data, _ := w.Finish(w.Rows(), w.Size())
		rng := rand.New(rand.NewPCG(0, 1))
		rng.Shuffle(len(prefixes), func(i, j int) { prefixes[i], prefixes[j] = prefixes[j], prefixes[i] })
		return coldLoadBenchBlock{
			name:     fmt.Sprintf("%s/rows=%d", name, w.Rows()),
			data:     data,
			seekKeys: prefixes,
		}
	}
	add := func(w *DataBlockEncoder, key, val []byte) {
		ik := base.MakeInternalKey(key, 1, base.InternalKeyKindSet)
		kcmp := w.KeyWriter.ComparePrev(ik.UserKey)
		w.Add(ik, val, block.InPlaceValuePrefix(kcmp.PrefixEqual()), kcmp, false /* isObsolete */, base.KVMeta{})
	}
	versionedKey := func(prefix []byte, ts int64) []byte {
		key := make([]byte, len(prefix)+testkeys.SuffixLen(ts))
		copy(key, prefix)
		testkeys.WriteSuffix(key[len(prefix):], ts)
		return key
	}

	var blocks []coldLoadBenchBlock
	// The BenchmarkDataBlockIter block: many small rows, 10 versions of each
	// prefix and 1-byte values.
	blocks = append(blocks, build("versions", func(w *DataBlockEncoder, i int) []byte {
		prefix := []byte(fmt.Sprintf("%08d", i))
		for ts := int64(10); ts >= 1; ts-- {
			add(w, versionedKey(prefix, ts), []byte{0})
		}
		return prefix
	}))
	// One version of each key with 128-byte values: fewer rows.
	val := make([]byte, 128)
	blocks = append(blocks, build("values128", func(w *DataBlockEncoder, i int) []byte {
		prefix := []byte(fmt.Sprintf("%012d", i))
		add(w, versionedKey(prefix, 1), val)
		return prefix
	}))
	// The BenchmarkDataBlockDecoderInit block: random 8-byte prefixes and
	// 8-byte values.
	keys, values := makeTestKeyRandomKVs(rand.New(rand.NewPCG(0, 20250919)), 8, 8, targetBlockSize)
	blocks = append(blocks, build("random", func(w *DataBlockEncoder, i int) []byte {
		add(w, keys[i], values[i])
		return keys[i][:8]
	}))
	return blocks
}

// BenchmarkDataBlockColdLoad compares loading a data block on a block cache
// miss with warm iteration over the same writer-produced block.
//
//   - cold/load+seek models a cache miss followed by one sparse seek: allocate a
//     block buffer, copy the block into it (standing in for the read), initialize
//     the cached block metadata, initialize an iterator over the handle, and
//     SeekGE to one key.
//   - cold/metadata isolates the block metadata initialization, which is where
//     decoder validation runs on a cache miss.
//   - warm/seek and warm/next use an iterator over a block whose metadata was
//     initialized once, as for a block cache hit.
//
// Constructor validation that is O(rows) appears only in the cold
// sub-benchmarks. Run with -count and compare against a base with benchstat.
func BenchmarkDataBlockColdLoad(b *testing.B) {
	getValuer := getInternalValuer(func([]byte) base.InternalValue {
		return base.MakeInPlaceValue(nil)
	})
	for _, blk := range coldLoadBenchBlocks() {
		b.Run(blk.name, func(b *testing.B) {
			var pool block.BufferPool
			pool.Init(2, block.ForCompaction)
			defer pool.Release()

			b.Run("cold/load+seek", func(b *testing.B) {
				var it DataBlockIter
				it.InitOnce(&testKeysSchema, testkeys.Comparer, getValuer, NoTieringColumns())
				idx := 0
				b.SetBytes(int64(len(blk.data)))
				for b.Loop() {
					v := block.Alloc(len(blk.data), &pool)
					copy(v.BlockData(), blk.data)
					if err := InitDataBlockMetadata(&testKeysSchema, v.BlockMetadata(), v.BlockData()); err != nil {
						b.Fatal(err)
					}
					// InitHandle releases the previous iteration's buffer.
					if err := it.InitHandle(testkeys.Comparer, v.MakeHandle(), blockiter.Transforms{}); err != nil {
						b.Fatal(err)
					}
					if kv := it.SeekGE(blk.seekKeys[idx], base.SeekGEFlagsNone); kv == nil {
						b.Fatal("key not found")
					}
					idx++
					if idx == len(blk.seekKeys) {
						idx = 0
					}
				}
				_ = it.Close()
			})

			b.Run("cold/metadata", func(b *testing.B) {
				v := block.Alloc(len(blk.data), &pool)
				defer v.Release()
				copy(v.BlockData(), blk.data)
				b.SetBytes(int64(len(blk.data)))
				for b.Loop() {
					if err := InitDataBlockMetadata(&testKeysSchema, v.BlockMetadata(), v.BlockData()); err != nil {
						b.Fatal(err)
					}
				}
			})

			v := block.Alloc(len(blk.data), &pool)
			copy(v.BlockData(), blk.data)
			if err := InitDataBlockMetadata(&testKeysSchema, v.BlockMetadata(), v.BlockData()); err != nil {
				b.Fatal(err)
			}
			var it DataBlockIter
			it.InitOnce(&testKeysSchema, testkeys.Comparer, getValuer, NoTieringColumns())
			if err := it.InitHandle(testkeys.Comparer, v.MakeHandle(), blockiter.Transforms{}); err != nil {
				b.Fatal(err)
			}
			defer func() { _ = it.Close() }()

			b.Run("warm/seek", func(b *testing.B) {
				idx := 0
				for b.Loop() {
					if kv := it.SeekGE(blk.seekKeys[idx], base.SeekGEFlagsNone); kv == nil {
						b.Fatal("key not found")
					}
					idx++
					if idx == len(blk.seekKeys) {
						idx = 0
					}
				}
			})

			b.Run("warm/next", func(b *testing.B) {
				kv := it.First()
				for b.Loop() {
					if kv == nil {
						kv = it.First()
					} else {
						kv = it.Next()
					}
				}
			})
		})
	}
}

// BenchmarkDecodeRawBytes measures decoding a 32 KiB RawBytes column, which
// validates its offsets once, and decoding a width-zero column whose encoded
// size is constant regardless of its logical count.
func BenchmarkDecodeRawBytes(b *testing.B) {
	for _, sliceLen := range []int{8, 128, 1024} {
		b.Run(fmt.Sprintf("sliceLen=%d", sliceLen), func(b *testing.B) {
			n := (32 << 10) / sliceLen
			vals := make([][]byte, n)
			for i := range vals {
				vals[i] = make([]byte, sliceLen)
			}
			data := rawBytesBlockForBench(vals)
			d := DecodeBlock(data, 0)
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				_ = d.RawBytes(0)
			}
		})
	}
	data := rawBytesBlockForBench([][]byte{nil})
	d := DecodeBlock(data, 0)
	for _, rows := range []uint32{1, 1 << 10, 1 << 20, 1 << 30} {
		b.Run(fmt.Sprintf("width=0/rows=%d", rows), func(b *testing.B) {
			for b.Loop() {
				_ = DecodeColumn(&d, 0, rows, DataTypeBytes, DecodeRawBytes)
			}
		})
	}
}

func rawBytesBlockForBench(vals [][]byte) []byte {
	var rb RawBytesBuilder
	rb.Init()
	for _, v := range vals {
		rb.Put(v)
	}
	return FinishBlock(len(vals), []ColumnWriter{&rb})
}
