# Review and simplify follow-up requirements

This document specifies fresh follow-up turns for the existing detached
`review` and `simplify` workflows. The shipped command contract is in
`docs/spec.md`.

## REQ-FOLLOWUP-001 — Require a successful parent and prompt

Git-agent must accept:

```text
git-agent review --follow-up <turn-id> <prompt...>
git-agent simplify --follow-up <turn-id> <prompt...>
```

The parent must be a successful real-provider turn from the same command and
project. The prompt is the remaining argv elements joined with one ASCII space
and must contain non-whitespace text. `--` permits a first prompt element that
starts with `-`.

## REQ-FOLLOWUP-002 — Isolate follow-up from ordinary launch options

`--follow-up` must be mutually exclusive with `--wait`, scope modes, ordinary
trailing focus, `--append-prompt`, orchestration artifacts, and explicit
provider or execution overrides. The child inherits only the parent's
`uncommitted`, `staged`, or `codebase` mode.

## REQ-FOLLOWUP-003 — Start one fresh targeted conversation

Every follow-up starts a fresh provider conversation using current
configuration, task policy, discovered guidance, skills, and read-only tools.
The first user message must be exactly one JSON object:

- review: `previous_findings` contains the prior report's findings and `prompt`
  contains the follow-up prompt;
- simplify: `previous_opportunities` contains the prior report's opportunities
  and `prompt` contains the follow-up prompt.

The message must exclude the original prompt, previous summary or
recommendation, prepared context, reasoning, tool transcript, checks, and other
final-report metadata.

## REQ-FOLLOWUP-004 — Inspect current authoritative repository state

The child must prepare current state under the inherited mode and bind normal
read-only tools and validation to it. Staged mode continues to exclude unstaged
bytes. Diff modes permit an empty current diff because the parent item may have
been fixed, committed, removed, or renamed. Existing mode-specific fingerprint
behavior remains authoritative.

## REQ-FOLLOWUP-005 — Preserve detached launch, progress, and wait

An accepted follow-up must use the existing detached task path, allocate a new
task ID, return the existing strict launch JSON, expose an authenticated
replayable SSE endpoint, and support repeatable `<command> --wait <new-id>`.
Successful launch adds no stderr output; wait prints only the strict report.
The session event may identify the parent but must not disclose the prompt or
prior report.

## REQ-FOLLOWUP-006 — Persist only reusable parent metadata

Every real-provider turn must persist its mode and optional parent task ID in
the existing owner-only background record. Dry-run turns remain ineligible
because they have no turn metadata. Follow-up must not persist provider
credentials, conversation contents, copied findings, provider usage, host
locators, lineage records, or a duplicate session-configuration store.

## REQ-FOLLOWUP-007 — Return only currently actionable items

The provider must re-evaluate only the named report's findings or
opportunities. Resolved or inapplicable items are omitted. Review may report a
regression directly caused by the attempted fix, but neither command may
expand into an unrelated full inspection. Empty findings or opportunities
means the targeted follow-up succeeded.

## REQ-FOLLOWUP-008 — Reuse existing failure and cancellation behavior

Parent metadata and report shape must be validated before the launcher accepts
the child. After launch, the existing detached workflow owns startup,
heartbeat, provider failure, cancellation, terminal persistence, SSE closure,
and repeatable wait behavior. Current review checks run after the provider
report through their existing boundary; previous check results are never
included in the provider prompt.
