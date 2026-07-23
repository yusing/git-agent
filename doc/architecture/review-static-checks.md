# Review static-check architecture

This document is the implementation handoff for
`SLICE-REVIEW-CHECKS-001`. It records the repository seams, upstream evidence,
shared data shapes, ordering, failure rules, and proof required to implement
`REQ-REVIEW-CHECK-001` through `REQ-REVIEW-CHECK-009` without repeating the
discovery work.

## Evidence baseline

The following code is the current owner at planning revision `ARCH-1`:

| Concern | Current owner and seam |
| --- | --- |
| Review launch, preparation, provider call, and final event | `internal/cli/app.go`, `(*App).runCodeReview`; the insertion point is after `runner.Run` succeeds and its validated text is decoded, but before `recorder.WriteExact("final", ...)` |
| Provider-authored review schema and validation | `internal/tasks/review/task.go`, `ReviewReport`, `OutputSchema`, `ValidateRepository`, and `Shape` |
| Authoritative diff-mode paths and drift fingerprint | `internal/tasks/review/task.go`, `PreparedContext`; populated by `Prepare` from recursive uncommitted or staged review snapshots in `internal/gitctx` |
| Durable final report and repeatable wait | `internal/background/store.go`, `(*Store).Complete` and `(*Store).Wait`; `Wait` returns the final event's `text` value unchanged |
| Detached self-execution precedent | `internal/cli/background.go`, `startDetachedTask` and `startDetachedProcess`; it locates the current executable and uses `os.StartProcess` with exact arguments and environment |
| Deterministic provider-free review fixture | `internal/cli/reviewtest.go`, `dryRunEvents` |
| Current normative review contract | `docs/spec.md`, section `git-agent review [--codebase|--uncommitted|--staged]` |

No existing static-check owner, result type, config search, or golangci-lint
dependency exists in the repository.

The upstream API assessment is pinned to
`github.com/golangci/golangci-lint/v2@v2.12.2`:

- [`pkg/commands/root.go`](https://github.com/golangci/golangci-lint/blob/v2.12.2/pkg/commands/root.go)
  exports `commands.Execute(BuildInfo)`, but command
  construction is private and argument parsing reads process-global `os.Args`.
- [`pkg/commands/run.go`](https://github.com/golangci/golangci-lint/blob/v2.12.2/pkg/commands/run.go)
  always calls `os.Exit(c.exitCode)` from the run command's
  persistent post-run hook. Calling it inside the detached review worker would
  bypass Git-agent cleanup, heartbeat completion, final-event persistence, and
  SSE shutdown.
- [`pkg/lint/context.go`](https://github.com/golangci/golangci-lint/blob/v2.12.2/pkg/lint/context.go)
  exposes `NewContextBuilder`, but its required package
  cache type comes from golangci-lint's `internal/cache`; Git-agent cannot
  construct that type from another Go module. Reassembling the runner from
  public packages is therefore not a supported alternative.
- [`pkg/config/base_loader.go`](https://github.com/golangci/golangci-lint/blob/v2.12.2/pkg/config/base_loader.go)
  and `pkg/config.NewLintersLoader` own configuration discovery and default
  loading. The integration must reach it through the upstream command rather
  than copy its search rules.
- [`pkg/printers/json.go`](https://github.com/golangci/golangci-lint/blob/v2.12.2/pkg/printers/json.go),
  `pkg/result.Issue`, and `pkg/report.Data` are the
  result structures emitted by the upstream JSON printer.
- [`pkg/exitcodes`](https://github.com/golangci/golangci-lint/blob/v2.12.2/pkg/exitcodes/exitcodes.go)
  defines `0` as success and `1` as issues found, but repository
  configuration can change the issue exit code. The helper must force
  `--issues-exit-code=1` to make the parent protocol deterministic.

## CTR-REVIEW-CHECK-001 — Preserve provider and final-report ownership

Protects REQ-REVIEW-CHECK-001, REQ-REVIEW-CHECK-004,
REQ-REVIEW-CHECK-007, and REQ-REVIEW-CHECK-010.

`internal/tasks/review` remains the owner of the provider-authored report and
the final review envelope. The provider `TextFormat`, `OutputSchema`,
`ValidateRepository`, and repair pass must continue using the existing
`ReviewReport`; `checks` must not be added to the provider schema.
`internal/checks` owns checker-neutral scope, results, normalization,
coordination, and registered-checker ordering. Neither owner may import a
concrete checker adapter.

Add a distinct typed final report that contains:

```text
summary: string
recommendation: APPROVE | COMMENT | REQUEST_CHANGES
findings: []Finding
checks: []checks.Result
orchestration_manifest_sha256?: string
```

The final type must be constructed only after strict decoding of the validated
provider text. Avoid a second untyped `map[string]any` report owner. The
orchestration digest currently injected in `internal/cli/app.go` must move into
the typed final report without changing its conditional presence.

`internal/background` remains format-agnostic: it persists the final event and
returns its `text` value. Do not add check-specific storage or a second result
file to the background store.

## CTR-REVIEW-CHECK-002 — Use one checker-neutral result and runner contract

Protects REQ-REVIEW-CHECK-004, REQ-REVIEW-CHECK-005,
REQ-REVIEW-CHECK-007, and REQ-REVIEW-CHECK-010.

Define the following domain in `internal/checks`:

```text
Result
  name: stable registered checker name
  status: "pass" | "findings" | "skipped" | "error"
  diagnostics: []Diagnostic
  omitted?: positive integer
  reason?: nonempty stable string
  error?: nonempty bounded string

Diagnostic
  path: normalized repository-relative path
  line: positive integer
  column?: positive integer
  code?: nonempty engine-specific rule or sub-check
  message: nonempty single-line string

Scope
  kind: "changed" | "codebase"
  root: absolute authoritative workspace root
  paths: exact repository-relative changed paths when kind="changed"
  components: sorted root-relative repository-component prefixes, including ""

Plan
  CheckerName(): stable checker name
  Runnable(): whether execution is required
  SkipReason(): stable reason when Runnable() is false

Runner
  Name(): stable checker name
  Plan(Scope): checker-owned immutable Plan
  Run(context, executable, Plan): Result or terminal error

Helper
  Runner plus RunHelper(validated private arguments)
```

`Scope` fields are private. Its constructor validates one absolute canonical
root, normalizes and clones changed paths and recursive repository-component
prefixes, requires the root component `""`, rejects paths for codebase scope,
and its accessors return copies. It exposes the longest containing component
for a repository-relative path. An adapter therefore cannot mutate the scope
seen by another checker or search ancestor configuration across a registered
submodule boundary.

Presence invariants:

- `pass`: empty diagnostics, no `omitted`, no `error`.
- `findings`: at least one diagnostic or a positive `omitted`; no `error`.
- `skipped`: empty diagnostics, no `omitted`, no `error`, and one stable
  non-secret `reason`.
- `error`: empty diagnostics and one bounded `error`; never include raw helper
  stdout, a full stack, environment, or a complete checker configuration.

The shared result constructors, validator, and shaper live in
`internal/checks`. Sorting key is `path`, `line`, `column`, `code`, `message`.
Apply the 100-item cap independently per checker only after its adapter filters
to authoritative scope and deduplicates. `omitted` counts additional in-scope
unique diagnostics, not upstream out-of-scope results.

A checker set owns deterministic registration, planning, progress, and
iteration. It rejects empty, duplicate, malformed, and unknown/future checker
names; plan-name mismatches; malformed skip plans; and result-name mismatches.
It constructs skipped results itself, calls progress only after a runnable plan
exists, and validates each result before accepting it.

`internal/checks/builtin` is the sole compile-time registration owner. Adding a
bundled checker requires its adapter plus one entry there. It must not require
modifying the shared result domain, review task, Git snapshot owner, background
store, wait path, or CLI lifecycle. This is not runtime plugin loading,
arbitrary command execution, or a model tool.

## CTR-REVIEW-CHECK-003 — Route private helpers through the checker set

Protects REQ-REVIEW-CHECK-002, REQ-REVIEW-CHECK-006,
REQ-REVIEW-CHECK-008, and REQ-REVIEW-CHECK-010.

The checker set owns one fixed, undocumented private command.
`internal/checks/builtin` constructs the same set for parent execution and child
dispatch. Its first validated argument is the registered checker name; the set
rejects unknown, malformed, or non-helper registrations before dispatching the
remainder to that adapter. Public help, completions, README, and the ordinary
command list must not expose this command.

For golangci-lint, the only viable bundled boundary is a synchronous child
invocation of the current Git-agent executable:

1. The review worker obtains its own executable with `os.Executable`.
2. It creates one owner-only temporary directory outside the repository.
3. It starts the same executable through the checker set's private command,
   registered name `golangci-lint`, a result path beneath that directory, one
   validated module working directory, and a bounded target list produced by
   the adapter's mode-specific planner.
4. The child validates that the result path is absolute, beneath the supplied
   owner-only temporary directory, and not a symlink. It also validates that
   the working directory is one discovered module root beneath the selected
   authoritative workspace and that targets are either the single fixed
   codebase target `./...` or repository-local `.go` files selected by the
   diff-mode planner. It must reject flags, absolute paths, escaping paths,
   empty targets, and mixed codebase/file targets.
5. In the child process only, the private dispatcher replaces `os.Args` with
   the validated, mode-specific golangci-lint argv and calls
   `golangci-lint/v2/pkg/commands.Execute`. Its `BuildInfo.Version` is exactly
   `v2.12.2`, `BuildInfo.GoVersion` is `runtime.Version()`, and commit/date
   metadata is empty; the version participates in upstream cache salt and must
   not be inherited from Git-agent's release version.
6. The child is expected to terminate through upstream `os.Exit`; no in-process
   cleanup or return value may be required after that call.
7. The parent waits, parses the private JSON file, classifies the exit, removes
   the entire temporary directory, and deterministically aggregates all
   planned module/package invocations into one `checks.Result`.

Use `os.StartProcess`, matching the repository's existing exact self-process
pattern. Do not add `exec.Command*`, search `PATH`, invoke an installed
golangci-lint binary, accept unvalidated caller-provided argv, expose a general
subprocess runner, or register any helper as a model tool.

The parent must create and concurrently drain bounded stdout and stderr pipes
so the helper cannot deadlock on output. The combined capture limit is 16 MiB.
After reaching the retained cap, it must continue draining and discard the
remainder until process exit. Captured text is diagnostic input only and is not
copied wholesale into the final report.

Cancellation must terminate the helper and its descendant Go-tool processes.
The implementation must use the repository's platform-specific process
attributes or add a check-specific process-group primitive with tests on
supported platforms; killing only the immediate helper PID is insufficient.

## CTR-REVIEW-CHECK-004 — Fix the upstream invocation

Protects REQ-REVIEW-CHECK-003 and REQ-REVIEW-CHECK-008.

The child working directory is the planned module root. Every invocation uses
the following fixed safety and output flags:

```text
golangci-lint run <validated-targets>
  --fix=false
  --issues-exit-code=1
  --show-stats=false
  --output.json.path=<private-result-path>
  --output.text.path=<null>
  --output.tab.path=<null>
  --output.html.path=<null>
  --output.checkstyle.path=<null>
  --output.code-climate.path=<null>
  --output.junit-xml.path=<null>
  --output.teamcity.path=<null>
  --output.sarif.path=<null>
```

Verify these names against the pinned v2.12.2 command flags during
implementation. Every supported non-JSON destination must be overridden so a
repository or home config cannot direct reports into the checkout. Do not pass
`--config`, `--no-config`, `--default`, `--enable`, `--disable`,
`--enable-only`, `--new`, `--new-from-rev`, or `--new-from-patch`.

Target construction is mode-specific:

- `--uncommitted` and `--staged`: group authoritative existing changed `.go`
  files by nearest containing module and package directory, then pass only
  those files. Separate package directories use separate invocations so Go's
  named-file rules cannot widen the target or fail due to cross-directory file
  lists.
- `--codebase`: discover every module deterministically and run one `./...`
  invocation from each module root. Nested modules are separate targets and are
  never replaced by a single invocation-root `./...`.

The absence of configuration is not detected by Git-agent. Each invocation
starts from its module root so golangci-lint performs its normal
current-directory, analyzed-path ancestor, home-directory, and default loading.

Exit classification after forcing `--issues-exit-code=1`:

- every planned invocation exiting `0` with a valid JSON result and no retained
  issues: `pass`;
- exit `1` plus a valid JSON result with issues: continue to filtering,
  deduplication, aggregation, and the global 100-diagnostic cap, producing
  `findings` or `pass` when every issue is outside scope;
- any other exit, missing/oversized/invalid JSON, or nonempty
  `JSONResult.Report.Error` in any invocation: one bounded `error` result,
  unless the parent context was canceled;
- parent context cancellation: terminal review failure, never `status:error`.

## CTR-REVIEW-CHECK-005 — Preserve authoritative review scope

Protects REQ-REVIEW-CHECK-005, REQ-REVIEW-CHECK-009, and
REQ-REVIEW-CHECK-010.

`KindReview` supplies a shared `checks.Scope` in all three review modes.
`KindSimplify` must not run the checker set or add `checks`. The scope producer
does not know language extensions, module formats, or checker-specific target
syntax. Eligibility belongs in each adapter; the shared coordinator only
orders registered runners and enforces that their returned names match their
registrations.

Every `KindReview` final report contains exactly one result for every
registered bundled checker, including skipped and error outcomes, in
registration order. The first increment registers only `golangci-lint`, so it
still emits one result. `KindSimplify` retains its existing schema and contains
no `checks` field.

For `ModeUncommitted`, build `Scope{kind: changed}` from the recursively
expanded `PreparedContext.Paths`, its snapshot component prefixes, and the live
authoritative root. The
golangci-lint adapter associates each existing `.go` path with its nearest
ancestor `go.mod` without crossing the path's containing repository component
and passes only the selected changed files. Final diagnostics must be
normalized against invocation root and filtered to the exact selected
changed-file set.

For `ModeStaged`, recursive initialized-submodule discovery, root-relative
paths, diff rendering, file reads, and fingerprinting must share one staged
review workspace owner in `internal/gitctx`. Before creating the shared
`Scope{kind: changed}`, materialize every component index tree into one
owner-only temporary workspace and set the scope root to that materialization.
All adapters consume the same root, exact prepared paths, and component
prefixes. Never inspect the dirty worktree as staged checker input.

For `ModeCodebase`, create `Scope{kind: codebase}` at the authoritative
repository root with recursively discovered component prefixes and without
inventing changed paths. The golangci-lint adapter discovers
every `go.mod` below that root, including initialized registered submodules,
while excluding Git metadata, vendored module trees, symlink escapes, and
duplicate module roots. It runs modules in deterministic root-relative order
and retains diagnostics below each discovered module. Invocation root need not
contain `go.mod`.

Absolute diagnostic paths, paths escaping invocation root, symlink escapes,
empty paths, and malformed locations are dropped and counted only in internal
diagnostics, not `omitted`.

The existing recursive uncommitted or staged-review fingerprint must be checked
immediately before checker workspace planning, after staged materialization,
and after all helper invocations, before combining the final report. Drift is
an authoritative-snapshot failure and must abort the task; it must not be
downgraded to `status:error`. Codebase mode retains its documented live-scope
behavior and has no launch fingerprint.

## CTR-REVIEW-CHECK-006 — Keep sequencing, progress, and failure explicit

Protects REQ-REVIEW-CHECK-001, REQ-REVIEW-CHECK-006, and
REQ-REVIEW-CHECK-007.

The `(*App).runCodeReview` success path becomes:

```text
prepare authoritative scope and fingerprint
run and validate provider report
strictly decode ReviewReport
if staged:
  verify recursive staged fingerprint
  materialize recursive index snapshot
  verify recursive staged fingerprint
if uncommitted:
  verify recursive uncommitted fingerprint
construct one immutable checks.Scope from mode root and exact paths
run registered checker set in deterministic order:
  adapter plans only inside checks.Scope
  if ineligible: append stable skipped Result without progress or helper
  if eligible:
    publish runtime.status running_static_checks with registered name
    run adapter under task context
    append normalized Result whose name matches registration
verify diff-mode fingerprint after the complete checker set
attach orchestration digest when present
construct and validate typed final report
publish/persist exactly one final event
finish SSE server
```

`runtime.status` is emitted only for launched work. It identifies
`phase=running_static_checks` and the current registered `check` name; total
progress is unknown, so do not invent a percentage. Helper stdout and stderr
remain private and cannot change launcher or wait stdout.

An ordinary check error preserves the expensive model result in the final
combined report. Repository drift, cancellation, final-report validation
failure, event persistence failure, or temporary-file security failure remains
a terminal task error with the existing empty-wait-stdout behavior.

Dry-run must exercise shared scope creation, each registered adapter's
mode-specific planning, the combined final-report encoder, and event
persistence without starting the provider or helpers. Each registered checker
emits a deterministic synthetic finding when eligible and a stable skipped
result otherwise. Simplify dry-run remains unchanged.

## CTR-REVIEW-CHECK-007 — Prove the integration at real boundaries

Protects REQ-REVIEW-CHECK-001 through REQ-REVIEW-CHECK-010.

Required production changes are limited to:

- `go.mod` and `go.sum` for the pinned upstream module;
- `internal/checks` for checker-neutral scope, result normalization, registered
  runner coordination, and private helper dispatch;
- `internal/checks/builtin` for the sole compile-time registration list;
- one bundled adapter owner under `internal/checks/golangci`;
- `internal/tasks/review` for strict provider decoding and typed final report
  combination;
- `internal/gitctx` for recursive staged review snapshots, fingerprints, reads,
  diffs, and safe index materialization;
- `internal/cli/app.go` for sequencing;
- one private checker-set command branch in `internal/cli`, not
  `cmd/git-agent/main.go`;
- platform-specific helper cancellation files only if the existing process
  primitives cannot terminate descendants safely;
- `internal/cli/reviewtest.go` for deterministic dry-run final shape;
- focused tests beside those owners;
- `docs/spec.md` and `README.md` after code behavior is settled.

Do not modify shell completions because there is no public command or flag. The
runner and helper interfaces are production extension boundaries, not test
hooks. Test the coordinator with fake runners defined only in test files and
test the concrete adapter with temporary Go repositories.

Minimum proof matrix:

| Case | Required observation |
| --- | --- |
| Two registered fake checkers | Both receive the same immutable scope and results preserve registration order |
| Ineligible registered checker | Stable skipped result, no progress callback, and no helper launch |
| Duplicate or malformed registration | Checker set construction fails before review execution |
| Unknown/future private helper name | Dispatch rejects it without invoking another adapter |
| Runner returns a mismatched name or malformed result | Coordinator rejects it as a terminal contract violation |
| Unrelated files for one checker | That checker cannot broaden exact changed scope and other checker results remain independent |
| No golangci config | Upstream standard defaults run; Git-agent creates no config |
| Repository config | Upstream discovers it without Git-agent path search |
| Passing uncommitted Go change | Only changed files in their containing modules are checked; `checks[0].status=pass` |
| Passing staged Go change | Isolated recursive index materialization is checked; unstaged bytes are absent |
| Codebase with root and nested modules | Every module runs independently in deterministic order |
| Invocation root without `go.mod` | Nested eligible modules still run; no root `./...` fallback |
| In-scope issue | Compact exact mode-authoritative diagnostic and `status=findings` |
| Out-of-scope issue | Removed before final report |
| More than 100 unique in-scope issues | Deterministic first 100 plus exact `omitted` |
| Invalid config or package load | Model report preserved plus bounded `status:error` |
| Non-Go diff or Go file outside a module | `skipped` when no eligible target; helper not started |
| Recursive submodule changes | Root-relative changed files run in their nearest containing submodule module |
| Simplify | Existing output contract unchanged |
| Orchestration manifest | Digest remains present beside `checks` |
| Worktree/index/submodule drift during diff-mode check | Terminal rerun error, no final combined report |
| Cancellation | Helper and descendants stop; wait fails with empty stdout |
| Repeated wait | Byte-equivalent logical combined JSON |
| Repository output safety | No source fix or report artifact appears in checkout |
| Dry run | Deterministic combined final payload without provider or real checker |
| Launch contract | Exactly one launch JSON object and successful launch stderr empty |

Run dependency-size and clean-build measurements before accepting the pinned
module into the release artifact. Record the before/after binary size and build
duration in implementation evidence; a material release-budget decision is not
hidden inside this architecture document.
