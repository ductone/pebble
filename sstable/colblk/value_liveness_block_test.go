// Copyright 2025 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/datadriven"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/stretchr/testify/require"
)

func TestValueLivenessBlock(t *testing.T) {
	var decoder ReferenceLivenessBlockDecoder
	var buf bytes.Buffer
	datadriven.RunTest(t, "testdata/value_liveness_block", func(t *testing.T, d *datadriven.TestData) string {
		buf.Reset()
		switch d.Cmd {
		case "build":
			var w ReferenceLivenessBlockEncoder
			w.Init()
			for i, line := range strings.Split(d.Input, "\n") {
				fields := strings.Fields(line)
				value := fields[0]
				w.AddReferenceLiveness(i, []byte(value))
			}

			data := w.Finish()
			decoder.Init(data)
			fmt.Fprint(&buf, decoder.DebugString())
			return buf.String()
		default:
			return fmt.Sprintf("unknown command: %s", d.Cmd)
		}
	})
}

// TestReferenceLivenessBlockDecoderMalformed checks that InitChecked accepts
// writer-produced blocks and returns corruption errors for malformed ones, and
// that Init panics with the same error instead of reading out of bounds.
func TestReferenceLivenessBlockDecoderMalformed(t *testing.T) {
	values := [][]byte{{0x00, 0x18, 0x01, 0x27}, {0x02, 0x05, 0x01, 0xff}, {0x07, 0x09, 0x01, 0x04}}
	var w ReferenceLivenessBlockEncoder
	w.Init()
	for i, v := range values {
		w.AddReferenceLiveness(i, v)
	}
	valid := slices.Clone(w.Finish())

	var d ReferenceLivenessBlockDecoder
	require.NoError(t, d.InitChecked(valid))
	require.Equal(t, len(values), d.BlockDecoder().Rows())
	for i, v := range values {
		require.Equal(t, v, d.LivenessAtReference(i))
	}

	minSize := int(HeaderSize(valueLivenessBlockColumnCount, valueLivenessBlockCustomHeaderSize)) + 1
	mutate := func(f func(b []byte)) []byte {
		b := slices.Clone(valid)
		f(b)
		return b
	}
	testCases := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "truncated-header", data: valid[:5]},
		{name: "truncated-column-header", data: valid[:minSize-1]},
		{name: "truncated-column", data: valid[:len(valid)-1]},
		{name: "version", data: mutate(func(b []byte) { b[0] = byte(Version1) + 1 })},
		{name: "zero-columns", data: mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[1:], 0) })},
		{name: "two-columns", data: mutate(func(b []byte) { binary.LittleEndian.PutUint16(b[1:], 2) })},
		{name: "column-type", data: mutate(func(b []byte) { b[blockHeaderBaseSize] = byte(DataTypeUint) })},
		{name: "extra-row", data: mutate(func(b []byte) {
			binary.LittleEndian.PutUint32(b[1+align16:], uint32(len(values)+1))
		})},
		{name: "fewer-rows", data: mutate(func(b []byte) {
			binary.LittleEndian.PutUint32(b[1+align16:], uint32(len(values)-1))
		})},
		// A row count whose offsets table cannot fit in the block.
		{name: "rows-max-int32", data: mutate(func(b []byte) {
			binary.LittleEndian.PutUint32(b[1+align16:], math.MaxInt32)
		})},
		// An intermediate offset greater than its successor: [0, 9, 8, 12].
		{name: "decreasing-offset", data: mutate(func(b []byte) {
			page := binary.LittleEndian.Uint32(b[blockHeaderBaseSize+1:])
			require.Equal(t, []byte{byte(makeUintEncoding(1, false)), 0, 4, 8, 12}, b[page:page+5])
			b[page+2] = 9
		})},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var d ReferenceLivenessBlockDecoder
			require.NoError(t, d.InitChecked(valid))
			err := d.InitChecked(tc.data)
			require.Error(t, err)
			require.True(t, base.IsCorruptionError(err), "%+v", err)
			// A failed InitChecked leaves the decoder empty.
			require.Equal(t, 0, d.BlockDecoder().Rows())

			// Init keeps its signature and panics with the same corruption
			// error instead of reading out of bounds.
			d.Init(valid)
			func() {
				defer func() {
					r := recover()
					rErr, ok := r.(error)
					require.True(t, ok, "Init panicked with %v", r)
					require.True(t, base.IsCorruptionError(rErr), "%+v", rErr)
				}()
				d.Init(tc.data)
			}()
			require.Equal(t, 0, d.BlockDecoder().Rows())

			// The decoder is reusable after a failure.
			require.NoError(t, d.InitChecked(valid))
			require.Equal(t, values[1], d.LivenessAtReference(1))
		})
	}
}
