# Branched review and simplify requirements

This document specifies model-directed parallel branches for the existing
detached `review` and `simplify` workflows. The shipped command and report
contracts remain in `docs/spec.md`.

## REQ-BRANCH-001 — Keep branching inside the existing commands

Git-agent must expose branching as a model tool during normal provider steps
of initial and follow-up `review` and `simplify` conversations. It must not
expose `branch` during dry-run event generation, forced finalization,
schema-repair requests, or any host-side merge stage.

Branching must not add a command, flag, environment variable, persistent
setting, compatibility alias, or second public task.

The existing launch JSON, authenticated SSE endpoint, terminal record,
`--wait`, public report schema, repository mode, read-only tool policy, and
provider configuration remain authoritative for the whole invocation.

## REQ-BRANCH-002 — Bound the branch tree by inspection depth

The existing public `balanced` depth is the medium policy for branching. The
maximum zero-based conversation depth and maximum immediate children of one
branch call are:

| Inspection depth | Maximum conversation depth | Maximum children |
|---|---:|---:|
| `fast` | 1 | 2 |
| `balanced` | 1 | 3 |
| `thorough` | 2 | 4 |

The initial conversation is depth 0, its children are depth 1, and their
children are depth 2. The `branch` tool must be absent when the current
conversation is already at its maximum depth.

These tree dimensions are the complete branch-concurrency policy. Git-agent
must not add an independent global concurrency limit or an application-level
queue. Every child accepted by a branch call becomes immediately eligible to
run in parallel. A maximally expanded thorough tree may contain 21 inspection
conversations and may have 16 depth-2 conversations active concurrently.

## REQ-BRANCH-003 — Publish a bounded model catalog to the agent

When branching is available, the agent must receive a strict, argument-free
`branch_help` tool whose description is exactly
``Use before deciding to use `branch` ``. Its successful result carries the
model catalog, the difficulty-to-effort mapping, and the model and effort
values allowed for the current command. The initial catalog is:

| Model | Suitable jobs |
|---|---|
| `gpt-5.6-sol` | Defect review, correctness, security, reliability, performance, concurrency, lifecycle, and invariant-heavy analysis |
| `gpt-5.6-terra` | Behavior-preserving simplification, reuse, clarity, efficiency, overengineering, and redundant-state analysis |
| `gpt-5.6-luna` | General or mixed inspection and inheritance when selected by current configuration |

An arbitrary model selected by `--model` or `OPENAI_MODEL` remains supported by
inheritance. The branch agent may select `inherit` or a listed catalog model;
it must not invent another model name.

The catalog must also supply this difficulty-to-effort guidance:

| Scope difficulty | Reasoning effort |
|---|---|
| Local, direct, or mechanical | `low` |
| Cross-file, caller/test, ordinary correctness, or structural reuse | `medium` |
| Security, concurrency, lifecycle, data integrity, state machine, or cross-boundary invariant | `high` |
| Exceptional cryptographic or adversarial multi-boundary analysis | `xhigh` |

Simplification must not select `xhigh`; its `branch_help` result therefore omits
`xhigh` from the allowed values while retaining the complete mapping. Model and
reasoning overrides are per child and otherwise inherit from the parent.

## REQ-BRANCH-004 — Branch only independently reviewable large work

The `branch` tool description and task instructions must direct the agent to
branch only when the remaining work is large enough to benefit from parallel
inspection and can be divided into at least two independently reviewable
responsibilities.

The agent must not branch merely to obtain multiple opinions about the same
scope, repeat an inspection, or increase reasoning effort. It should inspect
enough repository inventory to name useful scopes before branching and should
call the tool before doing substantial private work that would not be owned by
a child.

## REQ-BRANCH-005 — Require natural-language scope and keep paths advisory

Every child must have a nonempty natural-language `scope`. The scope states the
child's reporting responsibility and should distinguish it from sibling
responsibilities.

Every child also receives `path_hints`, an array of repository-relative file or
directory names intended only to accelerate discovery. The array may be empty.
Path hints:

- are not an allowlist or evidence boundary;
- do not need to cover every file the child may inspect or cite;
- do not prevent a child from reading or reporting relevant evidence elsewhere
  in the command's authoritative repository scope;
- do not need to form a complete or disjoint partition; and
- must not be used by final-report validation.

Overlap is discouraged through agent instructions, not through semantic host
validation. The host validates only the branch argument shape, child count,
nonempty scope, model and effort values, and safe repository-relative syntax
for nonempty hints.

## REQ-BRANCH-006 — Retire the calling conversation

An accepted `branch` call permanently retires the calling conversation. The
calling model does not wait for child results, receive a normal tool result, or
resume after the children finish.

Each child continues from the task context available at the branch point and
receives a host-framed branch result containing:

- an internal branch identity;
- the text `In scope: <scope>`;
- its advisory path hints;
- sibling scopes for overlap avoidance;
- its selected model and reasoning effort; and
- its zero-based depth.

The public detached task remains alive as coordinator even though the root
model conversation has retired.

## REQ-BRANCH-007 — Run accepted children in parallel

After the complete branch call passes structural validation, Git-agent must
start all immediate children without an additional concurrency gate. Nested
branch calls follow the same rule.

All children share the invocation's cancellation and optional overall timeout.
Staged and uncommitted children retain the prepared authoritative snapshot and
fingerprint behavior. Codebase children retain the existing live-codebase
behavior.

Each child starts with fresh copies of the invocation's selected model-step and
local-tool ceilings. An explicit `--max-steps` therefore applies to every
inspection conversation, not to the sum of the tree. Existing
hosted-capability policy, including any web-search cap, applies independently
to each child. Branching may multiply aggregate provider work up to the bounded
tree size.

## REQ-BRANCH-008 — Preserve scoped review behavior through the fork

Each child continues from the forked provider-visible conversation and the
selected branch function result. Git-agent must not append a child-specific
developer instruction. The inherited command request and branch result contain
the child responsibility, sibling responsibilities, path hints, report
contract, and evidence rules needed to continue the inspection.

Review and simplify children retain their inherited command semantics and
strict report schema. When depth remains, they also retain the branch
interfaces and may fork again.

## REQ-BRANCH-009 — Merge leaf reports without another model review

After every required leaf conversation succeeds, Git-agent must construct the
one public final result in-process without calling a reducer or reviewer model.
Merging is mechanical:

- review findings are concatenated in deterministic leaf order, then
  stably ordered by existing severity rules;
- review recommendation is recomputed from the concatenated severities;
- simplify opportunities are concatenated in deterministic leaf order;
- child summaries are concatenated in deterministic leaf order with their
  natural-language scopes;
- no semantic duplicate detection, source mapping, cross-scope coverage proof,
  or cross-scope evidence restriction is performed; and
- no leaf item may be omitted or rewritten by the merger.

The ordinary report-shape and repository-evidence validators still apply to
each leaf report and the mechanically assembled public report. Review checks
run once after the final concatenation and are added through the existing final
report boundary.

## REQ-BRANCH-010 — Fail rather than publish a partial review

Every accepted child is required for successful completion. A provider,
repository-tool, snapshot-drift, cancellation, timeout, or report-validation
failure in any child must make the public detached task fail. Git-agent must
not return the successful siblings as a complete report or produce a false
`APPROVE`.

The task may cancel still-running siblings after the first terminal child
failure. This cancellation is failure cleanup, not a concurrency limit.

## REQ-BRANCH-011 — Stream replayable branch topology and progress

The existing authenticated SSE endpoint, global event IDs, `Last-Event-ID`
replay, and task-level terminal behavior must remain authoritative for the
whole tree. Branching must not create a stream, replay cursor, endpoint, or
public task identity per child.

The stream must publish an accepted fan-out before publishing activity from its
children. It must identify every node with a stable task-local ID, explicit
parent ID, and zero-based depth. Child model activity must be carried under a
new branch-scoped outer event kind rather than reusing current task-level
reasoning, status, tool, hosted-tool, budget, request, or response kinds.

The stream must publish a lifecycle result when a child completes or fails.
Task-level `runtime.status` continues to report aggregate branch creation,
active and completed counts, failure, final concatenation, and normal
post-review checks. The existing outer `final` and `error` events remain the
only events that terminate the public task stream.

A consumer that ignores the new branch event kinds must still receive aggregate
task progress and the final merged report without interleaved child reasoning
or tool activity. A branch-aware consumer such as codex-herdr can use the
topology and scoped events to render independent child activity.

Node IDs and scope text must be bounded, and all scope, summary, tool, status,
and error text remains untrusted. Branch progress must not expose credentials,
hidden reasoning, provider authentication, or a new public control interface.

## REQ-BRANCH-012 — Preserve nonbranched execution

When the model returns a final report without calling `branch`, review and
simplify must follow the existing single-conversation path. Provider-local
parallel function calls remain disabled; branch parallelism occurs between
independent conversations only.
