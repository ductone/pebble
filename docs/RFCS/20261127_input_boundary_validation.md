- Feature Name: Input-boundary validation for Pebble's encoded formats
- Status: draft for discussion
- Start Date: 2026-11-27
- Authors: TBD
- RFC PR: TBD
- Pebble Issues: TBD

# Summary

Give every unchecked read or write of encoded data a nearby, reviewable proof that
its address and length are inside an owned, still-live buffer. Validate once when
bytes become a decoded view, then keep iteration and seeking fast. The proof must
cover arithmetic overflow, column/page extents, dependent indexes, mutable
buffers, and materialized output capacity; a checksum or a writer's usual output
is not a substitute. Corrupt bytes may produce a corruption error or an
assertion, but must never cause an out-of-bounds read or write.

This is a *series* of small changes, not a wholesale replacement of Pebble's
columnar decoding or an on-disk format change. It separates memory safety from
resource-exhaustion policy and keeps performance evidence alongside each step.

# Motivation and scope

Pebble reads SSTables produced by its own writers, older writers, external
storage and ingestion, and bytes whose checksum may be valid despite malformed
contents. Some decoded columns deliberately use unchecked pointer arithmetic
for speed. Their correctness currently depends on several distributed facts:
page offsets in the common header, each column's physical width, byte-offset
ordering, relationships between columns, and the capacity of synthesized keys.
A check at only one of those layers does not establish the others.

At the source snapshot used for this draft (`cockroachdb/pebble` master
`f546ea1473b6003e1269b145a31d9506c586ff3e`),
[`BlockDecoder.Init`](../../sstable/colblk/block.go) stores the block and
decodes its fixed header; `DecodeColumn` passes the whole block to a column
decoder and compares its returned end with the next page start *afterward*.
Neither operation proves page confinement before the callback runs.
[`sstable.Reader`](../../sstable/reader.go) initializes metadata for the main
data/index/keyspan paths on a block read before caching it; optional tiering
columns decode lazily from either directly borrowed bytes or a retained block
handle. Inner payloads (for example tiered histogram values) have their own
validation boundary. These are distinct proof owners, not a single
once-before-cache guarantee for all subformats.

The [columnar hardening topic at `6df164f0`](https://github.com/ductone/pebble/commit/6df164f0a095144dcaf9924a61bbc6d635bdfab3)
adds earlier page-header and column bounds checks, among other repairs; do
not attribute those checks to the pinned upstream-master baseline. Its
writer-produced excluded-key case shows why a seemingly obvious rule
(`recorded maximum key length <= block length`) can reject valid blocks.
The topic is a near-term fix, **not** evidence that every format is safe. Its
v2.1.7-based [integration line](https://github.com/ductone/pebble/tree/c1-integration-v2)
exists for a consumer pinned to v2; this RFC targets upstream master first.

## Initial proof matrix: unsigned columns

This matrix describes the pinned master baseline, not the separate hotfix.
Each implementation PR should extend it only for the consumers it migrates.

| Boundary and immediate caller | Baseline at `f546ea14` | Proposed proof owner |
| --- | --- | --- |
| [`BlockDecoder.Init`](../../sstable/colblk/block.go), called by data/index/keyspan metadata initializers or a direct decoder | Stores a slice and decodes the fixed header; it does not validate all page starts. | Check header size, version, column count and monotone page starts once, preserving alignment to the original block. |
| `DecodeColumn` calling `DecodeUnsafeUints` | Passes the whole block, then compares the returned offset with the next page. A decoder can construct an unsafe view before that comparison. | Pass a proved page extent to the decoder, allow only an explicitly non-dereferenced sentinel byte, and require an exact decoded end. |
| [`DecodeUnsafeUints`](../../sstable/colblk/unsafe_uints.go) constructing its pointer | A Go bounds check for `&b[off]` does not prove `rows * width` bytes are present; a delta base and alignment also advance the cursor. | Prove base/alignment/width and whole physical span before pointer construction; width zero checks the constant once and needs no row scan. |
| `UnsafeUints.At` and `UnsafeOffsets.At2`, reached by seeks/iteration and dependent columns | Indexing uses unchecked pointer arithmetic; a valid encoded span alone does not prove caller indexes. | Owning iterator or dependent decoder establishes row/adjacent-index range; pin an immutable block while the view remains live. |

The metadata callbacks recover initialization panics as corruption, but they
cannot undo an unsafe access. An optional lazily decoded column has its own
first-use proof even when the surrounding block was cached successfully.

Goals:

1. For every decoder path in scope, reject or safely panic on malformed
   extents *before* forming or dereferencing an unsafe pointer or slice.
2. Express each proof once at the boundary where bytes become a reusable view;
   do not add a bounds check to every `At`, `Next`, or `Seek` hot path.
3. Preserve all writer-produced and supported historical encodings, including
   zero-width constant columns, legitimate empty columns, and compressed
   logical row counts disproportionate to encoded bytes.
4. Make review tractable: a small vocabulary for checked arithmetic and
   validated views, explicit ownership of each invariant, and tests showing
   both accepted edge cases and rejected malformed ones.
5. Measure CPU, allocations and throughput against a fixed base for every
   migration slice. No performance claim without the corresponding result.

Non-goals: changing the SSTable/WAL format, treating checksums as authenticity,
a blanket `rows <= block bytes` rule, removing all `unsafe`, or promising that
arbitrary compressed data has a universal cheap materialization bound.

# Boundary contract

A format review records four distinct quantities for each field: its encoded
byte extent, its logical count, the buffer an accessor may address, and any
heap object materialized from it. The following rules apply to a *validated
view*, not to an arbitrary `[]byte`:

- **Physical extent.** Validate `start <= len(buf)` and
  `size <= len(buf)-start` before computing an exclusive `end` or narrowing
  to `int` or `uint32`. Prove the complete page and element extents, including
  alignment relative to the original block. The page's exclusive end may be
  used to form an empty Go slice, but `&buf[end]` needs a *real addressable
  byte*; an adjacent-page or trailing byte borrowed solely as a sentinel
  must never be dereferenced as page content. Avoid forming an unsafe pointer
  before its address is proved to lie within the underlying allocation.
- **Logical extent.** Check row/count conversions and `count+1` before use.
  If width is positive, `count <= available/width` bounds the physical
  column and makes any necessary once-per-row scan proportional to encoded
  bytes. If width is zero, there are no per-row bytes: validate the constant
  once, including a delta base, without looping over the logical count.
  Construction does not prove a subsequent caller-supplied index: `At(row)`
  requires `0 <= row < count`; `At2` and other adjacent reads also require
  `row+1 < encodedCount`, without overflowing the index expression.
- **Cross-column extent.** Validate offset monotonicity, summary-bit targets,
  bundle/prefix relationships and keyspan boundary indices against the
  columns they will index before exposing their unchecked accessors. A final
  offset does not establish that intermediate offsets are ordered.
- **Output extent.** Check the sum of all component lengths *without
  narrowing or overflow*, then slice the actual destination to the required
  size before built-in unsafe stores, including synthetic bytes. A header
  estimate can bound an allocation after normalization; it cannot justify
  a write beyond the allocated buffer. Existing registered `KeySchema`
  implementations are trusted code processing untrusted fields, with a
  public contract that the caller supplies enough capacity for the key and
  synthetic-suffix replacement. A generic view cannot certify arbitrary
  third-party unsafe code; preserve that capacity promise for old schemas
  while enforcing built-in materializers' actual-write bounds.
- **Lifetime and mutability.** A directly initialized decoder borrows
  immutable bytes until its last view is discarded; a handle-backed iterator
  pins the buffer until replacement or `Close`. A handle prevents recycling,
  *not* writes through another alias. Require exclusive ownership or an
  immutable borrow at entry; if that cannot be enforced, copy at the affected
  boundary. Mutation requires exclusive access and invalidation of every old
  view before revalidation, never concurrent aliasing. Do not store a pointer
  to a separately allocated descriptor in opaque block metadata: the GC
  cannot trace such a pointer. Enlarging block metadata may change cache
  allocation size classes even if allocs/op stays constant.

Checksums and decompression precede decoding but do not establish semantic
validity. Do not turn ordinary `panic` recovery into the safety mechanism:
the proofs must precede unsafe operations. At the reader boundary, convert
recoverable decoder assertions to corruption errors with file/block context
where the existing API supports it. A Go bounds panic is preferable to an
unsafe memory access; it is still a usability defect when it escapes a public
read API and should be measured separately.

## Proposed factoring, without per-row overhead

Start with a **package-local, allocation-free checked-range vocabulary**, not
a new public decoding framework. One helper checks an untrusted `(off, size)`
against a buffer length and returns `int` endpoints only after proving the
conversion fits. Another checks element count against width and remaining
bytes, with an explicit width-zero branch. Make the failure text name the
field and page. Keep the resulting bounds in existing decoder state, or in a
small unexported page descriptor when it avoids duplicate checks; benchmark
any descriptor allocation or extra header load before choosing a shape.

For example (illustrative API, **not** a prescriptive signature):

```go
func checkedRange(bufLen, off, size uint64) (start, end int, err error) {
    if off > bufLen || size > bufLen-off || off+size > uint64(maxInt) {
        return 0, 0, errCorruptExtent
    }
    return int(off), int(off + size), nil
}
```

The implementation should use the repository's error style and avoid interface
boxing or formatting on success. `maxInt` here means the target architecture's
`int` maximum. A `start == len(buf)` result is a valid empty slice, not a
license to take `&buf[start]`; an actual sentinel byte and alignment measured
from the original block are explicit caller obligations.

Make validated-view construction the only path that can populate an unsafe
accessor. Keep `UnsafeUints.At`, `RawBytes.At` and other proven accessors
unchecked in the hot loop; document the row-index preconditions for callers.
If a direct constructor remains necessary for a writer or a test, identify
its separate precondition. Review `uintptr` pointer arithmetic and
`unsafe.Slice` construction at those constructors: do not create a pointer
outside an allocation and assume a later slice will catch it. Validate
optional lazily decoded columns before their first use, and nested payloads
at their own decoder; initial metadata validation cannot prove those bytes.

The first implementation PR should prove this factoring on one column family
and measure it. It should *not* migrate every format in the same commit. If a
single generic abstraction costs more or obscures the format-specific proof,
keep the small arithmetic helpers and explicit local checks instead.

## Materialization and resource policy

Memory safety and bounded resource consumption are related but distinct.
A compressed constant column can represent many rows in few bytes; a
writer-compatible decoder cannot reject it merely because `rows > len(block)`.
Initialization should not spend CPU proportional to an unencoded logical count.
Actual iteration over requested rows necessarily does work, and an allocation
sized from a logical span length may still exhaust memory without addressing
outside an allocation. The same distinction applies to a decompressor-reported
output size and block-handle-derived read size.

Inventory every allocation driven by encoded fields and decide whether its
bound comes from a verified byte extent, a schema-specific expansion bound, or
an explicit, documented resource policy. For policies that could reject valid
historical blocks, collect writer-produced and real-workload distributions and
propose the limit separately, with a compatibility and operator-error story.
Do not silently smuggle an arbitrary fixed maximum into `checkedRange`.

# Proposed PR stack

| Slice | Change and acceptance | Where checks run |
| --- | --- | --- |
| 0. Near-term baseline | Land/review the existing columnar bounds and materializer repairs separately; preserve their valid-writer and canary regressions. | Current topic and v2 integration line; no format change. |
| 1. Map the first proof | Correct the pinned baseline, document a compact boundary matrix for one column family and its immediate callers (physical extent, row/index ownership, borrow lifetime, output). Record direct and handle-backed call paths; widen the inventory in follow-up PRs. | Design/review only; no runtime behavior change. |
| 2. Prove a physical column | Add tested, allocation-free checked ranges/counts and migrate the common header and one physical unsigned-integer family on an explicit baseline (reuse the topic if accepted). Cover empty/adjacent pages, real final padding, alignment, integer casts, width-zero delta bases, and checked `count+1`. | Once per decoded page, not per `At`; lazy columns validated before first use. |
| 3. Prove one built-in output | Give a built-in schema/iterator an actual-destination check before unsafe writes. Test valid excluded keys, an expanding schema fixture, synthetic suffix and exact-fit/canary output. Preserve registered schemas' capacity contract; defer new public interfaces. | At the materializer's output boundary, with scratch state per iterator. |
| 4. Extend the inventory | Migrate data, index, keyspan, key/value and liveness columns, then row SST blocks, footer/handles and decompression, value/blob, WAL/MANIFEST and batch inputs one format per PR. Include nested payloads and direct entry points; apply the same obligations, not necessarily the same types. A narrow review checklist may precede a mechanical checker, but no global unsafe-site lint is a prerequisite. | At each format's first read, optional lazy decode or nested decoder boundary. |
| 5. Resource policy | Investigate decompressed sizes, logical row/span materialization and large block handles separately. Adopt limits only with writer compatibility evidence and an explicit operator-facing policy. | Before expensive allocation; independent from memory-safety proof. |

The stack is ordered for review, not a mandate to land every later slice before
shipping slice 0. Upstream master is the source of truth. Backport only accepted,
relevant slices to the v2 consumer line while it exists; track fork maintenance
and exit separately. Never turn a code review or CI result into automatic
merge authority.

# Validation and performance plan

For each slice, record the source base and a small matrix of **valid writer
output**, **historical output**, **one-field mutations** and **compressed edge
cases**. Distinguish direct-constructor rejection, a contextual reader error,
and a writer round trip. For accepted fixtures compare decoded row values with
a safe reference; after metadata success exercise row and seek operations,
synthetic transforms and optional lazy columns. Each unsafe write regression
needs a bounded-capacity canary when a post-write panic would otherwise make
the test vacuous. Avoid emitting bytes from outside the fixture into test logs.
Retain a reproducible offline corpus of real malformed blocks without
credentials or external calls.

- Deterministic tests: min/max offset, overflow at each width, positive-width
  truncated pages, zero-width delta base and empty columns, nonmonotone
  intermediate offsets, cross-column indexes, valid excluded keys, an
  expanding registered schema and exact-fit destination buffers. Exercise
  direct, handle-backed and optional-tiering-first-use paths. Check legacy
  writer compatibility, not just malformed-input rejection. Test 32-bit
  casts where supported.
- Fuzz separately by format: generic columns, data (including custom
  `KeySchema`), index, keyspan, key/value and liveness, row blocks, value/blob,
  WAL, MANIFEST and batch as their slices land. Use writer-produced seeds and
  targeted mutations of headers, widths, offsets, counts and dependent fields.
  Bound harness inputs/iterations to avoid a fuzz-time resource denial. Go
  `checkptr=2`, invariants, and canaries are complementary; accepting a Go
  bounds panic is **not** proof that an unchecked pointer was safe.
- Benchmark against a pinned base on the same runner, repeated interleaved
  runs with `benchstat`, `-benchmem` and CPU profiles on meaningful changes.
  Start with existing `BenchmarkDataBlockDecoderInit`,
  `BenchmarkDataBlockIter` (seek/next/prev), `BenchmarkRawBytes`,
  `BenchmarkUnsafeUints`, `BenchmarkKeyspanBlock_RangeDeletions` and SST reader
  seek/scan benches. Measure cold validation, repeated warm-handle Init,
  optional tiering first use and steady-state iteration both with and without
  transforms. Hold encoded width-zero bytes fixed while varying the logical
  count; include an ordinary block read for validation overhead. Report ns/op,
  B/op, allocs/op, throughput and cached bytes/block or allocator size class;
  specify sample size and noise before deciding a regression.
- Performance acceptance: no added allocation on steady-state decoding or
  iteration, and no per-row check introduced solely to establish a proof that
  can be made once. Treat a reproducible hot-path slowdown as a design
  checkpoint, not a fixed percent to waive by fiat. Prefer removing repeated
  validation work (e.g. one check for a constant encoding) to offset new
  one-time checks. Validate representative end-to-end workload throughput
  before claiming improvement.

# Compatibility, rollout, and alternatives

No on-disk version bump is proposed. Checks may reject malformed blocks sooner;
existing writer-produced and supported old files must still decode. Stage each
implementation PR with its writer-compatibility tests, benchmark deltas and
recorded error-surface changes. Keep the consumer fork pin reproducible; sync
only the landed upstream fixes needed by that consumer, and do not confuse
technical CI with approval, merge or a deployed runtime exercise.

Alternatives considered:

- **Only fuzz and patch each crash:** useful discovery, but leaves proofs and
  ownership scattered and permits the same class of mistake elsewhere.
- **Use safe slice indexing for every accessor:** straightforward but may make
  the hottest iteration path slower; retain it as a fallback when a one-time
  proof cannot be made convincingly.
- **Treat checksummed or writer-produced bytes as trusted:** checksums detect
  accidental changes, not format validity; valid writer output has edge cases
  that simplified assumptions can reject.
- **Clamp every count/size to compressed bytes:** rejects valid zero-width
  encodings and synthetic-key cases; distinguish physical extent from logical
  output and allocation policy.
- **Recover all panics:** cannot undo an unsafe access and can obscure bugs;
  recover only at known error boundaries after the memory-safety invariant is
  established.

# Open questions for reviewers

1. What minimum internal API shape makes validated view construction obvious
   without allocating a descriptor per column or adding work to hot accessors?
2. How do we express third-party `KeySchema` capacity and expansion obligations
   without assuming all keys are bounded by block bytes?
3. Which buffer ownership paths (cache, mmap, externally constructed readers)
   can mutate bytes after metadata initialization or retain a view past release?
4. Which format-derived allocations need an independent resource policy, and
   what historical/writer corpus would justify a limit without false positives?
5. Which benchmarks and machine setup are sufficiently stable to gate each
   slice, and what low-overhead inventory check would catch a new unchecked
   accessor lacking a proof?
