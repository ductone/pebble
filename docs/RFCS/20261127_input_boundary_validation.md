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
[`BlockDecoder.Init`](../../sstable/colblk/block.go) checks the page-header
layout, `DecodeColumn` confines a decoder to its page, and the individual
column decoders construct views. [`sstable.Reader`](../../sstable/reader.go)
dispatches to metadata initializers on block reads. The block reader checks
checksums, decompresses, initializes metadata, and only then publishes a cache
value; cache hits reuse that initial work. These are useful existing boundaries,
not proof that every dependent field and allocation is already bounded.

The [current columnar hardening topic](https://github.com/ductone/pebble/tree/paul.querna/validate-columnar-block-bounds)
illustrates the gap: physical extents, offsets, bitmap summaries, keyspan
indexes, and key materialization each needed a different check. Its
writer-produced excluded-key case also demonstrates why a seemingly obvious
rule (`recorded maximum key length <= block length`) can reject valid blocks.
The topic is a near-term fix, **not** evidence that every format is safe. Its
v2.1.7-based [integration line](https://github.com/ductone/pebble/tree/c1-integration-v2)
exists for a consumer pinned to v2; this RFC targets upstream master first.

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
  `size <= len(buf)-start` before computing `end` or narrowing to `int` or
  `uint32`. Include alignment padding, the final addressable padding byte,
  page boundaries and complete element widths. Avoid checking after a
  wrapped addition, or after forming a raw pointer. A page accessor may only
  address its own validated page (plus explicitly specified padding).
- **Logical extent.** Check row/count conversions and `count+1` before use.
  If width is positive, `count <= available/width` bounds the physical
  column and makes a once-per-row validation scan proportional to encoded
  bytes. If width is zero, there are no per-row bytes: validate the constant
  once, or derive an invariant without looping over the logical count.
- **Cross-column extent.** Validate offset monotonicity, summary-bit targets,
  bundle/prefix relationships and keyspan boundary indices against the
  columns they will index before exposing their unchecked accessors. A final
  offset does not establish that intermediate offsets are ordered.
- **Output extent.** Before unsafe stores, slice the *actual destination*
  to the required size, including schema-specific and synthetic bytes. A
  format header that gives only an upper estimate can bound an allocation
  after normalization; it cannot justify a store beyond the allocated buffer.
  In particular, a too-small recorded value must not make a materializer
  write before a capacity check.
- **Lifetime and mutability.** Proof is invalid if the underlying bytes can
  change after validation, or if a cache/buffer handle is released while an
  accessor retains its pointer. Callers either hold the buffer's lifetime and
  enforce immutability or revalidate after mutation. An exported wrapper
  around an arbitrary mutable slice is not a safety proof.

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
`int` maximum. Special cases such as an allowed one-past-end padding address
must be explicit at the caller rather than hidden in this generic helper.

Make validated-view construction the only path that can populate an unsafe
accessor. Keep `UnsafeUints.At`, `RawBytes.At` and other proven accessors
unchecked in the hot loop. If a direct constructor remains necessary for a
writer or a test, identify it and state its separate precondition. Review
`uintptr` pointer arithmetic and `unsafe.Slice` construction at those
constructors: do not create a pointer outside an allocation and assume a later
slice will catch it. The view is meaningful only while its block handle is
retained and its bytes are immutable.

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
| 1. Map proofs | Record a boundary matrix for each column family, subformat and unsafe accessor: encoded fields, trust transition, view lifetime, dependent indexes, output sizes, and negative/valid-edge fixtures. Add a mechanical inventory check or narrow review checklist for new unchecked accessors; no runtime behavior change. | Start with `sstable/colblk`; inventory row blocks, value/blob blocks, filter/index/footer/handles, WAL, MANIFEST, batches and external ingest before claiming engine-wide coverage. |
| 2. Arithmetic primitives | Add tested, allocation-free checked ranges/counts and migrate one physical column family at a time. Keep exact end-offset checks and zero-width special cases. | Once per decoded page, not per `At`. |
| 3. View and schema contracts | Make it difficult to construct an unchecked view without the proved page extent; document cache/handle lifetime, mutable aliases and `KeySchema` expansion/store rules. Migrate data, index, keyspan and value/liveness subformats in reviewable PRs. | Metadata init before cache publication and any unchecked iterator use. |
| 4. Adjacent formats | Apply the same proof obligations, not necessarily the same types, to row SST blocks, footer/handles and decompression, value/blob, WAL/MANIFEST and batch inputs. One format per PR with existing error/compatibility behavior preserved. | At that format's first read/decoder boundary. |
| 5. Resource policy | Investigate decompressed sizes, logical row/span materialization and large block handles separately. Adopt limits only with writer compatibility evidence and an explicit operator-facing policy. | Before expensive allocation; independent from memory-safety proof. |

The stack is ordered for review, not a mandate to land every later slice before
shipping slice 0. Upstream master is the source of truth. Backport only accepted,
relevant slices to the v2 consumer line while it exists; track fork maintenance
and exit separately. Never turn a code review or CI result into automatic
merge authority.

# Validation and performance plan

For each slice, record the source base and a small matrix of **valid writer
output**, **historical output**, **one-field mutations** and **compressed edge
cases**. Each unsafe write regression needs a bounded-capacity canary when a
post-write panic would otherwise make the test vacuous. For unsafe reads,
exercise the public/metadata path as well as the narrow decoder path, and
avoid emitting bytes from outside the fixture into test logs. Retain a
reproducible offline corpus of real malformed blocks without credentials or
external calls.

- Deterministic tests: min/max offset, overflow at each width, positive-width
  truncated pages, zero-width constants and empty columns, nonmonotone
  intermediate offsets, cross-column indexes, valid excluded keys and
  exact-fit destination buffers. Check legacy writer compatibility, not just
  malformed-input rejection. Test 32-bit casts where supported.
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
  seek/scan benches. Add a constant-width huge-count decode benchmark and a
  representative ordinary block-read benchmark if the current suite cannot
  isolate validation cost. Report ns/op, B/op, allocs/op, throughput and cache
  behavior; specify sample size and noise before deciding a regression.
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
