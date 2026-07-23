# Review and simplify follow-up architecture

This document is the implementation contract for `SLICE-FOLLOWUP-001`. It
protects `REQ-FOLLOWUP-001` through `REQ-FOLLOWUP-008` by reusing the existing
detached review/simplify workflow.

## CTR-FOLLOWUP-001 — Route through the existing detached command

Protects REQ-FOLLOWUP-001, REQ-FOLLOWUP-002, and REQ-FOLLOWUP-005.

`internal/cli` parses the isolated `--follow-up` form. Before spawning, it opens
the current project store and validates the parent command, successful terminal
report, and turn metadata. The normal `startDetachedTask` path allocates the
child ID and waits for the child to create its durable record and advertise its
per-turn event endpoint. No second host, control socket, retry protocol, or
session lifetime is introduced.

The detached child repeats parent validation before provider work. This is
intentional: launcher validation gives immediate errors, while child validation
does not trust cross-process state.

## CTR-FOLLOWUP-002 — Keep durable state minimal

Protects REQ-FOLLOWUP-001, REQ-FOLLOWUP-006, and REQ-FOLLOWUP-008.

`internal/background.Record` version 3 may contain `TurnMetadata` with only
`mode` and optional `parent_id`. `AttachTurn` validates and atomically writes
that metadata through the existing record lock. Version 1 and 2 records remain
readable but cannot be follow-up parents because they lack turn metadata.

Do not create lineage, conversation, host, capacity, provider-usage, credential
identity, or session-configuration records. Historical successful parents may
be followed more than once; each invocation is an independent current-state
inspection.

## CTR-FOLLOWUP-003 — Construct one fresh user message

Protects REQ-FOLLOWUP-003 and REQ-FOLLOWUP-007.

`internal/tasks/review.FollowUpPrompt` validates the stored public report and
constructs the only user message for the fresh conversation. Review copies only
`findings`; simplify copies only `opportunities`; both add the canonical
prompt. Summary, recommendation, checks, orchestration digest, prepared
context, and provider transcript are omitted.

The existing `OpenAIRunner.Run` remains stateless. Follow-up does not add a
session API, provisional turn type, conversation commit protocol, provider
usage aggregation, or context-window router.

## CTR-FOLLOWUP-004 — Reprepare current mode-authoritative state

Protects REQ-FOLLOWUP-004 and REQ-FOLLOWUP-007.

`internal/tasks/review.PrepareFollowUp` reuses normal preparation while allowing
an empty diff. `internal/cli` then resolves current guidance and skills, builds
the normal mode-scoped registry, plans the normal budget, and validates against
the current fingerprint. Staged mode continues to use staged bytes; codebase
mode continues to inspect live repository state.

## CTR-FOLLOWUP-005 — Preserve report and event sequencing

Protects REQ-FOLLOWUP-005 and REQ-FOLLOWUP-008.

The existing event server, heartbeat, trace recorder, provider execution,
review-check boundary, final report assembly, terminal record, and `--wait`
implementation remain the sole owners. The session event adds only `parent`.
The prompt and prior report never become progress or diagnostic fields.

Initial and child real-provider turns attach metadata before provider work.
Dry-run follows its existing deterministic path without metadata.

## CTR-FOLLOWUP-006 — Prove the reduced boundary

Protects REQ-FOLLOWUP-001 through REQ-FOLLOWUP-008.

Focused tests must prove:

- missing prompts and conflicting flags fail before launch;
- command mismatch, unsuccessful records, missing metadata, and invalid modes
  are rejected;
- review and simplify prompts contain only prior actionable items and the new
  prompt;
- a detached child inherits the parent mode, uses current repository state,
  persists its parent ID, and completes through normal SSE/report storage;
- empty diff follow-up is valid while normal empty diff launch still fails;
- initial real-provider turns receive metadata and dry-run turns do not;
- record versions 1 and 2 remain readable;
- normal static checks, cancellation, terminal errors, and repeatable wait
  behavior remain unchanged.
