// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package blob

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/cockroachdb/crlib/crstrings"
	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/v2/internal/base"
	"github.com/cockroachdb/pebble/v2/sstable/block"
	"github.com/stretchr/testify/require"
)

func TestIndexBlockEncoding(t *testing.T) {
	var decoder indexBlockDecoder
	var buf bytes.Buffer
	datadriven.RunTest(t, "testdata/index_block", func(t *testing.T, d *datadriven.TestData) string {
		buf.Reset()
		switch d.Cmd {
		case "build":
			var e indexBlockEncoder
			e.Init()
			lines := crstrings.Lines(d.Input)
			var i int
			for i = 0; i < len(lines); i++ {
				line := lines[i]
				if line == "virtual-block-mappings" {
					i += 1
					break
				}
				fields := strings.Fields(line)
				require.Len(t, fields, 2)
				var err error
				var h block.Handle
				h.Offset, err = strconv.ParseUint(fields[0], 10, 64)
				require.NoError(t, err)
				h.Length, err = strconv.ParseUint(fields[1], 10, 64)
				require.NoError(t, err)
				e.AddBlockHandle(h)
			}
			if i < len(lines) {
				for _, line := range lines[i:] {
					fields := strings.Fields(line)
					require.Len(t, fields, 3)
					vblockID, err := strconv.ParseInt(fields[0], 10, 64)
					require.NoError(t, err)
					physicalBlockIndex, err := strconv.ParseInt(fields[1], 10, 64)
					require.NoError(t, err)
					valueID, err := strconv.ParseInt(fields[2], 10, 64)
					require.NoError(t, err)
					e.AddVirtualBlockMapping(BlockID(vblockID), int(physicalBlockIndex), BlockValueID(valueID))
				}
			}

			data := e.Finish()
			decoder.Init(data)
			fmt.Fprint(&buf, decoder.DebugString())
			return buf.String()

		case "get":
			for _, arg := range d.CmdArgs {
				blockIndex, err := strconv.Atoi(arg.Key)
				require.NoError(t, err)
				h, err := decoder.BlockHandle(blockIndex)
				if err != nil {
					fmt.Fprintf(&buf, "%d: error: %s\n", blockIndex, err)
					continue
				}
				fmt.Fprintf(&buf, "%d: %s\n", blockIndex, h)
			}
			return buf.String()

		case "remap-virtual-blockid":
			for _, arg := range d.CmdArgs {
				blockID, err := strconv.ParseInt(arg.Key, 10, 64)
				require.NoError(t, err)
				blockIndex, valueIDOffset, endRow, err := decoder.RemapVirtualBlockID(BlockID(blockID))
				if err != nil {
					fmt.Fprintf(&buf, "%d -> error: %s\n", blockID, err)
					continue
				}
				fmt.Fprintf(&buf, "%d -> block %d, with valueID offset %d", blockID, blockIndex, valueIDOffset)
				if endRow != math.MaxUint64 {
					fmt.Fprintf(&buf, ", end row %d", endRow)
				}
				fmt.Fprintln(&buf)
			}
			return buf.String()

		default:
			panic(fmt.Sprintf("unknown command: %s", d.Cmd))
		}
	})
}

func TestIndexBlockDecoderMalformedOffsets(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offsets []uint64
	}{
		{name: "decreasing", offsets: []uint64{10, 35, 20}},
		{name: "shorter-than-trailer", offsets: []uint64{10, 35, 35 + block.TrailerLen - 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e indexBlockEncoder
			e.Init()
			for i, off := range tc.offsets {
				e.offsets.Set(i, off)
			}
			e.countBlocks = len(tc.offsets) - 1
			var d indexBlockDecoder
			d.Init(e.Finish())
			h, err := d.BlockHandle(0)
			require.NoError(t, err)
			require.Equal(t, block.Handle{Offset: 10, Length: 20}, h)
			_, err = d.BlockHandle(1)
			require.True(t, errors.Is(err, base.ErrCorruption), "unexpected error: %v", err)
		})
	}
}

// TestIndexBlockDecoderMaxRowCount checks that an index block whose header
// claims math.MaxUint32 physical blocks is rejected even when its offsets
// column is constant-encoded, which would otherwise decode with 2^32 rows.
func TestIndexBlockDecoderMaxRowCount(t *testing.T) {
	var e indexBlockEncoder
	e.Init()
	// Zero offsets use the zero-width uint encoding, which a row count does
	// not bound by the size of the column.
	e.offsets.Set(0, 0)
	e.offsets.Set(1, 0)
	e.countBlocks = 1
	data := e.Finish()
	var d indexBlockDecoder
	d.Init(data)
	require.Equal(t, 1, d.BlockCount())
	// The columnar header follows the 4-byte virtual block count; its row
	// count follows the 1-byte version and the 2-byte column count.
	const rowsOffset = indexBlockCustomHeaderSize + 1 + 2
	require.Equal(t, uint32(1), binary.LittleEndian.Uint32(data[rowsOffset:]))
	binary.LittleEndian.PutUint32(data[rowsOffset:], math.MaxUint32)

	var md block.Metadata
	err := initIndexBlockMetadata(&md, data)
	require.True(t, errors.Is(err, base.ErrCorruption), "unexpected error: %v", err)
	require.ErrorContains(t, err, "invalid row count")
}
