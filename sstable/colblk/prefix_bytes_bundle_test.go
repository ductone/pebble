// Copyright 2026 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package colblk

import (
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/cockroachdb/crlib/crbytes"
	"github.com/cockroachdb/pebble/internal/binfmt"
	"github.com/cockroachdb/pebble/internal/treeprinter"
	"github.com/stretchr/testify/require"
)

// bundleBoundaryKeys are sorted keys whose runs of duplicates cross bundle
// boundaries for bundle sizes 1, 2 and 16.
var bundleBoundaryKeys = []string{
	"apple", "apple", "apricot", "banana", "banana", "banana", "banana", "banana",
	"band", "bandana", "cherry", "cherry", "cherry", "cherry", "cherry", "cherry",
	"cherry", "date", "date",
}

// buildPrefixBytes encodes keys with the given bundle size and finishes the
// first rows of them. The buffer has one trailing byte, standing in for a
// columnar block's padding byte; the column ends at len(buf)-1.
func buildPrefixBytes(t *testing.T, bundleSize int, keys []string, rows int) []byte {
	t.Helper()
	var b PrefixBytesBuilder
	b.Init(bundleSize)
	for i, k := range keys {
		shared := 0
		if i > 0 {
			shared = crbytes.CommonPrefix([]byte(keys[i-1]), []byte(k))
		}
		b.Put([]byte(k), shared)
	}
	size := b.Size(rows, 0)
	buf := make([]byte, size+1)
	require.Equal(t, size, b.Finish(0, rows, 0, buf))
	return buf
}

// requirePrefixBytesKeys checks every accessor of pb against keys, including
// SetAt followed by SetNext to the last row from every row.
func requirePrefixBytesKeys(t *testing.T, pb *PrefixBytes, bundleSize uint64, keys []string) {
	t.Helper()
	n := len(keys)
	require.Equal(t, n, pb.Rows())
	require.Equal(t, 1+uint64(n-1)/bundleSize, uint64(pb.BundleCount()))
	require.Equal(t, keys[0], string(pb.UnsafeFirstSlice()))
	var it PrefixBytesIter
	it.Init(len(slices.MaxFunc(keys, func(a, b string) int { return len(a) - len(b) })), nil)
	for i := range n {
		if got := string(pb.At(i)); got != keys[i] {
			t.Fatalf("At(%d) = %q, want %q", i, got, keys[i])
		}
		pb.SetAt(&it, i)
		for j := i; j < n; j++ {
			if j > i {
				pb.SetNext(&it)
			}
			if got := string(it.Buf); got != keys[j] {
				t.Fatalf("SetAt(%d) then SetNext to row %d = %q, want %q", i, j, got, keys[j])
			}
		}
	}
	// Search returns the first row whose key is >= the search key.
	probes := []string{"", "zzz"}
	for _, k := range keys {
		probes = append(probes, k, k+"\x00", k[:len(k)-1])
	}
	for _, k := range probes {
		wantIdx := sort.SearchStrings(keys, k)
		wantEq := wantIdx < n && keys[wantIdx] == k
		if idx, eq := pb.Search([]byte(k)); idx != wantIdx || eq != wantEq {
			t.Fatalf("Search(%q) = (%d, %t), want (%d, %t)", k, idx, eq, wantIdx, wantEq)
		}
	}
}

func requirePrefixBytesPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic containing %q", want)
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, want) {
			t.Fatalf("panic %q does not contain %q", msg, want)
		}
	}()
	fn()
}

// TestPrefixBytesWriterBundleSizes round-trips writer output with single, full
// and partial bundles. On 64-bit platforms PrefixBytesBuilder.Init encodes a
// bundle size of 1<<32 as shift 32.
func TestPrefixBytesWriterBundleSizes(t *testing.T) {
	bundleSizes := []int{1, 2, 16}
	if bits.UintSize == 64 {
		for _, shift := range []uint{31, 32} {
			bundleSizes = append(bundleSizes, 1<<shift)
		}
	}
	for _, bundleSize := range bundleSizes {
		t.Run(fmt.Sprintf("bundle=%d", bundleSize), func(t *testing.T) {
			shift := bits.TrailingZeros64(uint64(bundleSize))
			for n := 1; n <= len(bundleBoundaryKeys); n++ {
				for _, rows := range []int{n, n - 1} {
					if rows == 0 {
						continue
					}
					buf := buildPrefixBytes(t, bundleSize, bundleBoundaryKeys[:n], rows)
					require.Equal(t, byte(shift), buf[0])
					f := binfmt.New(buf)
					tp := treeprinter.New()
					prefixBytesToBinFormatter(f, tp.Child("prefix-bytes"), rows, nil)
					require.Contains(t, tp.String(), fmt.Sprintf("bundle size: %d\n", bundleSize))

					pb, endOffset := DecodePrefixBytes(buf, 0, uint32(rows))
					require.Equal(t, uint64(len(buf)-1), endOffset)
					requirePrefixBytesKeys(t, &pb, uint64(bundleSize), bundleBoundaryKeys[:rows])
				}
			}
		})
	}
}

// TestPrefixBytesDecodeBundleShift rewrites the shift byte of writer output
// that holds a single bundle. Any shift that keeps every row in one bundle
// leaves the layout unchanged: where the platform can build it, the result is
// byte-identical to writer output with that bundle size. On 32-bit platforms
// this decodes the shift 31 and 32 columns only a 64-bit writer produces.
// Shifts above 32 are rejected.
func TestPrefixBytesDecodeBundleShift(t *testing.T) {
	for _, rows := range []int{1, 3, 16} {
		keys := bundleBoundaryKeys[:rows]
		buf := buildPrefixBytes(t, 16, keys, rows)
		minShift := bits.Len32(uint32(rows - 1))
		for _, shift := range []int{minShift, 30, 31, 32} {
			t.Run(fmt.Sprintf("rows=%d/shift=%d", rows, shift), func(t *testing.T) {
				mutated := slices.Clone(buf)
				mutated[0] = byte(shift)
				if bits.UintSize == 64 || shift < 31 {
					require.Equal(t, buildPrefixBytes(t, 1<<shift, keys, rows), mutated)
				}
				pb, endOffset := DecodePrefixBytes(mutated, 0, uint32(rows))
				require.Equal(t, uint64(len(buf)-1), endOffset)
				requirePrefixBytesKeys(t, &pb, uint64(1)<<shift, keys)
			})
		}
		for _, shift := range []int{33, 63, 64, 255} {
			t.Run(fmt.Sprintf("rows=%d/shift=%d", rows, shift), func(t *testing.T) {
				mutated := slices.Clone(buf)
				mutated[0] = byte(shift)
				requirePrefixBytesPanic(t, "invalid PrefixBytes bundle shift", func() {
					DecodePrefixBytes(mutated, 0, uint32(rows))
				})
			})
		}
	}
}

// TestPrefixBytesDecodeLogicalCount checks slice-count limits and the offset
// indexes derived for the first and last bundles. A zero-width offset table
// encodes any number of zero offsets in no bytes, so these counts need no
// large allocation.
func TestPrefixBytesDecodeLogicalCount(t *testing.T) {
	const overflow = "overflow in PrefixBytes slice count"
	for _, tc := range []struct {
		count uint32
		shift uint8
		// wantPanic applies on all platforms, wantPanic32 only where an int
		// cannot index every slice.
		wantPanic   string
		wantPanic32 string
	}{
		{count: math.MaxUint32, shift: 0, wantPanic: overflow},
		{count: 1 << 31, shift: 0, wantPanic: overflow},
		{count: 1<<31 - 1, shift: 0, wantPanic32: overflow},
		{count: math.MaxUint32 - 1, shift: 32, wantPanic: overflow},
		{count: math.MaxUint32 - 2, shift: 32, wantPanic32: overflow},
		{count: 1<<31 - 3, shift: 31},
		{count: 1<<31 - 3, shift: 32},
		{count: 1<<30 + 1, shift: 30},
		{count: 1<<20 + 1, shift: 4},
	} {
		t.Run(fmt.Sprintf("count=%d/shift=%d", tc.count, tc.shift), func(t *testing.T) {
			buf := []byte{tc.shift, byte(uintEncodingAllZero), 0}
			wantPanic := tc.wantPanic
			if wantPanic == "" && bits.UintSize == 32 {
				wantPanic = tc.wantPanic32
			}
			if wantPanic != "" {
				requirePrefixBytesPanic(t, wantPanic, func() { DecodePrefixBytes(buf, 0, tc.count) })
				return
			}
			pb, endOffset := DecodePrefixBytes(buf, 0, tc.count)
			require.Equal(t, uint64(2), endOffset)

			bundleSize := uint64(1) << tc.shift
			nBundles := 1 + uint64(tc.count-1)/bundleSize
			totalSlices := uint64(tc.count) + nBundles
			require.Equal(t, int(tc.count), pb.Rows())
			require.Equal(t, nBundles, uint64(pb.BundleCount()))
			require.Equal(t, totalSlices, uint64(pb.rawBytes.slices))

			var it PrefixBytesIter
			it.Init(0, nil)
			// The first row of a bundle, so that SetAt does not scan back
			// over the empty rows before it.
			for _, row := range []uint64{0, (nBundles - 1) * bundleSize} {
				q := row / bundleSize
				pb.SetAt(&it, int(row))
				require.Equal(t, 1+q+row, uint64(it.offsetIndex))
				if q+1 < nBundles {
					require.Equal(t, (q+1)*(bundleSize+1), uint64(it.nextBundleOffsetIndex))
				} else {
					require.GreaterOrEqual(t, uint64(it.nextBundleOffsetIndex), totalSlices)
				}
				if row+1 < uint64(tc.count) {
					pb.SetNext(&it)
					next := row + 1
					require.Equal(t, 1+next/bundleSize+next, uint64(it.offsetIndex))
				}
			}
			idx, eq := pb.Search(nil)
			require.Equal(t, 0, idx)
			require.True(t, eq)
		})
	}
}
