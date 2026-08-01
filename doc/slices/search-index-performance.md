# Indexed Search Performance Hardening Evidence

## Target

- Base revision: `070c86b549622977b0e661b0c73440c99762377c`
- Entry point: `git-agent search`
- Corpus: this repository's filesystem index, 210 files and 3,360 chunks after
  the implementation changes
- Embeddings: `Qwen/Qwen3-Embedding-0.6B`, 1,024 dimensions
- Environment: Linux/amd64, Go 1.26.5, Intel Core i5-13500

This evidence covers warm indexed retrieval. It does not define a public
latency or memory guarantee and does not change the search contract in
`docs/spec.md`.

## Demonstrated Risks

1. Cached synchronized-pack validation retained all 369 decoded packs, whose
   payloads totaled 1,168,887,300 bytes. Configured search reached 2,318,352
   KiB peak RSS.
2. Same-target warm retrieval searched 92 historical manifests before loading
   the already-complete target index. The cache phase took 1.733 seconds of a
   1.891-second local request.
3. Unchanged schema-v2 snapshot export rebuilt the global pack catalog, and an
   identical fetched commit hard-reset the synchronized worktree again.

## Changes

- Validate cached catalog entries one pack at a time while preserving complete
  pack, slot, embedding-key, vector-digest, and payload validation.
- Load compatible vectors from the selected target first and search historical
  indexes only for unresolved embedding-input hashes.
- Load the shared local vector catalog and payload at most once during one
  cross-index exact-reuse pass.
- Skip synchronized checkout/reset only when both commit and branch already
  match the fetched target.
- Skip unchanged schema-v1 and schema-v2 snapshot writes; schema-v2 avoids
  loading the global pack catalog on this no-op path.

## Real-Repository Results

Process measurements used exact-query replay, `--limit 10`, brief output
redirected to `/dev/null`, and `/usr/bin/time -f '%e %U %S %M'`. Synchronized
and local-only runs were sequential so the global producer lock did not create
benchmark contention.

| Path | Before | After | Change |
| --- | ---: | ---: | ---: |
| Synchronized median, 7 runs | 10.05 s | 3.24 s | -67.8% |
| Synchronized p95/max | 10.31 s | 3.43 s | -66.7% |
| Synchronized peak RSS | 2,318,352 KiB | 247,636 KiB | -89.3% |
| Local default median | 1.84 s | 0.32 s | -82.6% |
| Local default p95 | 1.94 s | 0.50 s | -74.2% |
| Local default peak RSS | 199,048 KiB | 154,704 KiB | -22.3% |
| Local `--code --no-tests` median | 0.50 s | 0.27 s | -46.0% |
| Local `--code --no-tests` p95 | 0.57 s | 0.45 s | -21.1% |

The pre-change local row uses the 13 steady-state runs from a 15-run sample.
Two runs overlapping external corpus edits were excluded before implementation.
The post-change local rows contain 15 runs each without exclusions.

Built-in debug timings for local default search changed as follows:

| Phase | Before | After |
| --- | ---: | ---: |
| Total | 1.891 s | 202 ms |
| Discover | 87 ms | 82 ms |
| Cache | 1.733 s | 37 ms |
| Cached query embedding | 10 ms | 12 ms |
| Score | 45 ms | 49 ms |
| Replay | 16 ms | 21 ms |

Configured synchronized debug timing changed from 10.433 seconds total to
3.217 seconds. The recursive committed-HEAD cache phase changed from 1.253
seconds to 27 milliseconds, and the filesystem cache phase changed from 1.683
seconds to 28 milliseconds. Remaining synchronized time is primarily remote
reachability/fetch/reconciliation inside the outer `discover` timing bucket.

## Executable Benchmarks

Run:

```sh
shadowtree test ./internal/tasks/search -run=^$ \
  -bench='Benchmark(WarmIndexedSearchRun|ValidateCachedVectorPackCatalog)$' \
  -benchmem -count=5
```

Observed ranges:

| Benchmark | Time | Bytes/op | Allocs/op |
| --- | ---: | ---: | ---: |
| `BenchmarkWarmIndexedSearchRun` | 44.7-53.0 ms | 70.2-70.5 MB | 268,276-268,424 |
| `BenchmarkValidateCachedVectorPackCatalog` | 8.93-10.99 ms | 6.11 MB | 10,572-10,573 |

The warm benchmark builds a deterministic 200-file, 3,200-symbol corpus with
1,024-dimensional vectors, adds 100 historical manifests, primes one query,
then requires every timed request to report an index hit and exact replay
without another embedding call. The catalog benchmark validates 32 packs with
128 vectors per pack.

## Correctness Boundary

Focused and package-level tests retain these invariants:

- `--reindex` never reuses a stale generation.
- Capped embedding input, model, dimensions, and vector length remain reuse
  identity.
- Target-local source, blob, path, and line metadata are rebuilt on reuse.
- Deleted, ignored, scoped, code-filtered, and test records retain their
  existing cleanup/preservation semantics.
- Corrupt or legacy records fall back to embedding.
- Filesystem-to-revision and revision-to-revision reuse remain supported.
- Synchronization still performs remote reachability, fetch, import,
  compatible export, commit convergence, push, and existing progress phases.
- A remote default-branch rename still updates local branch ownership even when
  its commit hash is unchanged.

## Unverified

- A full cold `--reindex` was not benchmarked.
- Fresh-machine synchronization, remote failure/retry, and catalog corruption
  were covered by automated tests but not timed against the production-sized
  synchronized store.
- The result does not establish scaling beyond this 3,360-chunk repository or
  the synthetic 3,200-symbol benchmark corpus.
- Scoring remains an exact scan; it was not changed because its measured
  45-49-millisecond phase is no longer the dominant warm-path cost.
