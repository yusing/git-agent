# AGENTS.md

## Project Structure

- `cmd/git-agent/`: CLI entrypoint.
- `internal/cli/`: argument parsing, command dispatch, stdout/stderr behavior.
- `internal/config/`: flag, environment, auth, and provider configuration.
- `internal/agent/`: bounded Responses API tool-calling loop.
- `internal/openai/`: official OpenAI Go SDK adapter.
- `internal/guidance/`: AGENTS/CLAUDE-family project guidance discovery and
  rendering.
- `internal/gitctx/`: typed Git repository inspection and context collection.
- `internal/contextpack/`: compaction helpers for generated-heavy or large
  context.
- `internal/tools/`: curated read-only model tool registry and tool envelopes.
- `internal/textutil/`: text normalization and output shaping helpers.
- `internal/trace/`: JSON session traces and human console trace rendering.
- `docs/spec.md`: behavioral specification and execution-flow diagrams.
- `README.md`: user-facing command, configuration, build, and debug
  documentation. DO NOT REPEAT CONTRACT FROM SPEC TO README.
- `completions/git-agent.*`: shell completions; current support is fish.
- `.shadowtree.toml`: Go profile and installation recipe.
- `bin/`: local build output; do not treat generated binaries as source.

## Constraints

- Use `README.md` and `docs/spec.md` as the source of truth before changing
  behavior.
- When behavior, commands, flags, output contracts, or architecture change,
  update the related docs in the same patch.
- When CLI flags change, update every related surface in the same patch:
  `docs/spec.md` for the normative contract, `README.md` for user-facing usage,
  `internal/cli/app.go` help text and related tests, and
  `completions/git-agent.*` when completion candidates change.
- Do not use `exec.Command*` outside tests, including for `git`, unless the
  user explicitly asks to change that policy.
- Do not add write-capable model tools, arbitrary shell tools, or generic
  "run any git command" tools unless the user explicitly asks for that design
  change.
- Keep provider-specific request/response translation in `internal/openai`
  unless the user explicitly asks for an architecture change.
- Do not change documented stdout/stderr contracts casually; update tests and
  docs when changing them.
- Do not log API keys, bearer tokens, or auth files in traces, debug output, or
  errors.
- Prefer tests that use temporary repositories and fake servers over tests that
  depend on local Git configuration, network access, or real provider calls.
- DO NOT PRODUCE ANY ARTIFACT TO THE REPO, ESPECIALLY BUILT BINARY HANGING IN THE PROJECT ROOT
