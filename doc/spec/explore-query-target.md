# Explore query-target requirements

This document specifies use-case guidance for synchronous exploration. The
shipped command contract is normative in `docs/spec.md`.

## REQ-EXPLORE-FOR-001 — Select a query target

Git Agent must accept the public `--for` surface defined in
[`doc/brief.md` § Explore query targets](../brief.md#explore-query-targets).
Omitting `--for` must retain the existing universal exploration guidance.
`--fast` must continue to affect only `service_tier=priority`.

The targets must guide the answer toward:

- `diagnose`: the reproducer, immediate failure mechanism, and bottleneck or
  regression cause;
- `change`: the implementation boundary, affected behavior, and focused
  validation needed for a change;
- `behavior`: current semantics, contracts, and invariants;
- `owner`: the authoritative implementation, callers, and subsystem boundary.

## REQ-EXPLORE-FOR-002 — Preserve target through follow-ups

A context-preserving follow-up without `--for` must inherit its parent's query
target. When `--for` selects a different target, Git Agent must append exactly
one replayable developer message using the changed-target prefix required by
[`doc/brief.md` § Query-target constraints](../brief.md#query-target-constraints),
followed by that target's guidance. It must not rewrite Responses `instructions`,
replace the prompt-cache key, discard prior input, or trigger a fresh semantic
search solely because the target changed.

Supplying the already-active target must behave like inheritance and must not
append a duplicate target-change message. An existing depth-limit reset remains
a fresh search with a new cache key and uses the selected target as its initial
guidance. A stored session created before query targets existed must remain
follow-up eligible and behave as a universal-target session.

## REQ-EXPLORE-FOR-003 — Reject unsupported targets before work

A missing or unsupported `--for` value must return a nonzero usage error before
semantic retrieval or a provider request. Help, normative documentation,
user-facing usage, tests, and fish completion must expose the accepted values
owned by the brief's first-draft scope.

## REQ-EXPLORE-FOR-004 — Keep history collection out of runtime

Query-target selection must not read Codex session history or `~/.codex` at
runtime. The local history scan informed the fixed initial target vocabulary
only; it does not add telemetry, prompt logging, or a discovery dependency.

## REQ-EXPLORE-FOR-005 — Batch only compatible targets

A provider batch must contain only searches with the same service tier, parent
identity, and selected query target. Initial searches with different targets and
sibling follow-ups that select different targets must use separate provider
requests because each batch has one stable system prompt and one replay parent.
