# Review static-check requirements

This draft specifies the next review increment. The shipped review contract
remains in `docs/spec.md` until this increment is implemented; that
implementation must reconcile the normative command and output contract there
in the same patch.

## REQ-REVIEW-CHECK-001 — Run checks after the review agent

For an eligible review, Git-agent must run static checks only after the review
agent has returned a schema-valid, repository-validated report. Git-agent must
combine the agent report and check results before publishing or persisting the
terminal final event.

The provider schema must continue to describe only the model-authored review
report. The model must not be asked to fabricate, summarize, or deduplicate
static-check results.

## REQ-REVIEW-CHECK-002 — Bundle golangci-lint

The first increment must support Go through golangci-lint only. Git-agent must
link the tested golangci-lint implementation into its distribution and must not
require, discover, install, or download a separate `golangci-lint` executable
at review time.

## REQ-REVIEW-CHECK-003 — Delegate configuration to golangci-lint

Git-agent must delegate golangci-lint configuration-file discovery and
zero-configuration defaults to golangci-lint. Git-agent must not implement a
parallel search for `.golangci.yml`, `.golangci.yaml`, `.golangci.toml`, or
`.golangci.json`, generate a default configuration, or define its own default
linter set.

Repository, ancestor, home-directory, invalid-configuration, and no-file
behavior must therefore match the bundled golangci-lint version.

## REQ-REVIEW-CHECK-004 — Add compact checks to the report

The final review JSON must preserve the existing top-level `summary`,
`recommendation`, and `findings` fields and their current semantics. It must add
a required top-level `checks` array and must continue preserving the optional
`orchestration_manifest_sha256` field when orchestration evidence was supplied.

Each registered check must contribute one result containing its stable `name`,
`status`, and `diagnostics`. `status` must be one of `pass`, `findings`,
`skipped`, or `error`. `omitted`, `reason`, and `error` must appear only when
applicable; `skipped` must include a stable nonempty `reason`. Static-check
diagnostics must not be converted into model findings or alter the
model-authored recommendation.

## REQ-REVIEW-CHECK-005 — Bound, locate, and scope diagnostics

Each retained diagnostic must contain only a repository-relative `path`,
positive `line`, optional positive `column`, optional nonempty `code`, and
nonempty single-line `message`. The enclosing check `name` identifies the
engine; `code` identifies an engine-specific rule or sub-check when one exists.

Git-agent must normalize and deterministically sort diagnostics, retain only
paths in the authoritative scope selected by the review mode, retain at most
100 diagnostics, limit each normalized message to 512 bytes, and report the
number of omitted in-scope diagnostics.

For `--uncommitted` and `--staged`, the eligible diagnostic scope is exactly
the changed `.go` files from the same recursively expanded repository and
submodule snapshot used to prepare the review diff and context pack. For
`--codebase`, the eligible diagnostic scope is every Go source path below each
discovered module in authoritative repository scope.

## REQ-REVIEW-CHECK-006 — Report progress and propagate cancellation

Before starting each eligible registered checker, Git-agent must publish a
`runtime.status` event with `phase=running_static_checks` and that checker's
stable `check` name. Ineligible checkers must not produce false running
progress.

Cancellation of the detached review task must cancel the active check and
remain a terminal task error. Check progress and diagnostics must never add
human text to the strict launcher or `review --wait` stdout contracts.

## REQ-REVIEW-CHECK-007 — Preserve useful results on check failure

No diagnostics must produce `status: pass`; one or more retained or omitted
diagnostics must produce `status: findings`.

Configuration, package-loading, analysis, helper-protocol, or result-decoding
failure must preserve the already validated model report and produce
`status: error` with a safe single-line error limited to 1 KiB. Context
cancellation is not a check result and must fail the task.

## REQ-REVIEW-CHECK-008 — Keep checking read-only

Git-agent must explicitly disable golangci-lint fixes and override every output
destination supported by the pinned golangci-lint version so repository or home
configuration cannot write source fixes or report artifacts into the reviewed
checkout.

Git-agent must retain only a private temporary JSON result, bound helper output
to 16 MiB, and remove its temporary artifacts after parsing on success,
failure, or cancellation.

Staged checks must run against an owner-only temporary materialization of the
recursive index snapshot. They must not read unstaged or untracked worktree
bytes. Materialization failure, unsafe paths, snapshot drift, and cleanup
failure are terminal task errors rather than check results.

## REQ-REVIEW-CHECK-009 — Respect review modes and Go module boundaries

Git-agent must support golangci-lint for `--uncommitted`, `--staged`, and
`--codebase` without broadening the selected review scope.

For `--uncommitted` and `--staged`, Git-agent must start from the authoritative
changed paths, keep only existing `.go` files, associate each file with its
nearest containing `go.mod`, and invoke golangci-lint only for those changed
files. The authoritative changed-path set and snapshot fingerprint must include
initialized registered submodules recursively, using the same ownership and
root-relative path rules as review diff preparation. Changed Go files outside a
Go module are ineligible and must not cause a checker launch.

For `--codebase`, Git-agent must discover every `go.mod` in authoritative
repository scope, including modules inside initialized registered submodules,
and run each module independently. It must not assume the invocation root is a
module or replace multi-module discovery with one root-level `./...` run.

When no mode-eligible Go module and Go source target exist, Git-agent must
return one explicit `skipped` result with a stable reason and must not start the
helper. Module and file ordering must be deterministic.

## REQ-REVIEW-CHECK-010 — Isolate checker-specific behavior

Static-check scope, result normalization, deterministic checker ordering,
progress, cancellation, final-report combination, and private-helper dispatch
must be checker-neutral. The first increment registers only bundled
golangci-lint, but adding another bundled checker or language must require an
adapter and one registration rather than changes to provider schemas, Git
snapshot ownership, detached review sequencing, background persistence, or
wait output.

The shared scope must preserve the recursively discovered repository-component
prefixes from the same authoritative snapshot. An adapter may discover
language projects within a component, but must not cross a submodule boundary
while searching ancestors for project configuration.

This extension boundary must not expose arbitrary commands, runtime plugin
loading, public helper commands, or model-callable check tools. Every adapter
must still validate its own targets and helper protocol against the shared
authoritative scope.
