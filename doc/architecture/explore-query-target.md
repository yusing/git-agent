# Explore query-target architecture

This contract protects the shared prompt-cache and session-persistence boundary
for REQ-EXPLORE-FOR-001 through REQ-EXPLORE-FOR-005.

## ARCH-EXPLORE-FOR-001 — Keep target ownership in `internal/explore`

`internal/explore` owns the closed query-target type, target guidance, initial
system-prompt selection, follow-up target-change input, and persisted active
target metadata. `internal/cli` owns only flag parsing, pre-work validation, and
threading the selected value through the existing coordinator path.

No target behavior belongs in `internal/agent`, semantic search preparation, a
runtime history reader, or a new subsystem.

## ARCH-EXPLORE-FOR-002 — Persist instruction and active targets separately

The existing `internal/explore.Session` schema must retain both the target used
to construct the branch's stable Responses instructions and the currently
active target. Every child copies the instruction target and persists its
resulting active target. The empty value is the universal target and preserves
the compatibility behavior required by REQ-EXPLORE-FOR-002.

The coordinator remains the only writer of completed session records; the
store remains responsible for validation and owner-only atomic persistence.

## ARCH-EXPLORE-FOR-003 — Append target changes at the replay boundary

Initial requests select stable instructions from the initial target. A
context-preserving follow-up reconstructs those same instructions from the
persisted instruction target. When the active target changes,
`internal/explore` appends the single developer message required by
REQ-EXPLORE-FOR-002 to cloned parent history before appending the follow-up user
message.

The existing `agent.Request.Input` and prompt-cache key interfaces remain
authoritative. Target changes must not add mutable instruction handling or
target-specific cache logic to `internal/agent`.

## ARCH-EXPLORE-FOR-004 — Include the selected target in batch identity

The coordinator's compatibility identity must include the selected query target
in addition to workspace, service tier, and parent identity. Every persisted
batch item carries that selected target, and the leader rejects an item whose
target does not match its coordinator identity. This keeps each provider request
on one stable system prompt and one replay parent without disabling compatible
same-target batching.
