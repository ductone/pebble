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

On invalid input, stop processing the affected block: return corruption or
assert before unsafe memory access, without exposing a partial view. This
series of small changes uses Pebble's existing `Init`/`Decode` structure, keeps
writer-compatible encodings, and treats allocation limits as a related but
separate obligation. No on-disk format change or general decoding framework
is proposed.

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

## First proof matrix: unsigned columns

This matrix describes the pinned master baseline, not the separate hotfix.
Each implementation PR should extend it only for the consumers it migrates.

| Boundary and immediate caller | Baseline at `f546ea14` | Proposed proof owner |
| --- | --- | --- |
| [`BlockDecoder.Init`](../../sstable/colblk/block.go), called by data/index/keyspan metadata initializers or a direct decoder | Stores a slice and decodes the fixed header; it does not validate all page starts. | Check header size, version, column count and monotone page starts once, preserving alignment to the original block. |
| `DecodeColumn` calling `DecodeUnsafeUints` | Passes the whole block, then compares the returned offset with the next page. A decoder can construct an unsafe view before that comparison. | Pass a proved page extent to the decoder, allow only an explicitly non-dereferenced sentinel byte, and require an exact decoded end. |
| [`DecodeUnsafeUints`](../../sstable/colblk/unsafe_uints.go) constructing its pointer | A Go bounds check for `&b[off]` does not prove `rows * width` bytes are present; a delta base and alignment also advance the cursor. | Prove base/alignment/width and whole physical span before pointer construction; width zero checks the constant once and needs no row scan. |
| `UnsafeUints.At` and `UnsafeOffsets.At2`, reached by seeks/iteration and dependent columns | Indexing uses unchecked pointer arithmetic; a valid encoded span alone does not prove caller indexes. | Owning iterator or dependent decoder establishes row/adjacent-index range; pin an immutable block while the view remains live. |
| `RawBytes` consuming `UnsafeOffsets` | It bounds only the final offset; intermediate offsets can be unordered, and accessors use unchecked addresses. | Check all relevant offsets once if a cache-miss scan is justified, or guard the few dependent consumers; width-zero offsets need no scan. |

The metadata callbacks recover initialization panics as corruption, but they
cannot undo an unsafe access. An optional lazily decoded column has its own
first-use proof even when the surrounding block was cached successfully.

Goals:

1. For every decoder path in scope, stop on malformed extents before an unsafe
   access; never expose a partially validated view.
2. Validate at construction when a reusable proof pays off; allow first-use or
   checked-consumer validation when it is both sound and cheaper.
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
  trusted code acting on untrusted block fields. The current public
  `KeySeeker` contract promises a sufficiently large caller buffer; old
  custom schemas may expand beyond the encoded block. Built-ins must check
  their actual writes, but an arbitrary custom unsafe seeker is not covered
  until a capacity contract and migration path are agreed; do not claim
  engine-wide safety for such schemas in the meantime.
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
| Dependent offsets or indexes | Validate relationships once before unchecked consumers; zero-width constants stay O(1). | If a positive-width O(rows) scan costs more on cold reads, prove each access with checked consumers and benchmark both designs. |
| Optional nested payload | Decode and memoize on first use under the owning handle/borrow. | Validate eagerly only if all readers need it and cold-read cost warrants it. |
| Materialized output | Check the real destination before each unsafe write, with length arithmetic checked first. | Use a safe slice/copy when a proof is harder to maintain than its measured cost. |
| Allocation from encoded length | Validate representability and known format bounds before allocating. | Additional resource limits need writer-compatibility evidence; no arbitrary count-to-compressed-size clamp. |

For each migrated family, record the first unchecked use, owning constructor
or consumer, validation timing, buffer lifetime, failure path, and cold/warm
cost. This is a review aid, not a runtime descriptor or a new public API.

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

## Materialization and resource policy

Memory safety and bounded resource consumption are related but distinct.
A compressed constant column can represent many rows in few bytes; a
writer-compatible decoder cannot reject it merely because `rows > len(block)`.
Initialization should not spend CPU proportional to an unencoded logical count.
Actual iteration over requested rows necessarily does work, and an allocation
sized from a logical span length may still exhaust memory without addressing
outside an allocation. The same distinction applies to a decompressor-reported
output size and block-handle-derived read size.

Inventory allocations driven by encoded fields: verified byte extent,
schema-specific expansion and decompressor output are different bounds.
Reject unrepresentable sizes before allocating. Limits on valid compressed
output, block handles or logical span materialization need writer and
historical-file evidence plus an operator-visible failure mode. They are a
resource-policy track, not an arbitrary maximum hidden in a bounds helper.

# Proposed upstream PR stack

The RFC and its proof matrix are the design slice; they do not gate a fix.
The fork topic is prior art, not an already-landed upstream slice. Port only
the relevant changes onto master, adapting its different decoder signatures.
Each implementation PR stands alone with its own malformed and valid-writer
tests, baseline, and benchmark where it changes a hot path.

| PR | Independently useful change | Required check |
| --- | --- | --- |
| 1. Output capacity | Normalize `maximumKeyLength` as an allocation hint without rejecting valid excluded keys; check synthetic-length sums before narrowing and the actual destination before built-in MVCC unsafe stores. Leave third-party schema API decisions explicit. | Exact-fit and restricted-capacity canaries, both synthetic suffix lengths, excluded long key written/read through an iterator; no new per-row scan. |
| 2. Physical structure | Prove the common header/page starts once in `BlockDecoder.Init`; confine `DecodeColumn` to its page and bound complete positive-width integer, bitmap and prefix physical extents before constructing unsafe views. Keep zero-row padding and alignment explicit. | Direct and metadata-cache paths, empty/adjacent pages, delta base, width-zero, malformed header/summary, and header/uint init benchmarks. |
| 3. Dependent relationships | Bound raw-byte offsets, keyspan indexes and blob-index remaps at their owning constructors or dependent consumers. For positive-width offsets, compare a one-time O(rows) scan against checked consumers before choosing; constants remain O(1). | Reproducible cold-cache read and warm-iterator benchmarks, safe-reference decoded rows, cross-column mutation fixtures. |
| Follow-ups | Cover first-use tiering and nested payloads, direct liveness/properties/suffix-rewriter paths, and error conversion by call site. Add constructor fuzz targets and extend to row blocks/WAL/MANIFEST with format-specific notes rather than a blanket decoder rewrite. | Preserve each path's existing API contract and report any new rejection. |
| Resource track | Audit decompressed lengths, block handles, logical span allocations and format-derived count limits. Adopt policies only after valid-writer compatibility measurements. | Fail before expensive allocation without rejecting supported files by assumption. |

If the hotfix lands upstream first, reuse its tested checks and submit only the
remaining work rather than duplicating them. Backport relevant landed slices
to the v2 consumer line only while necessary; track fork maintenance and exit
separately. Code review and technical CI never grant merge authority.

Error conversion is separate from the memory-safety proof:

| Entry path | Current reader behavior | Proposed policy |
| --- | --- | --- |
| Cached columnar data/index/keyspan | Metadata init runs at block load and recovers decoder panics as corruption. | Keep contextual errors; the constructors still prove bounds for direct callers. |
| Direct `Init`/`Decode*`, including suffix rewriting and liveness | Can run without a reader metadata callback or its recovery. | Establish proof in that same constructor; document/assert or wrap at the public call site, without continuing after failure. |
| Lazy tiering or nested key/value payload | Decodes after surrounding block initialization, sometimes from a direct borrow. | Validate before first use, memoize only on success, and handle failure at that caller's API boundary. |
| Metaindex, filter, value or blob-reference liveness block reads | Some block reads use a no-op metadata initializer. | Validate in each actual consumer; a checksum and successful cache insert are not a decoded-view proof. |

# Validation and performance plan

For each PR, pair valid writer and historical output with focused field
mutations. Distinguish constructor rejection, contextual reader error, and
successful writer round trip. For accepted fixtures compare decoded rows
with a safe reference and exercise seeks, synthetic transforms, and optional
first-use paths. A canary around a restricted output buffer catches a
store-before-slice regression that a later panic would conceal. Run those
tests in default as well as invariants builds; never print bytes outside
the fixture.

- Cover empty/adjacent pages and their real padding byte, width-zero uints
  and offsets with a large logical count, truncated positive widths, invalid
  bitmap encoding/summary, custom-header counts, nonmonotone offsets and
  cross-column indexes. Test both direct and handle-backed construction,
  and old writer-produced blocks; use 32-bit and both endian accessor
  implementations where the test infrastructure supports them.
- Upstream master at the pinned base has no Go `Fuzz*` targets. Add small
  fuzz targets at each constructor boundary (including direct/lazy paths),
  seeded with writer-produced blocks and mutated widths, counts, offsets and
  nested fields. Follow the first three PRs with format-specific targets for
  row blocks, blob/value handles, WAL, MANIFEST and batches. Agree on corpus
  ownership and CI time budget with maintainers. Bound each fuzz input's
  materialization work; `checkptr=2`, invariants and canaries complement,
  but cannot replace, a reviewable unsafe-access proof.
- Benchmark against a pinned base on the same runner, interleaving repeated
  runs with `benchstat`, `-benchmem` and profiles for material changes.
  Existing `BenchmarkDataBlockDecoderInit`, `BenchmarkDataBlockIter`,
  `BenchmarkRawBytes`, `BenchmarkUnsafeUints` and
  `BenchmarkKeyspanBlock_RangeDeletions` cover useful steady-state paths.
  Add a cold-cache block-load benchmark that forces metadata initialization:
  positive-width offset validation is O(rows) *per cache miss*, potentially
  the dominant new cost. Compare it with checked-consumer access and with a
  warm handle; isolate optional tiering first use and transforms. Hold
  width-zero encoded bytes constant while varying claimed rows to catch
  accidental O(rows) initialization. Record ns/op, B/op, allocs/op,
  throughput and cache bytes/block or size class, with sample size/noise.
- Preserve iteration speed and avoid extra steady-state allocations. Prefer
  a one-time check when it amortizes; allow a measured checked-consumer
  alternative when the cold-read scan is worse. No decoder initialization
  loop may scale with an unencoded width-zero logical count. Treat a repeatable
  slowdown as a design checkpoint and verify an end-to-end workload before
  claiming an improvement.

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
- **Safe indexing at dependent consumers:** a viable alternative to the
  O(rows) offset scan for sparse access; compare cold and warm workloads.
  Do not bounds-check every hot accessor by default.
- **Treat checksummed or writer-produced bytes as trusted:** checksums detect
  accidental changes, not format validity; valid writer output has edge cases
  that simplified assumptions can reject.
- **Clamp every count/size to compressed bytes:** rejects valid zero-width
  encodings and synthetic-key cases; distinguish physical extent from logical
  output and allocation policy.
- **Recover all panics:** cannot undo an unsafe access and can obscure bugs;
  recover only at known error boundaries after the memory-safety invariant is
  established.
- **One universal `ValidatedBlock` type or global panic recovery:** neither
  proves nested schema-specific relationships or repairs an unsafe access.
  Prefer a small local range helper and the existing constructors.

# Open questions for reviewers

1. When do eager offset scans beat checked consumers on representative
   cache-miss workloads? Which exact callers can be proved safe either way?
2. What compatible contract can require custom `KeySeeker` implementations
   to check real output capacity before unsafe stores? Would an optional
   schema-specific expansion bound or a test-only conformance helper suffice?
   Until resolved, the engine-wide guarantee excludes arbitrary custom code.
3. Which direct borrowed-slice paths, beginning with suffix rewriting, can
   guarantee immutability for the lifetime of every view?
4. Which format-derived allocations need a separate resource limit, and
   which writer/historical files support that decision?
5. Who owns the first constructor fuzz corpus and CI budget? Which small
   benchmark set can detect cold-read, lazy-first-use and warm-iterator costs?
