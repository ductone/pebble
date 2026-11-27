// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/crlib/crstrings"
	"github.com/cockroachdb/crlib/testutils/leaktest"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/blobtest"
	"github.com/cockroachdb/pebble/internal/bytealloc"
	"github.com/cockroachdb/pebble/internal/cache"
	"github.com/cockroachdb/pebble/internal/manifest"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/internal/testutils"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/sstable/blob"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/valsep"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

func TestBlobRewrite(t *testing.T) {
	defer leaktest.AfterTest(t)()
	var (
		bv  blobtest.Values
		vs  valsep.ValueSeparation
		tw  sstable.RawWriter
		fn  base.DiskFileNum
		buf bytes.Buffer
		// Set up a logging FS to capture the filesystem operations throughout
		// the test. When testing a value separation policy that writes new blob
		// files, this demonstrates the creation of new blob files and that
		// they're created lazily, the first time a value is actually separated.
		fs = vfs.WithLogging(vfs.NewMem(), func(format string, args ...interface{}) {
			fmt.Fprint(&buf, "# ")
			fmt.Fprintf(&buf, format, args...)
			fmt.Fprintln(&buf)
		})
	)
	ctx := context.Background()
	var st objstorageprovider.Settings
	st.Local.FS = fs
	objStore, err := objstorageprovider.Open(st)
	require.NoError(t, err)
	defer objStore.Close()

	initRawWriter := func() {
		if tw != nil {
			require.NoError(t, tw.Close())
		}
		fn++
		w, _, err := objStore.Create(ctx, base.FileTypeTable, fn, objstorage.CreateOptions{})
		require.NoError(t, err)
		tw = sstable.NewRawWriter(w, sstable.WriterOptions{
			TableFormat: sstable.TableFormatMax,
		})
		tw = &sstable.LoggingRawWriter{LogWriter: &buf, RawWriter: tw}
	}

	datadriven.RunTest(t, "testdata/blob_rewrite",
		func(t *testing.T, d *datadriven.TestData) string {
			buf.Reset()
			switch d.Cmd {
			case "init":
				if tw != nil {
					require.NoError(t, tw.Close())
				}
				bv = blobtest.Values{}
				switch x := d.CmdArgs[0].String(); x {
				case "preserve-blob-references":
					lines := crstrings.Lines(d.Input)
					inputBlobPhysicalFiles := make(map[base.BlobFileID]*manifest.PhysicalBlobFile, len(lines))
					for _, line := range lines {
						bfm, err := manifest.ParseBlobFileMetadataDebug(line)
						require.NoError(t, err)
						fn = max(fn, bfm.Physical.FileNum)
						inputBlobPhysicalFiles[bfm.FileID] = bfm.Physical
					}
					pbr := valsep.NewPreserveAllHotBlobReferences(
						inputBlobPhysicalFiles,
						0, /* outputBlobReferenceDepth */
						0, /* minimumSize */
						0, /* minimum MVCC garbage size */
					)
					vs = pbr
				case "write-new-blob-files":
					var minimumSize int
					d.MaybeScanArgs(t, "minimum-size", &minimumSize)
					newSep := valsep.NewWriteNewBlobFiles(
						testkeys.Comparer,
						func() (objstorage.Writable, objstorage.ObjectMetadata, error) {
							fn++
							return objStore.Create(ctx, base.FileTypeBlob, fn, objstorage.CreateOptions{})
						},
						blob.FileWriterOptions{},
						minimumSize,
						0, /* minimum MVCC garbage size */
						valsep.WriteNewBlobFilesOptions{},
					)
					vs = newSep
				default:
					t.Fatalf("unknown value separation policy: %s", x)
				}
				return buf.String()
			case "add":
				if tw == nil {
					initRawWriter()
				}
				for line := range crstrings.LinesSeq(d.Input) {
					ikv := base.ParseInternalKV(line)
					if value := string(ikv.InPlaceValue()); strings.HasPrefix(value, "blob") {
						ikv.V, err = bv.ParseInternalValue(value)
						require.NoError(t, err)
					}
					require.NoError(t, vs.Add(tw, &ikv, false /* forceObsolete */, false /* isLikelyMVCCGarbage */, base.KVMeta{}))
				}
				return buf.String()
			case "close-output":
				if tw != nil {
					require.NoError(t, tw.Close())
					tw = nil
				}

				meta, err := vs.FinishOutput()
				require.NoError(t, err)
				if len(meta.NewBlobFiles) == 0 {
					fmt.Fprintln(&buf, "no blob file created")
				} else {
					for _, bf := range meta.NewBlobFiles {
						fmt.Fprintf(&buf, "Blob file created: %s\n", bf.FileMetadata)
						fmt.Fprintln(&buf, bf.FileStats)
					}
				}
				if len(meta.BlobReferences) == 0 {
					fmt.Fprintln(&buf, "blobrefs:[]")
				} else {
					fmt.Fprintln(&buf, "blobrefs:[")
					for i, ref := range meta.BlobReferences {
						fmt.Fprintf(&buf, " %d: %s %d", i, ref.FileID, ref.ValueSize)
						if ref.ValueSize != ref.BackingValueSize && ref.BackingValueSize > 0 {
							fmt.Fprintf(&buf, "/%d", ref.BackingValueSize)
						}
						fmt.Fprintf(&buf, "\n")
					}
					fmt.Fprintln(&buf, "]")
				}
				return buf.String()
			case "rewrite-blob":
				// rewrite-blob <target-blob-file> <sstable1> <sstable2> ...
				if len(d.CmdArgs) < 2 {
					t.Fatalf("rewrite-blob requires at least a target blob file and one SSTable")
				}
				targetBlobFileStr := d.CmdArgs[0].String()
				targetBlobFileNum, err := strconv.ParseUint(targetBlobFileStr, 10, 64)
				if err != nil {
					t.Fatalf("invalid blob file number %s: %v", targetBlobFileStr, err)
				}
				var sstableFileNums []base.DiskFileNum
				for i := 1; i < len(d.CmdArgs); i++ {
					sstFileNumStr := d.CmdArgs[i].String()
					sstFileNum, err := strconv.ParseUint(sstFileNumStr, 10, 64)
					if err != nil {
						t.Fatalf("invalid SSTable file number %s: %v", sstFileNumStr, err)
					}
					sstableFileNums = append(sstableFileNums, base.DiskFileNum(sstFileNum))
				}

				fileCache := NewFileCache(1, 100)
				defer fileCache.Unref()
				blockCache := NewCache(1024)
				defer blockCache.Unref()
				blockCacheHandle := blockCache.NewHandle()
				defer blockCacheHandle.Close()
				mockFC := fileCache.newHandle(
					blockCacheHandle,
					objStore,
					&base.LoggerWithNoopTracer{Logger: base.DefaultLogger},
					sstable.ReaderOptions{},
					func(base.ObjectInfo, error) error { return nil },
				)
				defer mockFC.Close()
				var sstables []*manifest.TableMetadata
				for _, sstFileNum := range sstableFileNums {
					sst := &manifest.TableMetadata{
						TableBacking: &manifest.TableBacking{
							DiskFileNum: sstFileNum,
						},
						BlobReferences: []manifest.BlobReference{{FileID: base.BlobFileID(targetBlobFileNum)}},
					}
					sstables = append(sstables, sst)
				}

				inputBlob := manifest.BlobFileMetadata{
					FileID: base.BlobFileID(targetBlobFileNum),
					Physical: &manifest.PhysicalBlobFile{
						FileNum:      base.DiskFileNum(targetBlobFileNum),
						CreationTime: uint64(time.Now().Unix()),
					},
				}
				fn++
				outputWritable, _, err := objStore.Create(ctx, base.FileTypeBlob, fn, objstorage.CreateOptions{})
				require.NoError(t, err)

				rewriter := newBlobFileRewriter(mockFC, block.ReadEnv{}, fn, outputWritable,
					blob.FileWriterOptions{}, sstables, inputBlob)
				stats, err := rewriter.Rewrite(context.Background())
				if err != nil {
					fmt.Fprintf(&buf, "rewrite error: %v\n", err)
				} else {
					fmt.Fprintf(&buf, "Successfully rewrote blob file %s to %s\n",
						targetBlobFileStr, fn.String())
					fmt.Fprintf(&buf, "Input SSTables: %v\n", sstableFileNums)
					fmt.Fprintf(&buf, "SSTables with blob references: %d\n", len(sstables))
					fmt.Fprintln(&buf, stats)
				}

				return buf.String()
			default:
				t.Fatalf("unknown command: %s", d.Cmd)
			}
			panic("unreachable")
		})
}

// TestBlobRewriteRandomized tests blob file rewriting by constructing a blob
// file and n sstables that each reference one value in the blob file.
//
// It then runs a blob rewrite repeatedly, passing in a random subset of the
// sstables as extant references. Each blob rewrite may rewrite the original
// blob file, or one of the previous iteration's rewritten blob files.
func TestBlobRewriteRandomized(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const numKVs = 1000
	const blobFileID = 100000
	const numRewrites = 10

	seed := time.Now().UnixNano()
	t.Logf("seed: %d", seed)
	rng := rand.New(rand.NewPCG(0, uint64(seed)))

	// Generate keys and values.
	keys, values := func() (keys, values [][]byte) {
		keys = make([][]byte, numKVs)
		values = make([][]byte, numKVs)
		var alloc bytealloc.A
		for i := 0; i < numKVs; i++ {
			var key, value []byte
			key, alloc = alloc.Copy([]byte(fmt.Sprintf("%06d", i)))
			value, alloc = alloc.Copy(append([]byte(fmt.Sprintf("%d", i)),
				bytes.Repeat([]byte{'v'}, rng.IntN(100))...))
			keys[i] = key
			values[i] = value
		}
		return keys, values
	}()

	ctx := context.Background()
	var st objstorageprovider.Settings
	st.Local.FS = vfs.NewMem()
	objStore, err := objstorageprovider.Open(st)
	require.NoError(t, err)

	// Write the source blob file.
	var originalBlobFile manifest.BlobFileMetadata
	handles := make([]blob.Handle, numKVs)
	{
		w, _, err := objStore.Create(ctx, base.FileTypeBlob, base.DiskFileNum(blobFileID), objstorage.CreateOptions{})
		require.NoError(t, err)
		blobWriter := blob.NewFileWriter(base.DiskFileNum(blobFileID), w, blob.FileWriterOptions{})
		for i := range numKVs {
			if rng.IntN(20) == 0 {
				blobWriter.FlushForTesting()
			}
			handles[i] = blobWriter.AddValue(values[i], false /* isLikelyMVCCGarbage */)
		}
		stats, err := blobWriter.Close()
		require.NoError(t, err)
		require.Equal(t, numKVs, int(stats.ValueCount))
		originalBlobFile = manifest.BlobFileMetadata{
			FileID: base.BlobFileID(blobFileID),
			Physical: &manifest.PhysicalBlobFile{
				FileNum:      base.DiskFileNum(blobFileID),
				CreationTime: uint64(time.Now().Unix()),
				ValueSize:    stats.UncompressedValueBytes,
				Size:         stats.FileLen,
			},
		}
	}

	// Write numKVs SSTables, each with 1 kv pair, all referencing the same blob
	// file.
	originalTables := make([]*manifest.TableMetadata, numKVs)
	originalValueIndices := make([]int, numKVs)
	for i := range numKVs {
		w, _, err := objStore.Create(ctx, base.FileTypeTable, base.DiskFileNum(i), objstorage.CreateOptions{})
		require.NoError(t, err)
		tw := sstable.NewRawWriter(w, sstable.WriterOptions{
			TableFormat: sstable.TableFormatMax,
		})
		require.NoError(t, tw.AddWithBlobHandle(
			base.MakeInternalKey(keys[i], base.SeqNum(i), base.InternalKeyKindSet),
			blob.InlineHandle{
				InlineHandlePreface: blob.InlineHandlePreface{
					ReferenceID: base.BlobReferenceID(0),
					ValueLen:    uint32(len(values[i])),
				},
				HandleSuffix: blob.HandleSuffix{
					BlockID: handles[i].BlockID,
					ValueID: handles[i].ValueID,
				},
			},
			base.ShortAttribute(0),
			false, /* forceObsolete */
			base.KVMeta{},
		))
		require.NoError(t, tw.Close())
		originalValueIndices[i] = i
		valueSize := uint64(len(values[i]))
		originalTables[i] = &manifest.TableMetadata{
			TableNum: base.TableNum(i),
			TableBacking: &manifest.TableBacking{
				DiskFileNum: base.DiskFileNum(i),
			},
			BlobReferences: []manifest.BlobReference{
				{FileID: base.BlobFileID(blobFileID), ValueSize: valueSize, BackingValueSize: valueSize},
			},
		}
	}

	fc := NewFileCache(1, 100)
	defer fc.Unref()
	c := cache.New(100)
	defer c.Unref()
	ch := c.NewHandle()
	defer ch.Close()
	fch := fc.newHandle(ch, objStore, base.NoopLoggerAndTracer{}, sstable.ReaderOptions{}, nil)
	defer fch.Close()
	var bufferPool block.BufferPool
	bufferPool.Init(4, block.ForBlobFileRewrite)
	defer bufferPool.Release()
	readEnv := block.ReadEnv{BufferPool: &bufferPool}

	type sourceFile struct {
		metadata          manifest.BlobFileMetadata
		valueIndices      []int
		referencingTables []*manifest.TableMetadata
	}
	files := []sourceFile{{
		metadata:          originalBlobFile,
		valueIndices:      originalValueIndices,
		referencingTables: originalTables,
	}}

	var verboseBuffer bytes.Buffer
	verboseBuffer.Grow(16 << 10 /* 16KiB */)
	defer func() {
		if t.Failed() {
			t.Logf("verbose output:\n%s", verboseBuffer.String())
		}
	}()

	for i := range numRewrites {
		fileIdx := rng.IntN(len(files))
		fileToRewrite := files[fileIdx]

		// Rewrite the blob file.
		newBlobFileNum := base.DiskFileNum(blobFileID + i + 1)

		// Pick a random subset of the referencing tables to use as remaining
		// extant references.
		n := 1
		if len(fileToRewrite.valueIndices) > 1 {
			n = testutils.RandIntInRange(rng, 1, len(fileToRewrite.valueIndices))
		}
		fmt.Fprintf(&verboseBuffer, "rewriting file %s, preserving %d values\n", fileToRewrite.metadata.Physical.FileNum, n)

		// Produce the inputs for the rewrite.
		newFile := sourceFile{
			metadata: manifest.BlobFileMetadata{
				FileID: base.BlobFileID(blobFileID),
				Physical: &manifest.PhysicalBlobFile{
					FileNum: newBlobFileNum,
				},
			},
			valueIndices:      make([]int, n),
			referencingTables: make([]*manifest.TableMetadata, n),
		}
		for k, j := range rng.Perm(len(fileToRewrite.valueIndices))[:n] {
			valueIndex := fileToRewrite.valueIndices[j]
			newFile.valueIndices[k] = valueIndex
			newFile.referencingTables[k] = originalTables[valueIndex]
		}
		slices.Sort(newFile.valueIndices)
		for _, idx := range newFile.valueIndices {
			fmt.Fprintf(&verboseBuffer, "newFile.valueIndices: %d: %q; handle: %s\n", idx, values[idx], handles[idx])
		}

		// Rewrite the blob file.
		w, _, err := objStore.Create(ctx, base.FileTypeBlob, newBlobFileNum, objstorage.CreateOptions{})
		require.NoError(t, err)
		opts := blob.FileWriterOptions{
			FlushGovernor: block.MakeFlushGovernor(128<<rng.IntN(6), 90, 100, nil),
		}
		rewriter := newBlobFileRewriter(fch, readEnv, newBlobFileNum,
			w, opts, newFile.referencingTables, fileToRewrite.metadata)
		stats, err := rewriter.Rewrite(ctx)
		require.NoError(t, err)
		require.LessOrEqual(t, n, int(stats.ValueCount))
		newFile.metadata.Physical.ValueSize = stats.UncompressedValueBytes
		newFile.metadata.Physical.Size = stats.FileLen

		// Verify that the rewritten blob file contains the correct values, and
		// that they may still be accessed using the original handles.
		var valueFetcher blob.ValueFetcher
		valueFetcher.Init(constantFileMapping(newBlobFileNum), fch, readEnv,
			blob.SuggestedCachedReaders(1))
		func() {
			defer func() { _ = valueFetcher.Close() }()
			for _, valueIndex := range newFile.valueIndices {
				handle := handles[valueIndex]
				val, _, err := valueFetcher.Fetch(ctx, blobFileID, handle.BlockID, handle.ValueID)
				require.NoError(t, err)
				require.Equal(t, values[valueIndex], val)
			}
		}()

		// Add the new blob file to the list of blob files so that a future
		// rewrite can use it as the source file. This ensures we test rewrites
		// of rewritten blob files.
		files = append(files, newFile)
	}
}

// constantFileMapping implements base.BlobFileMapping and always maps to itself.
type constantFileMapping base.DiskFileNum

// Assert that (*inputFileMapping) implements base.BlobFileMapping.
var _ base.BlobFileMapping = constantFileMapping(0)

func (m constantFileMapping) Lookup(fileID base.BlobFileID) (base.ObjectInfo, bool) {
	return base.ObjectInfoLiteral{
		FileType:    base.FileTypeBlob,
		DiskFileNum: base.DiskFileNum(m),
		Bounds:      base.UserKeyBounds{},
	}, true
}

// TestBlobRewriteMalformedLivenessReference checks that a blob rewrite returns
// a corruption error naming the table, instead of panicking or reading past the
// block, when a table's blob reference liveness block cannot serve the
// reference ID its metadata assigns to the input blob file.
func TestBlobRewriteMalformedLivenessReference(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const blobFileNum = base.DiskFileNum(10)
	const tableFileNum = base.DiskFileNum(11)
	ctx := context.Background()

	// setup writes a blob file holding one value and a table whose single key
	// references it as reference ID 0, so the table's liveness block has one
	// row. mutate may rewrite the liveness block in place.
	setup := func(t *testing.T, mutate func(livenessBlock []byte)) objstorage.Provider {
		fs := vfs.NewMem()
		var st objstorageprovider.Settings
		st.Local.FS = fs
		objStore, err := objstorageprovider.Open(st)
		require.NoError(t, err)

		value := []byte("blob-value")
		w, _, err := objStore.Create(ctx, base.FileTypeBlob, blobFileNum, objstorage.CreateOptions{})
		require.NoError(t, err)
		blobWriter := blob.NewFileWriter(blobFileNum, w, blob.FileWriterOptions{})
		handle := blobWriter.AddValue(value, false /* isLikelyMVCCGarbage */)
		_, err = blobWriter.Close()
		require.NoError(t, err)

		w, _, err = objStore.Create(ctx, base.FileTypeTable, tableFileNum, objstorage.CreateOptions{})
		require.NoError(t, err)
		tw := sstable.NewRawWriter(w, sstable.WriterOptions{
			TableFormat: sstable.TableFormatMax,
			Checksum:    block.ChecksumTypeCRC32c,
		})
		require.NoError(t, tw.AddWithBlobHandle(
			base.MakeInternalKey([]byte("a"), 1, base.InternalKeyKindSet),
			blob.InlineHandle{
				InlineHandlePreface: blob.InlineHandlePreface{ReferenceID: 0, ValueLen: uint32(len(value))},
				HandleSuffix:        blob.HandleSuffix{BlockID: handle.BlockID, ValueID: handle.ValueID},
			},
			base.ShortAttribute(0),
			false, /* forceObsolete */
			base.KVMeta{},
		))
		require.NoError(t, tw.Close())

		if mutate != nil {
			mutateBlobRefIndexBlock(t, fs, objStore, tableFileNum, mutate)
		}
		return objStore
	}

	// rewrite runs the heap-generation phase of a blob rewrite, in which the
	// table's metadata lists the input blob file at position refID.
	rewrite := func(t *testing.T, objStore objstorage.Provider, refID int) error {
		fc := NewFileCache(1, 100)
		defer fc.Unref()
		c := cache.New(100)
		defer c.Unref()
		ch := c.NewHandle()
		defer ch.Close()
		fch := fc.newHandle(ch, objStore, base.NoopLoggerAndTracer{}, sstable.ReaderOptions{}, nil)
		defer fch.Close()

		refs := make([]manifest.BlobReference, refID+1)
		for i := range refs {
			refs[i].FileID = base.BlobFileID(blobFileNum + 100 + base.DiskFileNum(i))
		}
		refs[refID].FileID = base.BlobFileID(blobFileNum)
		table := &manifest.TableMetadata{
			TableNum:       base.TableNum(tableFileNum),
			TableBacking:   &manifest.TableBacking{DiskFileNum: tableFileNum},
			BlobReferences: refs,
		}
		inputBlob := manifest.BlobFileMetadata{
			FileID:   base.BlobFileID(blobFileNum),
			Physical: &manifest.PhysicalBlobFile{FileNum: blobFileNum, CreationTime: uint64(time.Now().Unix())},
		}
		// Only the heap-generation phase reads the liveness block, so the
		// rewriter is built without an output blob file writer.
		rewriter := &blobFileRewriter{
			fc:        fch,
			readEnv:   block.ReadEnv{},
			sstables:  []*manifest.TableMetadata{table},
			inputBlob: inputBlob,
			blkHeap:   blockHeap{},
		}
		return rewriter.generateHeap(ctx)
	}

	t.Run("valid", func(t *testing.T) {
		objStore := setup(t, nil)
		defer objStore.Close()
		require.NoError(t, rewrite(t, objStore, 0))
	})

	t.Run("reference-id-beyond-block", func(t *testing.T) {
		objStore := setup(t, nil)
		defer objStore.Close()
		err := rewrite(t, objStore, 1)
		require.Error(t, err)
		require.True(t, base.IsCorruptionError(err), "%+v", err)
		require.Contains(t, err.Error(), "reference ID 1")
		require.Contains(t, err.Error(), "has 1 rows")
	})

	t.Run("malformed-block", func(t *testing.T) {
		objStore := setup(t, func(blk []byte) { blk[0]++ /* columnar block version */ })
		defer objStore.Close()
		err := rewrite(t, objStore, 0)
		require.Error(t, err)
		require.True(t, base.IsCorruptionError(err), "%+v", err)
		require.Contains(t, err.Error(), "table 000011 blob reference index")
		// Depending on the colblk checks below this package, either the block
		// decoder or InitChecked rejects the version.
		require.Regexp(t, `reference liveness block.*(?i:version)[ =]2`, err.Error())
	})

	t.Run("rows-max-int32", func(t *testing.T) {
		objStore := setup(t, func(blk []byte) { setLivenessBlockRows(blk, math.MaxInt32) })
		defer objStore.Close()
		err := rewrite(t, objStore, 0)
		require.Error(t, err)
		require.True(t, base.IsCorruptionError(err), "%+v", err)
		require.Contains(t, err.Error(), "table 000011 blob reference index")
		require.Regexp(t, `reference liveness block: .*extends? beyond`, err.Error())
	})

	t.Run("decreasing-offset", func(t *testing.T) {
		// The table has a single row, so make its start offset exceed its end.
		objStore := setup(t, func(blk []byte) { decreaseLivenessBlockOffset(t, blk, 0) })
		defer objStore.Close()
		err := rewrite(t, objStore, 0)
		require.Error(t, err)
		require.True(t, base.IsCorruptionError(err), "%+v", err)
		require.Contains(t, err.Error(), "table 000011 blob reference index")
		require.Contains(t, err.Error(), "is less than the preceding offset")
	})
}

// mutateBlobRefIndexBlock rewrites, in place on fs, the blob reference
// liveness block of the table with the given file number, which must have been
// written with CRC32c checksums. The block trailer's checksum is recomputed so
// that the mutation is not detected as a checksum mismatch.
func mutateBlobRefIndexBlock(
	t *testing.T,
	fs vfs.FS,
	objStore objstorage.Provider,
	tableFileNum base.DiskFileNum,
	mutate func(livenessBlock []byte),
) {
	ctx := context.Background()
	readable, err := objStore.OpenForReading(ctx, base.FileTypeTable, tableFileNum, objstorage.OpenOptions{})
	require.NoError(t, err)
	r, err := sstable.NewReader(ctx, readable, sstable.ReaderOptions{})
	require.NoError(t, err)
	layout, err := r.Layout()
	require.NoError(t, err)
	bh := layout.BlobReferenceIndex
	require.NoError(t, r.Close())
	require.NotZero(t, bh.Length)

	path := base.MakeFilepath(fs, "", base.FileTypeTable, tableFileNum)
	src, err := fs.Open(path)
	require.NoError(t, err)
	data, err := io.ReadAll(src)
	require.NoError(t, err)
	require.NoError(t, src.Close())
	blk := data[bh.Offset : bh.Offset+bh.Length]
	mutate(blk)
	var c block.Checksummer
	c.Init(block.ChecksumTypeCRC32c)
	blockType := data[bh.Offset+bh.Length]
	trailer := block.MakeTrailer(blockType, c.Checksum(blk, blockType))
	copy(data[bh.Offset+bh.Length:], trailer[:])
	f, err := fs.Create(path, vfs.WriteCategoryUnspecified)
	require.NoError(t, err)
	_, err = f.Write(data)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// The helpers below edit a reference liveness block in place, relying on the
// columnar block layout: a version byte, a 2-byte column count and a 4-byte row
// count, followed by the single column's type byte and 4-byte page start. The
// column is a RawBytes whose page begins with its offsets encoding byte.

// setLivenessBlockRows overwrites the block header's row count.
func setLivenessBlockRows(blk []byte, rows uint32) {
	binary.LittleEndian.PutUint32(blk[3:], rows)
}

// decreaseLivenessBlockOffset makes offset i of the block's offsets table
// exceed offset i+1. The offsets must be encoded with a 1-byte width, as they
// are for small blocks.
func decreaseLivenessBlockOffset(t *testing.T, blk []byte, i int) {
	page := binary.LittleEndian.Uint32(blk[8:])
	require.Equal(t, byte(1), blk[page], "expected 1-byte offsets")
	rows := int(binary.LittleEndian.Uint32(blk[3:]))
	require.Less(t, i, rows)
	offsets := blk[page+1 : int(page)+2+rows]
	require.Less(t, offsets[i+1], byte(0xff))
	offsets[i] = offsets[i+1] + 1
}
