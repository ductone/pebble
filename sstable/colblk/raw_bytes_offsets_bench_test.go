// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"unsafe"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/compression"
	"github.com/cockroachdb/pebble/internal/crc"
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

// BenchmarkDataBlockColdLoad measures loading a writer-produced data block on
// a block cache miss, which is where decoder validation runs. Warm iteration
// over an already loaded block is covered by BenchmarkDataBlockIter.
//
//   - metadata isolates the block metadata initialization.
//   - load+seek allocates a block buffer, copies the uncompressed block into it,
//     initializes the block metadata and an iterator over the handle, and seeks
//     to one key.
//   - miss/<algorithm> models an in-memory block read: copy the compressed block
//     into a buffer, checksum it, decompress it into a block buffer, then
//     initialize and seek as above. It excludes I/O.
//
// Run with -count and compare against a base with benchstat.
func BenchmarkDataBlockColdLoad(b *testing.B) {
	getValuer := getInternalValuer(func([]byte) base.InternalValue {
		return base.MakeInPlaceValue(nil)
	})
	for _, blk := range coldLoadBenchBlocks() {
		b.Run(blk.name, func(b *testing.B) {
			var pool block.BufferPool
			pool.Init(4, block.ForCompaction)
			defer pool.Release()

			b.Run("metadata", func(b *testing.B) {
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

			// loadAndSeek calls fill to populate a newly allocated block buffer,
			// then initializes the block metadata and an iterator, and seeks.
			loadAndSeek := func(b *testing.B, fill func(dst []byte)) {
				var it DataBlockIter
				it.InitOnce(&testKeysSchema, testkeys.Comparer, getValuer, NoTieringColumns())
				idx := 0
				b.SetBytes(int64(len(blk.data)))
				for b.Loop() {
					v := block.Alloc(len(blk.data), &pool)
					fill(v.BlockData())
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
			}

			b.Run("load+seek", func(b *testing.B) {
				loadAndSeek(b, func(dst []byte) { copy(dst, blk.data) })
			})

			for _, setting := range []compression.Setting{compression.MinLZFastest, compression.SnappySetting} {
				c := compression.GetCompressor(setting)
				compressed, used := c.Compress(nil, blk.data)
				c.Close()
				checksum := crc.New(compressed).Value()
				b.Run("miss/"+used.String(), func(b *testing.B) {
					d := compression.GetDecompressor(used.Algorithm)
					defer d.Close()
					loadAndSeek(b, func(dst []byte) {
						raw := block.Alloc(len(compressed), &pool)
						defer raw.Release()
						copy(raw.BlockData(), compressed)
						if crc.New(raw.BlockData()).Value() != checksum {
							b.Fatal("checksum mismatch")
						}
						if err := d.DecompressInto(dst, raw.BlockData()); err != nil {
							b.Fatal(err)
						}
					})
				})
			}
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

// BenchmarkRawBytesAtEmpty measures RawBytes.At over columns of nonempty,
// empty and alternating slices.
func BenchmarkRawBytesAtEmpty(b *testing.B) {
	const n = 1024
	for _, tc := range []struct {
		name  string
		empty func(i int) bool
	}{
		{"nonempty", func(int) bool { return false }},
		{"empty", func(int) bool { return true }},
		{"alternating", func(i int) bool { return i%2 == 1 }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			vals := make([][]byte, n)
			for i := range vals {
				if !tc.empty(i) {
					vals[i] = []byte("12345678")
				}
			}
			d := DecodeBlock(rawBytesBlockForBench(vals), 0)
			rb := d.RawBytes(0)
			var sink uintptr
			for b.Loop() {
				for i := range n {
					s := rb.At(i)
					sink += uintptr(len(s)) + uintptr(unsafe.Pointer(unsafe.SliceData(s)))
				}
			}
			if sink == 0 {
				b.Fatal("unexpected sink")
			}
		})
	}
}
