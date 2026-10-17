# AGENTS.md

## Project Structure

- `cmd/git-agent/`: CLI entrypoint.
- `internal/cli/`: argument parsing, command dispatch, stdout/stderr behavior.
- `internal/config/`: flag, environment, auth, and provider configuration.
- `internal/agent/`: Go task preparation, validation, and fixed Node runtime bridge.
- `runtime/`: Agents SDK orchestration, provider adaptation, and isolated JavaScript executor.
- `internal/openai/`: shared provider types and official OpenAI Go SDK embeddings adapter.
- `internal/guidance/`: AGENTS/CLAUDE-family project guidance discovery and
  rendering.
- `internal/gitctx/`: typed Git repository inspection and context collection.
- `internal/contextpack/`: compaction helpers for generated-heavy or large
  context.
- `internal/explore/`: synchronous exploration batching and durable follow-up
  sessions.
- `internal/tools/`: curated read-only model tool registry and tool envelopes.
- `internal/jsonx/`: encoding/json/v2 helpers for number preservation and extra-value detection.
- `internal/textutil/`: text normalization and output shaping helpers.
- `internal/trace/`: human console trace recording and rendering.
- `docs/spec.md`: behavioral specification and execution-flow diagrams.
- `README.md`: user-facing command, configuration, build, and debug
  documentation. DO NOT REPEAT CONTRACT FROM SPEC TO README.
- `completions/git-agent.*`: shell completions; current support is fish.
- `.shadowtree.toml`: Go profile and installation recipe.
- `bin/`: untracked local build output.
- `internal/agent/dist/runtime.mjs`: tracked generated bundle shipped in Go
  modules. Regenerate with `npm run build` when runtime sources or dependencies
  change; edit the owning sources in `runtime/` instead of the bundle.

## Constraints

- Use `README.md` and `docs/spec.md` as the source of truth before changing
  behavior.
- When behavior, commands, flags, output contracts, or architecture change,
  update the related docs in the same patch.
- When CLI flags change, update every related surface in the same patch:
  `docs/spec.md` for the normative contract, `README.md` for user-facing usage,
  `internal/cli/app.go` help text and related tests, and
  `completions/git-agent.*` when completion candidates change.
- Keep Git inspection in typed Go code. Outside tests, `exec.Command*` is limited
  to native commit execution in `internal/cli`, fixed skill commands in
  `internal/skillcmd`, and the embedded Node runtime in `internal/agent`.
  Model-generated JavaScript runs only inside the isolated interpreter.
  New subprocess capabilities require user authorization.
- Do not add write-capable model tools, arbitrary shell tools, or generic
  "run any git command" tools unless the user explicitly asks for that design
  change.
- Keep Responses provider adaptation in `runtime/provider.ts` and embeddings
  adaptation in `internal/openai`. Let the SDK own transport and conversation management.
- Do not change documented stdout/stderr contracts casually; update tests and
  docs when changing them.
- Do not log API keys, bearer tokens, or auth files in traces, debug output, or
  errors.
- Store long model prompts in the owning package's `prompts` directory; shared
  loop prompts belong in `internal/agent/prompts`. Do not hardcode them in Go code.
- Prefer tests that use temporary repositories and fake servers over tests that
  depend on local Git configuration, network access, or real provider calls.

## UX

For long running process, MUST INTEGRATE PROGRESS REPORTING. E.g.:

- (Re)Indexing
- Git sync
- Migration
