# Review and simplify follow-up brief

## Outcome

After addressing a completed `review` finding or `simplify` opportunity, an
operator can launch a fresh detached re-evaluation against current repository
state and retrieve its strict report through the existing SSE and `--wait`
workflow.

## First-draft scope

- `review --follow-up <turn-id> <prompt...>` and
  `simplify --follow-up <turn-id> <prompt...>`.
- A successful same-command, same-project provider turn as parent.
- The parent's review mode, current repository state, current configuration,
  guidance, skills, tools, validation, and checks.
- One fresh user message containing only prior findings or opportunities plus
  the required prompt.
- Existing detached launch, progress, failure, cancellation, storage, and wait
  behavior.

## Non-goals

- Continuing or persisting a provider conversation.
- A session host, control socket, lineage, host epoch, context threshold, or
  inherited provider configuration.
- Preventing multiple independent follow-ups from one historical report.
- Persisting credentials, provider usage, copied findings, or transcripts.
- Full inspection of unrelated changes during targeted follow-up.
- Follow-up from dry-run output.

## Constraints and assumptions

- `--follow-up` is isolated from all ordinary launch flags and focus.
- The remaining argv elements are joined with one ASCII space and must contain
  non-whitespace text.
- Staged mode still excludes unstaged bytes; diff modes allow an empty current
  diff for re-evaluation.
- The provider receives only prior actionable items and the new prompt.
- Version 3 background records store only mode and optional parent ID.
