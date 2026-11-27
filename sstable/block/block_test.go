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

// overflowVarint is a 10-byte varint whose final byte exceeds the single bit
// remaining in a uint64; binary.Uvarint reports it as an overflow (n < 0).
var overflowVarint = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}

// tooLongVarint is an 11-byte varint whose continuation bits never terminate
// within binary.MaxVarintLen64 bytes; binary.Uvarint reports it as an
// overflow (n < 0).
var tooLongVarint = []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x00}

func concat(bufs ...[]byte) []byte {
	return bytes.Join(bufs, nil)
}

func encodeHandle(h Handle) []byte {
	buf := make([]byte, 2*binary.MaxVarintLen64)
	return buf[:h.EncodeVarints(buf)]
}

func TestDecodeHandle(t *testing.T) {
	maxHandle := Handle{Offset: math.MaxUint64, Length: 7}
	maxEncoded := encodeHandle(maxHandle)
	require.Len(t, maxEncoded, binary.MaxVarintLen64+1)

	testCases := []struct {
		name     string
		src      []byte
		want     Handle
		consumed int
	}{
		{name: "nil", src: nil},
		{name: "empty", src: []byte{}},
		{name: "truncated-first-single-byte", src: []byte{0x80}},
		{name: "truncated-first-multi-byte", src: []byte{0xff, 0xff, 0xff}},
		{name: "missing-second", src: []byte{0x01}},
		{name: "truncated-second", src: []byte{0x01, 0x80}},
		{name: "truncated-second-multi-byte", src: []byte{0x01, 0xff, 0xff}},
		{name: "overflow-first", src: concat(overflowVarint, []byte{0x01})},
		{name: "too-long-first", src: concat(tooLongVarint, []byte{0x01})},
		{name: "overflow-second", src: concat([]byte{0x01}, overflowVarint)},
		{name: "too-long-second", src: concat([]byte{0x01}, tooLongVarint)},
		{name: "overflow-second-trailing", src: concat([]byte{0x01}, overflowVarint, []byte{0x01, 0x02})},
		{
			name:     "valid-small",
			src:      []byte{0x05, 0x06},
			want:     Handle{Offset: 5, Length: 6},
			consumed: 2,
		},
		{
			name:     "valid-trailing-bytes-not-consumed",
			src:      []byte{0x05, 0x06, 0xff, 0xff},
			want:     Handle{Offset: 5, Length: 6},
			consumed: 2,
		},
		{
			name:     "valid-max-uint64-offset",
			src:      maxEncoded,
			want:     maxHandle,
			consumed: len(maxEncoded),
		},
		{
			name:     "valid-max-uint64-both",
			src:      encodeHandle(Handle{Offset: math.MaxUint64, Length: math.MaxUint64}),
			want:     Handle{Offset: math.MaxUint64, Length: math.MaxUint64},
			consumed: 2 * binary.MaxVarintLen64,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Decode twice to verify the function is pure and repeatable.
			for range 2 {
				var h Handle
				var n int
				require.NotPanics(t, func() { h, n = DecodeHandle(tc.src) })
				require.Equal(t, tc.want, h)
				require.Equal(t, tc.consumed, n)
			}
		})
	}
}

func TestDecodeHandleRoundTrip(t *testing.T) {
	values := []uint64{0, 1, 127, 128, 1 << 32, math.MaxUint64 - 1, math.MaxUint64}
	for _, off := range values {
		for _, length := range values {
			h := Handle{Offset: off, Length: length}
			enc := encodeHandle(h)
			got, n := DecodeHandle(enc)
			require.Equal(t, h, got)
			require.Equal(t, len(enc), n)
			// Every strict prefix of a valid encoding is truncated and must be
			// rejected without panicking.
			for i := range len(enc) {
				got, n := DecodeHandle(enc[:i])
				require.Equal(t, Handle{}, got, "prefix %x", enc[:i])
				require.Equal(t, 0, n, "prefix %x", enc[:i])
			}
		}
	}
}

// checkDecodeHandleInvariants verifies that DecodeHandle never panics, that
// invalid input yields a zero handle and zero consumed bytes, and that valid
// input reports a consumed length within src that decodes identically.
func checkDecodeHandleInvariants(t *testing.T, src []byte) {
	t.Helper()
	var h Handle
	var n int
	require.NotPanics(t, func() { h, n = DecodeHandle(src) }, "src %x", src)
	if n == 0 {
		require.Equal(t, Handle{}, h, "src %x", src)
		return
	}
	require.Greater(t, n, 0, "src %x", src)
	require.LessOrEqual(t, n, len(src), "src %x", src)
	h2, n2 := DecodeHandle(src[:n])
	require.Equal(t, h, h2, "src %x", src)
	require.Equal(t, n, n2, "src %x", src)
}

// TestDecodeHandleExhaustiveShortInputs exercises every input of up to two
// bytes, plus every prefix and suffix-extension of overflowing encodings.
func TestDecodeHandleExhaustiveShortInputs(t *testing.T) {
	checkDecodeHandleInvariants(t, nil)
	for a := range 256 {
		checkDecodeHandleInvariants(t, []byte{byte(a)})
		for b := range 256 {
			checkDecodeHandleInvariants(t, []byte{byte(a), byte(b)})
		}
	}
	for _, bad := range [][]byte{overflowVarint, tooLongVarint} {
		for i := range len(bad) + 1 {
			for b := range 256 {
				// Overflowing varint in the first position, followed by an
				// arbitrary byte.
				checkDecodeHandleInvariants(t, concat(bad[:i], []byte{byte(b)}))
				// Valid first varint followed by a (possibly overflowing)
				// second varint.
				checkDecodeHandleInvariants(t, concat([]byte{byte(b) & 0x7f}, bad[:i]))
			}
		}
	}
}

func TestDecodeHandleWithProperties(t *testing.T) {
	invalid := [][]byte{
		nil,
		{0x80},
		{0x01},
		{0x01, 0x80},
		concat(overflowVarint, []byte{0x01}),
		concat([]byte{0x01}, overflowVarint),
		concat([]byte{0x01}, tooLongVarint),
	}
	for _, src := range invalid {
		var got HandleWithProperties
		var err error
		require.NotPanics(t, func() { got, err = DecodeHandleWithProperties(src) }, "src %x", src)
		require.Error(t, err, "src %x", src)
		require.Contains(t, err.Error(), "invalid block.Handle")
		require.Equal(t, HandleWithProperties{}, got, "src %x", src)
	}

	h := HandleWithProperties{
		Handle: Handle{Offset: math.MaxUint64, Length: 42},
		Props:  []byte("props"),
	}
	enc := h.EncodeVarints(make([]byte, 2*binary.MaxVarintLen64))
	got, err := DecodeHandleWithProperties(enc)
	require.NoError(t, err)
	require.Equal(t, h, got)

	// A valid handle without properties decodes with empty properties.
	got, err = DecodeHandleWithProperties([]byte{0x05, 0x06})
	require.NoError(t, err)
	require.Equal(t, Handle{Offset: 5, Length: 6}, got.Handle)
	require.Empty(t, got.Props)
}
