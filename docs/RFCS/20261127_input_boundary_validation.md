- Feature Name: Input-boundary validation for Pebble's encoded formats
- Status: draft for discussion
- Start Date: 2026-11-27
- Authors: TBD
- RFC PR: TBD
- Pebble Issues: TBD

# Summary

Every unchecked access to encoded bytes needs a proof of its physical extent,
logical index and buffer lifetime before the access. Use the cheapest proof at
the owning decoder: eagerly for a small fixed header, lazily for optional
payloads, or at an access point when scanning first would cost more. Reuse the
proof rather than adding bounds checks to every hot-path operation.

On invalid input, fail at the first proved boundary through the existing
corruption or assertion path (which may terminate the engine); never expose
a partial view or read beyond its buffer. This stack addresses malformed
encodings and unsafe access, not the size of otherwise valid data. It uses
Pebble's `Init`/`Decode` structure without changing the on-disk format or
introducing a general decoder framework.

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

As prior art, the [fork's columnar hardening topic at `6df164f0`](https://github.com/ductone/pebble/commit/6df164f0a095144dcaf9924a61bbc6d635bdfab3)
adds earlier page-header and column bounds checks, plus materialization
repairs; none is in the pinned upstream master. A writer-produced excluded-key
case shows why `recorded maximum key length <= block length` can reject valid
blocks. The topic is a near-term fix, **not** proof that every format is safe.
Its v2.1.7-based [consumer integration line](https://github.com/ductone/pebble/tree/c1-integration-v2)
is separate from the upstream-master PR sequence below: that port must account
for master's `offset uint64, rows uint32` decoder signatures.

Merged changes illustrate a local style, not agreement on a new policy:
[#5856](https://github.com/cockroachdb/pebble/pull/5856) widened decoder
offsets and added two production bounds checks;
[#4664](https://github.com/cockroachdb/pebble/pull/4664) added
invariants-only caller checks; [#5093](https://github.com/cockroachdb/pebble/pull/5093)
removed a pointer to save per-block metadata; and
[#4346](https://github.com/cockroachdb/pebble/pull/4346) improved
corruption reporting with file context. Invariants checks catch programmer
misuse, but do not prove encoded extents in production. The proposed checks
extend the existing decoder idiom without changing global reporting.

## First proof matrix: unsigned columns

This matrix describes the pinned master baseline, not the separate hotfix.
Each implementation PR should extend it only for the consumers it migrates.

| Boundary and immediate caller | Baseline at `f546ea14` | Proposed proof owner |
| --- | --- | --- |
| [`BlockDecoder.Init`](../../sstable/colblk/block.go), called by data/index/keyspan metadata initializers or a direct decoder | Stores a slice and decodes the fixed header; it does not validate all page starts. | Check header size, version, column count and monotone page starts once, preserving alignment to the original block. |
| `DecodeColumn` calling `DecodeUnsafeUints` | Passes the whole block, then compares the returned offset with the next page. A decoder can construct an unsafe view before that comparison. | Pass a proved page extent to the decoder, allow only an explicitly non-dereferenced sentinel byte, and require an exact decoded end. |
| [`DecodeUnsafeUints`](../../sstable/colblk/unsafe_uints.go) constructing its pointer | A Go bounds check for `&b[off]` does not prove `rows * width` bytes are present; a delta base and alignment also advance the cursor. | Prove base/alignment/width and whole physical span before pointer construction; width zero checks the constant once and needs no row scan. |
| `UnsafeUints.At` and `UnsafeOffsets.At2`, reached by seeks/iteration and dependent columns | Indexing uses unchecked pointer arithmetic; a valid encoded span alone does not prove caller indexes. | Owning iterator or dependent decoder establishes row/adjacent-index range; pin an immutable block while the view remains live. |
| `RawBytes` consuming `UnsafeOffsets` | It bounds only the final offset; intermediate offsets can be unordered, and accessors use unchecked addresses. | Scan relevant offsets once if justified, or bounds-check dependent consumers, including per-row value reads and key materialization; compare warm per-row cost with cold scan cost. Width-zero offsets need no scan. |

The metadata callbacks recover initialization panics as corruption, but they
cannot undo an unsafe access. An optional lazily decoded column has its own
first-use proof even when the surrounding block was cached successfully.

# Boundary contract

A format review records three distinct quantities for each field: its encoded
byte extent, its logical count, and the buffer an accessor may address.
Allocations for otherwise valid values are a separate concern. The following
rules apply to a *validated view*, not to an arbitrary `[]byte`:

- **Physical extent.** Validate `start <= len(buf)` and
  `size <= len(buf)-start` before computing an exclusive `end` or narrowing
  to `int` or `uint32`. Prove the complete page and element extents, including
  alignment relative to the original block. The page's exclusive end may be
  used to form an empty Go slice, but `&buf[end]` needs a *real addressable
  byte*; an adjacent-page or trailing byte borrowed solely as a sentinel
  must never be dereferenced as page content. Avoid forming an unsafe pointer
  before its address is proved to lie within the underlying allocation.
- **Logical extent.** Check row/count conversions, including independent
  counts in custom headers, and `count+1` before use. Positive-width
  `count <= available/width` bounds the physical column; a dependent-offset
  scan, when chosen, is O(rows) per initialization. Width zero has no per-row
  bytes: check the constant (including a delta base) once, never loop over
  an unencoded logical count. `At(row)` still requires `0 <= row < count`;
  adjacent reads need both indexes valid without overflowing the addition.
- **Cross-column extent.** Validate offset monotonicity, summary-bit targets,
  bundle/prefix relationships and keyspan boundary indices against the
  columns they will index before exposing their unchecked accessors. A final
  offset does not establish that intermediate offsets are ordered.
- **Output extent.** Check sums of component lengths without narrowing or
  overflow, then slice the actual destination before built-in unsafe stores,
  including synthetic bytes. A header maximum is an allocation hint, not
  evidence that a store fits. Registered `KeySchema` implementations are
  trusted code acting on untrusted block fields; Pebble currently promises
  their `KeySeeker` a sufficiently large caller buffer. Do not shrink that
  buffer below its recorded maximum for a valid custom schema without a
  proved schema-specific expansion bound. Built-ins must check actual writes,
  but arbitrary custom unsafe code is not covered until a compatible
  capacity contract and migration path are agreed. Do not claim engine-wide
  safety for those schemas yet.
- **Lifetime and mutability.** Cached decoder metadata shares its block's
  allocation and is live only while the handle is retained; direct `Init`
  borrows the caller's bytes until the last view is discarded. For example,
  the uncompressed aligned suffix-rewriter path can decode a block aliasing
  its caller's slice. Neither a handle nor a checked constructor prevents
  mutation through another alias. Require immutability while a view is live;
  an entry point that cannot enforce this may need an owned copy. Mutation
  requires exclusive access and invalidation of *all* old views before
  revalidation. Do not put pointers to separate allocations into opaque
  metadata that the GC cannot trace, and measure cache size classes if
  metadata grows.

The proof lives in the `Decode*`/`Init` constructor shared by cached, direct,
and lazy callers; it cannot rely on a metadata callback having run. An
optional or nested decoder proves its own fields before first use. A failed
constructor must not publish a cache value, set a lazy-ready flag, or let
callers reuse a partial view. Keep the current decoder assertion style;
convert panics to contextual corruption errors at applicable API boundaries,
auditing direct call sites separately. Recovery cannot undo an unsafe access,
and a Go bounds panic is preferable to one but not a reason to keep reading.

## Choosing a validation tool

| Field/use | Default tool | When to choose another |
| --- | --- | --- |
| Fixed header and page table | Eager checked arithmetic in `BlockDecoder.Init`, O(columns). | None if the fields select later pointer addresses. |
| Physical column | Check its complete extent in its `Decode*` constructor, once per constructed view. | A lazy column runs the same constructor at first use, not on every iterator step. |
| Dependent offsets or indexes | Prefer a one-time scan before unchecked consumers; zero-width constants stay O(1). | Only if cold-read cost is unacceptable, benchmark complete checked-consumer coverage against the scan; per-row checks can hurt hot iteration. |
| Optional nested payload | Decode at first use under the owning handle/borrow; publish sibling views together only after all succeed. Tiering currently memoizes per iterator, not cache entry. | Warm iterator first use may repeat page validation; benchmark before adding cached metadata or an unchecked reuse path. |
| Materialized output | Check the real destination before each unsafe write, with length arithmetic checked first. | Use a safe slice/copy when a proof is harder to maintain than its measured cost. |

For each migrated family, record the first unchecked use, owning constructor
or consumer, validation timing, lifetime, failure path, and cold/warm cost.
Prove any bound derived from encoded bytes in production; retain
invariants-only checks for programmer misuse. This is a review rule, not a
runtime descriptor, global recovery mechanism or new public API.

## Proposed factoring, without per-row overhead

Start with a small package-local range check: compare offset with the slice
length, then size with the remaining bytes, before adding or narrowing.
The proved slice bounds also establish that the result fits `int`. Use
existing decoder assertions rather than an error-returning wrapper around
every hot operation. An exclusive end permits an empty slice, but not
`&buf[end]` without an actual padding or adjacent sentinel byte; alignment
remains relative to the original block.

Keep checks in existing `Init`/`Decode*` constructors and the dependent
consumer that owns the invariant. Keep proven `UnsafeUints.At` and
`RawBytes.At` fast, but make row-index preconditions explicit at their
callers. Do not add a cached descriptor or per-column allocation by default:
opaque metadata cannot trace separate pointers and even a larger inline
descriptor can change cache allocation size classes. If a shared helper
obscures a format-specific proof or costs more, retain a few direct checks.
Comments belong on non-obvious contracts (sentinel, lifetime, width-zero,
unchecked accessor), with tests carrying the malformed-input examples.

## Scope boundary: malformed data, not resource limits

This stack rejects inconsistent encoded extents, overflowing address
arithmetic, out-of-range dependent indexes, unordered offsets and unsafe
output stores *before* their unchecked use. Reject an input extent that
cannot fit the encoded buffer before constructing its view or allocating
solely to decode that known-impossible extent. Grow the output buffer or
fail before an unsafe store that would exceed its actual capacity. A panic
or error at that boundary is preferable to reading or writing outside the
allocation.

Large is not by itself malformed. A very large value (even 5 GiB if its
format supports that length), a format-valid large block handle, or a
width-zero column representing many logical rows is not rejected merely
for its size. Decoding valid data may exhaust memory or CPU under existing
behavior; preventing that is a separate resource-budget and
availability effort, not a success criterion for this stack. This does
not excuse integer overflow or out-of-bounds access, and new validation
must not loop over an unencoded logical count just to prove it safe.
Avoid inventing a compressed-size-to-row-count cap.

Any future policy for large decompressed output, handles or materialized
spans needs its own compatibility evidence, limits and failure semantics.
It should not be hidden in a bounds helper or gate these input-safety PRs.

# Proposed upstream PR stack

This fork RFC is a design and tracking aid, not an upstream policy prerequisite.
Submit small, independently useful implementation PRs, each with malformed
and valid-writer tests and a benchmark where a hot path changes. Port only
the relevant fork-topic changes onto master, adapting its decoder signatures;
do not ask reviewers to accept the whole architecture before the first fix.

| PR | Independently useful change | Required check |
| --- | --- | --- |
| 1. Output capacity | Match the existing built-in untyped-suffix idiom: slice the destination before the MVCC suffix stores. Widen synthetic prefix/recorded-max/suffix arithmetic before narrowing, without shrinking valid custom-schema capacity or adding a clamp. | In default builds, test wall-only (+10) and wall+logical (+14) output with exact-fit and restricted-capacity canaries; exercise length overflow in both iterator initializers without huge allocations. |
| 2. Page and uint extents | Complete the physical checks begun in #5856: validate page starts in `BlockDecoder.Init`, confine `DecodeColumn` before its callback, and bound `DecodeUnsafeUints` before returning an unsafe view. Adapt shared callers; assign lazy tiering views together after both decode, in a small companion commit. | A malformed-block table failing on master, valid writer/old block fixtures, zero-row padding, width-zero/delta base, direct/cached reads, a tiering-second-column failure followed by reinit, `GOARCH=386` build, decoder and warm-tiering first-use benchmarks. |
| 3. RawBytes offsets | Check monotonicity once in the `RawBytes` constructor before returning unchecked accessors; skip the scan for width-zero constants. A per-row checked-consumer design is fallback only if measured cold misses make the scan unacceptable and *every* dependent path is covered. | Compare a cold block load plus sparse seek with warm `BenchmarkDataBlockIter` reads on the same writer blocks; test nonmonotone offsets, decoded values and contextual cached corruption. |

If the hotfix lands upstream first, reuse its tested checks and submit only the
remaining work rather than duplicating them. Backport relevant landed slices
to the v2 consumer line only while necessary; track fork maintenance and exit
separately. Code review and technical CI never grant merge authority.

## Source-backed follow-up slices

These are candidate fork PRs, not claims that source inspection has reproduced
each failure. Start each with a malformed fixture reaching the named boundary,
then preserve valid writer output. The fork's default branch is `master`, not
`main`: independent PRs target it; genuinely dependent reviews target the
previous topic branch until the series is assembled. Do not duplicate earlier
patches, open upstream PRs or merge PRs as part of this work. PRs 1–3 above
remain the columnar foundation.

1. **Block handles (independent, tiny).** `sstable/block.DecodeHandle`
   slices `src[n:]` before checking whether `binary.Uvarint` returned a
   negative count for an overflowing first varint. Check both counts before
   using them and retain the documented `(Handle{}, 0)` invalid-input
   contract. Test an overflowing first and second varint plus a writer
   round trip; existing callers should still report corruption. No hot-path
   benchmark is needed for this change.
2. **Blob remapping (independent, small).** The writer's
   `indexBlockEncoder.AddVirtualBlockMapping` puts the gap sentinel in the
   *low* 32 bits; `cachedReader.GetUnsafeValue` compares the decoded
   *high* half, so that check never catches a referenced gap. Preserve
   writer-produced gap entries but reject a reference to one before using
   `indexBlockDecoder.BlockHandle`. Prove virtual/physical block indexes and
   adjacent offset order before their unchecked reads or subtraction;
   check `ValueID + valueIDOffset` in widened arithmetic against loaded
   value rows on both cold and cached paths. Exercise a rewritten file
   with a gap followed by a live block and nonzero value offset, valid
   repeat fetches and invalid virtual/value IDs; require contextual fetch
   errors. Benchmark warm fetch compares if they land on that path.
3. **Blob and value-block handle bytes (two independent PRs).** In
   `sstable/blob/handle.go` and `sstable/valblk/valblk.go`, the respective
   preface and suffix decoders load up to four continuation bytes via
   `unsafe.Add` without checking `len(src)`. Preserve the short valid
   fast path; fall back to checked decode for truncated encodings, or prove
   each byte before use. Test truncation at each varint position with a
   restricted-capacity canary, both eager handle creation and lazy
   `ValueFetcher.FetchHandle` / `valblk` fetch, and existing writer round
   trips. Decide whether the caller returns an error or a checked panic;
   if an exported signature changes, update all callers. Benchmark valid
   handle-heavy iteration, not just an isolated cold decoder.
4. **Row-block entry and seek paths (two dependent PRs).** In
   `sstable/rowblk/rowblk_iter.go`, `Iter.Init` reads a restart footer;
   `readFirstKey`, `readEntry` and seek/skip loops decode unrolled varints
   through unchecked pointers, then form `unsafe.Slice` key/value views.
   First prove footer/restart extents and entry lengths before key/value
   slices; then cover the separate seek/skip pointer paths before claiming
   whole-block protection. Test writer blocks with several restart groups
   and empty blocks, corrupted restart offsets, truncated 1–5-byte
   varints, too-short internal keys, inflated unshared/value lengths, and
   forward/reverse/seek behavior with canaries. Preserve the iterator's
   error contract; compare cold `Init` and existing
   `BenchmarkBlockIterSeekGE/SeekLT/Next/Prev`, deciding between a one-time
   scan and hot-path guards from measured cost. Row blocks remain in use
   for older table formats, not just historical test data.
5. **Prefix bundle arithmetic (after PRs 2–3).**
   `DecodePrefixBytes` reads an untrusted bundle shift; `bundleCalc` derives
   counts and indexes consumed by `PrefixBytes.SetAt`, `SetNext`,
   `UnsafeFirstSlice` and `Search`. Prove the derived offset indexes in
   representable arithmetic before unchecked access, preserving the
   block-wide shared prefix stored at offset zero. Determine the actual
   writer-supported shift range before choosing a cutoff: the writer uses
   `TrailingZeros32`, so do not reject a valid large bundle by assumption.
   Mutate shifts and index counts, test single/partial bundles and duplicate
   keys across boundaries against writer output; benchmark cold decode
   only if it gains more than an O(1) check.
6. **Bitmap summary targets (after PR 2).** `DecodeBitmap` proves the
   encoded nonzero bitmap's size, but `SeekSetBitGE/LE` can use a
   summary-selected primary word through unchecked `data.At`. Reject a
   nonexistent target before the read and set bits beyond `bitCount`
   in the final primary word; zero encoding remains O(1). Test writer
   zero/dense/sparse bitmaps at 63/64/65 bits, corrupt tail-summary and
   padding bits, and forward/reverse seeks. Measure summary-seek and cold
   decode. A false summary that hides real bits is also malformed, but a
   full consistency scan is a separately measured semantic decision, not
   silently covered by the minimal bounds fix.
7. **Keyspan boundary indexes (after PRs 2–3).**
   `KeyspanDecoder.Init` decodes separately counted boundary indices;
   `keyspanIter.materializeSpan` uses them to index trailers, suffixes and
   values. Prove nondecreasing indexes within key-row count before
   exposing the iterator; equal indexes are valid gaps. Check width-zero
   constants once, not once per logical row. Test writer gapped/empty
   spans, decreasing and past-end indexes across
   `First/Last/SeekGE/SeekLT/Next/Prev`; retain
   `BenchmarkKeyspanBlock_RangeDeletions` and measure cold validation if it
   scans positive-width entries.
8. **Direct and nested error boundaries (after PRs 2–3, by caller).**
   `ReferenceLivenessBlockDecoder.Init` executes after a no-op cache
   metadata initializer in ingest, blob rewrite, level checking and layout.
   Confirm each call's recovery/error contract, check `refID < Rows()` in
   blob rewrite before `LivenessAtReference`, and make
   `DecodeBlobRefLivenessEncoding` validate its bitmap size before a Go
   slice that currently panics despite the function's error return.
   A separate reader-open PR may wrap `decodePropertiesBlock` and
   columnar metaindex direct initialization if a malformed fixture
   proves an escaping assertion. Test each actual public call site with
   writer-produced positives, truncated outer/inner bytes, no partial
   state and contextual errors. The histogram describer is another
   *conditional* caller, not part of the liveness PR by default; avoid
   claiming a new unsafe read in the bitmap's Go slice itself.

Seed constructor fuzz targets with writer-produced blocks and mutated
counts, shifts, offsets and truncated varints in their owning PRs once
the regression is fixed. Add row-block targets after its guards. WAL
`record.Reader` and MANIFEST `VersionEdit.Decode` did not show an `unsafe`
decoder in this initial source survey: audit their error contracts and
write a format-specific target only with a proved gap. MANIFEST variable
lengths may drive large allocations; constrain the *fuzz harness* without
adding a production size cap under this RFC. No fake no-op PR is needed
when an audit finds no relevant defect.

Error conversion is separate from the memory-safety proof:

| Entry path | Current reader behavior | Proposed policy |
| --- | --- | --- |
| Cached columnar data/index/keyspan | Metadata init runs at block load and recovers decoder panics as corruption. | Keep contextual errors; the constructors still prove bounds for direct callers. |
| Direct `Init`/`Decode*`, including suffix rewriting and liveness | Can run without a reader metadata callback or its recovery. | Establish proof in that same constructor; document/assert or wrap at the public call site, without continuing after failure. |
| Lazy tiering or nested key/value payload | Decodes after surrounding block initialization, sometimes from a direct borrow; the two tiering views are assigned separately and an assertion may escape during iteration. | Validate before first use; decode sibling views into locals and assign together, then handle failure at that caller's API boundary without treating a partial view as ready. |
| Metaindex, filter, value or blob-reference liveness block reads | Some block reads use a no-op metadata initializer. | Validate in each actual consumer; a checksum and successful cache insert are not a decoded-view proof. |

The existing corruption listener may terminate the process by default; an
error return or recovered assertion does not promise continued operation.

# Validation and performance plan

For each PR, pair valid writer and historical output with focused field
mutations. Distinguish constructor rejection, contextual reader error, and
successful writer round trip. For accepted fixtures compare decoded rows
with a safe reference and exercise seeks, synthetic transforms, and optional
first-use paths. A canary around a restricted output buffer catches a
store-before-slice regression that a later panic would conceal. Run those
tests in default as well as invariants builds; never print bytes outside
the fixture.

- Cover empty/adjacent pages and real padding, width-zero uints and offsets
  with large logical counts, truncated positive widths, bitmap summaries,
  custom-header counts, nonmonotone offsets and dependent indexes. Check
  direct/handle paths and old writer output. Build the shared decoder
  signature changes for `GOARCH=386` as well as the normal target; cover
  both endian accessor implementations where supported.
- Upstream master at the pinned base has no Go `Fuzz*` targets. Add small
  fuzz targets at each constructor boundary (including direct/lazy paths),
  seeded with writer-produced blocks and mutated widths, counts, offsets and
  nested fields. Follow the first three PRs with format-specific targets for
  row blocks, blob/value handles, WAL, MANIFEST and batches. Agree on corpus
  ownership and CI time budget with maintainers. Bound materialization
  inside the fuzz harness to keep CI usable, not by changing the decoder's
  acceptance policy; `checkptr=2`, invariants and canaries complement, but
  cannot replace, a reviewable unsafe-access proof.
- Benchmark against a pinned base on the same runner, interleaving repeated
  runs with `benchstat`, `-benchmem` and profiles for material changes.
  Existing `BenchmarkDataBlockDecoderInit`, `BenchmarkDataBlockIter`,
  `BenchmarkRawBytes`, `BenchmarkUnsafeUints` and
  `BenchmarkKeyspanBlock_RangeDeletions` cover decoder and iterator microbenchmarks.
  Add a cold-cache block-load benchmark that forces metadata initialization:
  positive-width offset validation is O(rows) *per cache miss*, potentially
  the dominant new cost. Compare it with checked-consumer access and with a
  warm handle; isolate optional tiering first use and transforms. Hold
  width-zero encoded bytes constant while varying claimed rows to catch
  accidental O(rows) initialization. Record ns/op, B/op, allocs/op,
  throughput and cache bytes/block or size class, with sample size/noise.
- Preserve iteration speed and avoid extra steady-state allocations. Prefer
  one-time constructor validation; use per-row checks only after measuring
  a cold-read regression and proving every dependent path. No initialization
  loop may scale with an unencoded width-zero logical count. Treat a
  repeatable slowdown as a design checkpoint, not automatic acceptance.

# Compatibility, rollout, and alternatives

No on-disk version bump is proposed. Checks may reject malformed blocks sooner;
existing writer-produced and supported old files must still decode. Stage each
implementation PR with its writer-compatibility tests, benchmark deltas and
recorded error-surface changes. Keep the consumer fork pin reproducible; sync
only the landed upstream fixes needed by that consumer, and do not confuse
technical CI with approval, merge or a deployed runtime exercise.

Fuzzing alone discovers cases but does not establish a reusable proof.
Checksums and writer output do not validate encoded extents; a blanket
`rows <= len(block)` check rejects valid width-zero columns. Global panic
recovery cannot undo an unsafe read. Prefer local checks in the existing
constructors, with checked dependent consumers only when a measured
constructor scan costs too much and every consumer is covered.

# Open questions for reviewers

1. When do eager offset scans beat checked consumers on representative
   cache-miss workloads? Which exact callers can be proved safe either way?
2. What compatible contract can require custom `KeySeeker` implementations
   to check real output capacity before unsafe stores? Could an optional
   expansion bound or test-only conformance helper help? Until resolved,
   preserve at least the existing recorded capacity for valid custom files
   and exclude arbitrary unsafe schema code from the engine-wide claim.
3. May lazy tiering reuse a cached page-table proof, or must it re-prove
   on each iterator's first use? An inline metadata change may enlarge every
   cached block; measure that against first-use cost.
4. Which direct borrowed-slice paths, beginning with suffix rewriting, can
   guarantee immutability for the lifetime of every view?
5. Who owns the first constructor fuzz corpus and CI budget? Which small
   benchmark set can detect cold-read, lazy-first-use and warm-iterator costs?
