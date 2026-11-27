// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package blob

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/cockroachdb/crlib/testutils/leaktest"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/sstableinternal"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/stretchr/testify/require"
)

// TestRewrittenFileGap writes a blob file, rewrites it with a FileRewriter
// that drops every value of one block (producing a gap in the virtual block
// mapping), and verifies that the rewritten file serves the live values while
// rejecting references to the gap and out-of-range block or value IDs.
func TestRewrittenFileGap(t *testing.T) {
	defer leaktest.AfterTest(t)()
	for _, cacheSize := range []int64{0, 1 << 20} {
		t.Run(fmt.Sprintf("cache=%d", cacheSize), func(t *testing.T) {
			testRewrittenFileGap(t, cacheSize)
		})
	}
}

func testRewrittenFileGap(t *testing.T, cacheSize int64) {
	ctx := context.Background()
	const blobFileID = base.BlobFileID(1)
	const origFileNum, rewrittenFileNum = base.DiskFileNum(1), base.DiskFileNum(2)
	opts := FileWriterOptions{
		FlushGovernor: block.MakeFlushGovernor(64<<10, 90, 0, nil),
	}
	val := func(blockID BlockID, valueID BlockValueID) []byte {
		return fmt.Appendf(nil, "b%d-v%d", blockID, valueID)
	}

	rp := &mockReaderProvider{readers: map[base.DiskFileNum]*FileReader{}}
	if cacheSize > 0 {
		rp.c = cache.NewWithShards(cacheSize, 1)
		rp.ch = rp.c.NewHandle()
	}
	defer rp.Close()
	openReader := func(obj objstorage.Readable, fileNum base.DiskFileNum) {
		r, err := NewFileReader(ctx, obj, FileReaderOptions{
			ReaderOptions: block.ReaderOptions{
				CacheOpts: sstableinternal.CacheOptions{CacheHandle: rp.ch, FileNum: fileNum},
			},
		})
		require.NoError(t, err)
		rp.readers[fileNum] = r
	}
	defer func() {
		for _, r := range rp.readers {
			require.NoError(t, r.Close())
		}
	}()

	// Write the original file: four physical blocks of four values each.
	origObj := &objstorage.MemObj{}
	w := NewFileWriter(origFileNum, origObj, opts)
	for b := BlockID(0); b < 4; b++ {
		if b > 0 {
			w.flush()
		}
		for v := BlockValueID(0); v < 4; v++ {
			h := w.AddValue(val(b, v), false /* isLikelyMVCCGarbage */)
			require.Equal(t, b, h.BlockID)
			require.Equal(t, v, h.ValueID)
		}
	}
	_, err := w.Close()
	require.NoError(t, err)
	openReader(origObj, origFileNum)

	// Rewrite the file. Virtual block 1 has no live values (a gap). Virtual
	// blocks 0 and 2 share physical block 0; virtual block 2 has a nonzero
	// valueID offset. Virtual block 3 is in physical block 1.
	rewrittenObj := &objstorage.MemObj{}
	rw := NewFileRewriter(blobFileID,
		base.ObjectInfoLiteral{FileType: base.FileTypeBlob, DiskFileNum: origFileNum},
		rp, block.ReadEnv{}, rewrittenFileNum, rewrittenObj, opts)
	copyBlock := func(blockID BlockID, valueIDs ...int) {
		var size int
		for _, v := range valueIDs {
			size += len(val(blockID, BlockValueID(v)))
		}
		require.NoError(t, rw.CopyBlock(ctx, blockID, size, valueIDs))
	}
	copyBlock(0, 0, 2)
	copyBlock(2, 1, 3)
	rw.w.flush()
	copyBlock(3, 0)
	stats, err := rw.Close()
	require.NoError(t, err)
	require.Equal(t, 2, int(stats.BlockCount))
	openReader(rewrittenObj, rewrittenFileNum)

	// The read-only layout formatter describes the gap without treating it as
	// a physical block.
	layout, err := rp.readers[rewrittenFileNum].Layout()
	require.NoError(t, err)
	require.Contains(t, layout, "virtual blocks mapping:\n"+
		"virtual block 0 -> physical block 0 (valueID offset: 0)\n"+
		"virtual block 1 -> unreferenced\n"+
		"virtual block 2 -> physical block 0 (valueID offset: 3)\n"+
		"virtual block 3 -> physical block 1 (valueID offset: 0)\n"+
		"\nphysical blocks:\n")

	newFetcher := func(diskFileNum base.DiskFileNum) *ValueFetcher {
		f := &ValueFetcher{}
		f.Init(inputFileMapping{info: base.ObjectInfoLiteral{
			FileType: base.FileTypeBlob, DiskFileNum: diskFileNum,
		}}, rp, block.ReadEnv{}, 1)
		return f
	}
	fetch := func(f *ValueFetcher, blockID BlockID, valueID BlockValueID) ([]byte, error) {
		v, _, err := f.Fetch(ctx, blobFileID, blockID, valueID)
		return v, err
	}
	expectValue := func(t *testing.T, f *ValueFetcher, blockID BlockID, valueID BlockValueID) {
		v, err := fetch(f, blockID, valueID)
		require.NoError(t, err, "fetch(%d, %d)", blockID, valueID)
		require.Equal(t, string(val(blockID, valueID)), string(v), "fetch(%d, %d)", blockID, valueID)
	}

	// Live values, including repeated fetches from the current block and
	// alternating between virtual blocks that share a physical block.
	t.Run("live", func(t *testing.T) {
		f := newFetcher(rewrittenFileNum)
		defer func() { require.NoError(t, f.Close()) }()
		for _, h := range []struct {
			b BlockID
			v BlockValueID
		}{{0, 0}, {0, 0}, {0, 2}, {2, 1}, {2, 3}, {0, 2}, {2, 3}, {3, 0}, {3, 0}, {2, 1}, {0, 0}} {
			expectValue(t, f, h.b, h.v)
		}
		// Value IDs skipped by the rewrite within a live virtual block's
		// range are empty placeholders, not errors.
		for _, h := range []struct {
			b BlockID
			v BlockValueID
		}{{0, 1}, {2, 0}, {2, 2}} {
			v, err := fetch(f, h.b, h.v)
			require.NoError(t, err, "fetch(%d, %d)", h.b, h.v)
			require.Empty(t, v, "fetch(%d, %d)", h.b, h.v)
		}
		// The end rows of the rewritten file's virtual blocks are computed
		// once and shared by the file's fetchers: virtual block 0 ends where
		// virtual block 2 begins in physical block 0, virtual block 1 is a gap
		// and virtual blocks 2 and 3 extend to the ends of their physical
		// blocks.
		endRows := rp.readers[rewrittenFileNum].virtualBlockEndRows.Load()
		require.NotNil(t, endRows)
		require.Equal(t, []uint32{3, 0, math.MaxUint32, math.MaxUint32}, *endRows)
		f2 := newFetcher(rewrittenFileNum)
		defer func() { require.NoError(t, f2.Close()) }()
		expectValue(t, f2, 2, 3)
		require.Same(t, endRows, rp.readers[rewrittenFileNum].virtualBlockEndRows.Load())
	})

	isCorruption := func(err error) bool { return errors.Is(err, base.ErrCorruption) }
	isGap := func(err error) bool { return errors.HasAssertionFailure(err) }
	type errCase struct {
		name    string
		file    base.DiskFileNum
		blockID BlockID
		valueID BlockValueID
		// warm, if set, is a valid (blockID, valueID) fetched before the
		// malformed reference so that the index block and (if the virtual
		// block matches) the value block are already loaded.
		warm *[2]uint32
		ok   func(error) bool
	}
	warm := func(b, v uint32) *[2]uint32 { return &[2]uint32{b, v} }
	var cases []errCase
	for _, wc := range []*[2]uint32{nil, warm(0, 0), warm(2, 1), warm(3, 0)} {
		cases = append(cases,
			errCase{"gap", rewrittenFileNum, 1, 0, wc, isGap},
			errCase{"virtual-block-out-of-range", rewrittenFileNum, 4, 0, wc, isCorruption},
			errCase{"virtual-block-max", rewrittenFileNum, math.MaxUint32, 0, wc, isCorruption},
			// Physical block 0 holds 7 rows; virtual block 2 starts at row 3,
			// so virtual block 0 holds rows [0, 3). Value IDs 3 and 4 of
			// virtual block 0 would resolve to virtual block 2's rows.
			errCase{"value-at-next-virtual-block", rewrittenFileNum, 0, 3, wc, isCorruption},
			errCase{"value-bleeds-into-next-virtual-block", rewrittenFileNum, 0, 4, wc, isCorruption},
			errCase{"value-past-block", rewrittenFileNum, 2, 4, wc, isCorruption},
			// MaxUint32-2 + 3 wraps to 0 in 32 bits.
			errCase{"value-wraps-32-bits", rewrittenFileNum, 2, math.MaxUint32 - 2, wc, isCorruption},
			// Physical block 1 holds 1 row.
			errCase{"value-past-last-block", rewrittenFileNum, 3, 1, wc, isCorruption},
		)
	}
	for _, wc := range []*[2]uint32{nil, warm(0, 0), warm(3, 3)} {
		cases = append(cases,
			errCase{"orig/physical-block-out-of-range", origFileNum, 4, 0, wc, isCorruption},
			errCase{"orig/value-out-of-range", origFileNum, 3, 4, wc, isCorruption},
		)
	}
	defer func() {
		// The original file was never rewritten, so its end rows are never
		// computed.
		require.Nil(t, rp.readers[origFileNum].virtualBlockEndRows.Load())
	}()
	for _, c := range cases {
		name := c.name + "/cold"
		if c.warm != nil {
			name = fmt.Sprintf("%s/warm=%d.%d", c.name, c.warm[0], c.warm[1])
		}
		t.Run(name, func(t *testing.T) {
			f := newFetcher(c.file)
			defer func() { require.NoError(t, f.Close()) }()
			if c.warm != nil {
				expectValue(t, f, BlockID(c.warm[0]), BlockValueID(c.warm[1]))
			}
			_, err := fetch(f, c.blockID, c.valueID)
			require.Error(t, err)
			require.True(t, c.ok(err), "unexpected error: %+v", err)
			// The fetcher remains usable after rejecting the reference.
			if c.warm != nil {
				expectValue(t, f, BlockID(c.warm[0]), BlockValueID(c.warm[1]))
			}
			if c.file == rewrittenFileNum {
				expectValue(t, f, 2, 3)
			} else {
				expectValue(t, f, 1, 1)
			}
		})
	}
}

// BenchmarkRewrittenFileAlternatingFetch measures fetching values from a
// rewritten blob file while alternating between two live virtual blocks that
// share a physical block. The rewrite drops every block between the two live
// blocks, leaving a hole of gap entries in the virtual block mapping. Each
// switch between the virtual blocks remaps the virtual block ID; the "repeat"
// variant fetches from a single virtual block for comparison. The block cache
// is large enough to hold every block, and is warmed before timing.
func BenchmarkRewrittenFileAlternatingFetch(b *testing.B) {
	for _, hole := range []int{0, 64, 4096, 65536} {
		b.Run(fmt.Sprintf("hole=%d", hole), func(b *testing.B) {
			benchmarkRewrittenFileAlternatingFetch(b, hole)
		})
	}
}

func benchmarkRewrittenFileAlternatingFetch(b *testing.B, hole int) {
	ctx := context.Background()
	const blobFileID = base.BlobFileID(1)
	const origFileNum, rewrittenFileNum = base.DiskFileNum(1), base.DiskFileNum(2)
	const valuesPerBlock = 16
	opts := FileWriterOptions{
		FlushGovernor: block.MakeFlushGovernor(64<<10, 90, 0, nil),
	}
	val := func(blockID BlockID, valueID BlockValueID) []byte {
		return fmt.Appendf(nil, "b%d-v%d", blockID, valueID)
	}

	rp := &mockReaderProvider{readers: map[base.DiskFileNum]*FileReader{}}
	rp.c = cache.NewWithShards(64<<20, 1)
	rp.ch = rp.c.NewHandle()
	defer rp.Close()
	openReader := func(obj objstorage.Readable, fileNum base.DiskFileNum) {
		r, err := NewFileReader(ctx, obj, FileReaderOptions{
			ReaderOptions: block.ReaderOptions{
				CacheOpts: sstableinternal.CacheOptions{CacheHandle: rp.ch, FileNum: fileNum},
			},
		})
		require.NoError(b, err)
		rp.readers[fileNum] = r
	}
	defer func() {
		for _, r := range rp.readers {
			require.NoError(b, r.Close())
		}
	}()

	// Write the original file with hole+2 blocks. Only the first and last
	// blocks hold values that survive the rewrite.
	lastBlockID := BlockID(hole + 1)
	origObj := &objstorage.MemObj{}
	w := NewFileWriter(origFileNum, origObj, opts)
	for blk := BlockID(0); blk <= lastBlockID; blk++ {
		if blk > 0 {
			w.flush()
		}
		n := BlockValueID(1)
		if blk == 0 || blk == lastBlockID {
			n = valuesPerBlock
		}
		for v := BlockValueID(0); v < n; v++ {
			w.AddValue(val(blk, v), false /* isLikelyMVCCGarbage */)
		}
	}
	_, err := w.Close()
	require.NoError(b, err)
	openReader(origObj, origFileNum)

	// Rewrite the file, keeping only the first and last blocks. Both land in
	// the same physical block.
	rewrittenObj := &objstorage.MemObj{}
	rw := NewFileRewriter(blobFileID,
		base.ObjectInfoLiteral{FileType: base.FileTypeBlob, DiskFileNum: origFileNum},
		rp, block.ReadEnv{}, rewrittenFileNum, rewrittenObj, opts)
	valueIDs := make([]int, valuesPerBlock)
	for i := range valueIDs {
		valueIDs[i] = i
	}
	for _, blk := range []BlockID{0, lastBlockID} {
		var size int
		for _, v := range valueIDs {
			size += len(val(blk, BlockValueID(v)))
		}
		require.NoError(b, rw.CopyBlock(ctx, blk, size, valueIDs))
	}
	stats, err := rw.Close()
	require.NoError(b, err)
	require.Equal(b, 1, int(stats.BlockCount))
	openReader(rewrittenObj, rewrittenFileNum)

	run := func(b *testing.B, blockIDs []BlockID) {
		var f ValueFetcher
		f.Init(inputFileMapping{info: base.ObjectInfoLiteral{
			FileType: base.FileTypeBlob, DiskFileNum: rewrittenFileNum,
		}}, rp, block.ReadEnv{}, 1)
		defer func() { require.NoError(b, f.Close()) }()
		fetch := func(i int) {
			blk := blockIDs[i%len(blockIDs)]
			v, _, err := f.Fetch(ctx, blobFileID, blk, BlockValueID(i%valuesPerBlock))
			if err != nil {
				b.Fatal(err)
			}
			if len(v) == 0 {
				b.Fatal("empty value")
			}
		}
		// Warm the index block, value block and block cache.
		for i := 0; i < 2*len(blockIDs); i++ {
			fetch(i)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			fetch(i)
		}
		b.StopTimer()
	}
	b.Run("alternating", func(b *testing.B) { run(b, []BlockID{0, lastBlockID}) })
	b.Run("repeat", func(b *testing.B) { run(b, []BlockID{0}) })
}
