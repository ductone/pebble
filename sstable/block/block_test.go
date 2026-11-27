// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package block

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// overflowVarint is a 10-byte varint that overflows a uint64.
var overflowVarint = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}

func encodeHandle(h Handle) []byte {
	buf := make([]byte, 2*binary.MaxVarintLen64)
	return buf[:h.EncodeVarints(buf)]
}

func TestDecodeHandle(t *testing.T) {
	maxHandle := Handle{Offset: math.MaxUint64, Length: math.MaxUint64}
	maxEncoded := encodeHandle(maxHandle)
	maxVarint := maxEncoded[:binary.MaxVarintLen64]

	testCases := []struct {
		name     string
		src      []byte
		want     Handle
		consumed int
	}{
		{name: "empty", src: nil},
		{name: "truncated-first", src: []byte{0x80}},
		{name: "missing-second", src: []byte{0x01}},
		{name: "truncated-second", src: []byte{0x01, 0x80}},
		{name: "overflow-first", src: append(bytes.Clone(overflowVarint), 0x01)},
		{name: "overflow-second", src: append([]byte{0x01}, overflowVarint...)},
		{name: "overflow-second-after-max-first", src: append(bytes.Clone(maxVarint), overflowVarint...)},
		{name: "zero", src: []byte{0x00, 0x00}, consumed: 2},
		{name: "max", src: maxEncoded, want: maxHandle, consumed: len(maxEncoded)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h, n := DecodeHandle(tc.src)
			require.Equal(t, tc.want, h)
			require.Equal(t, tc.consumed, n)
		})
	}

	for i := range len(maxEncoded) {
		h, n := DecodeHandle(maxEncoded[:i])
		require.Equal(t, Handle{}, h, "prefix length %d", i)
		require.Equal(t, 0, n, "prefix length %d", i)
	}
}

func TestDecodeHandleWithProperties(t *testing.T) {
	got, err := DecodeHandleWithProperties(append([]byte{0x01}, overflowVarint...))
	require.ErrorContains(t, err, "invalid block.Handle")
	require.Equal(t, HandleWithProperties{}, got)

	h := HandleWithProperties{
		Handle: Handle{Offset: math.MaxUint64, Length: 42},
		Props:  []byte("props"),
	}
	got, err = DecodeHandleWithProperties(h.EncodeVarints(make([]byte, 2*binary.MaxVarintLen64)))
	require.NoError(t, err)
	require.Equal(t, h, got)
}
