# git-agent specification

## 1. Purpose and non-goals

### Purpose

`git-agent` replaces shell-heavy Fish `gen_*` wrappers with a standalone Go
binary that:

- gathers Git and repository context without shelling out to ad hoc scripts
- uses an OpenAI-compatible Responses API client
- runs a bounded, read-only, tool-calling agent loop
- emits only the final commit message or release note on stdout
- preserves project guidance behavior close to Codex for AGENTS-family files

Primary initial workflows:

- `git-agent commit-msg`
- `git-agent commit-msg --amend`
- `git-agent release-note <base> <release>`

### Non-goals

v1 must not:

- execute arbitrary shell commands on behalf of the model
- merge AGENTS-family and CLAUDE-family guidance into the same prompt
- implement provider-specific plugins beyond OpenAI-compatible HTTP
- add write-capable repository tools
- preserve exact raw `git` CLI output byte-for-byte when a typed Go equivalent
  is clearer and stable

## 2. User-facing commands

### Commands

#### `git-agent commit-msg`

Generate a commit message from the staged diff in the current repository.

#### `git-agent commit-msg --amend`

Generate a commit message for the final post-amend commit result, not a delta
note about the newly staged changes.

#### `git-agent release-note <base> <release>`

Generate a GitHub release body for the range from `<base>` to `<release>`.

### Flags

All subcommands reserve this shared flag surface:

- `--model`
- `--base-url`
- `--timeout`
- `--max-steps`
- `--guidance-family`
- `--debug`

`commit-msg` additionally supports:

- `--amend`

### Environment variables

v1 uses only standard OpenAI-compatible environment variables:

- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`
- `OPENAI_MODEL`

Resolution order:

1. explicit CLI flag
2. environment variable
3. internal default when defined by that subsystem

### stdout / stderr contract

- stdout: final generated artifact only
- stderr: diagnostics, debug output, validation failures, provider/tool loop
  traces when `--debug` is enabled

### Exit behavior

Nonzero exit codes are returned for:

- invalid CLI arguments
- missing repository context
- missing required environment configuration
- provider/API failures
- tool execution failures
- validation failures that cannot be repaired

## 3. Architecture

### Package map

- `cmd/git-agent`: process entrypoint
- `internal/cli`: argument parsing and command dispatch
- `internal/config`: environment and flag materialization
- `internal/agent`: bounded agent loop contract
- `internal/openai`: OpenAI-compatible Responses API client
- `internal/guidance`: project guidance discovery and rendering
- `internal/gitctx`: typed repository inspection
- `internal/tools`: curated read-only tool registry
- `internal/tasks/commitmsg`: commit message behavior
- `internal/tasks/releasenote`: release note behavior
- `internal/textutil`: shared normalization and output shaping helpers

### Request assembly layers

Every task request is assembled in this order:

1. system prompt
2. developer-style project guidance block
3. task-specific user prompt
4. tool registry for that task

The project guidance block is not treated as ordinary user text. It is a
separate injected layer mirroring Codex’s style.

### Agent loop lifecycle

1. resolve config and repo context
2. resolve project guidance for the task target path
3. build task-specific system prompt and initial user prompt
4. send request to the Responses API
5. if the model requests tools, execute only registered read-only tools
6. append tool results and continue until final text is returned
7. validate output against task rules
8. if invalid and repair budget remains, run exactly one repair pass
9. print final text to stdout

### Bounded execution

The runtime must enforce:

- maximum model steps
- maximum tool calls
- maximum bytes/lines per tool result
- per-request timeout
- overall task timeout

## 4. Guidance resolution

### Goal

Follow Codex-style scoped project guidance formatting while preserving a
single-family rule:

- same-family scoped files may concatenate
- different-family files never concatenate

### Family precedence

Default family selection:

1. AGENTS-family
2. CLAUDE-family fallback if and only if no AGENTS-family guidance was found
3. no guidance if neither family is present

### Family membership

Initial AGENTS-family candidates:

- `AGENTS.override.md`
- `AGENTS.md`

Initial CLAUDE-family candidates:

- `CLAUDE.md`

Future fallback filenames may be added per family, but each family keeps its own
precedence chain.

### Scope discovery

Guidance resolution walks from repository root to the target directory in order.
For each directory in that chain:

1. choose at most one file from the active family using that family’s filename
   precedence
2. append it to the resolved source list

Example:

- `/repo/AGENTS.md`
- `/repo/frontend/AGENTS.md`
- `/repo/frontend/admin/AGENTS.md`

For a target inside `frontend/admin`, all three files are concatenated in that
order.

Example of disallowed cross-family merge:

- `/repo/AGENTS.md`
- `/repo/frontend/CLAUDE.md`

Result: choose AGENTS-family only, ignore CLAUDE-family entirely.

### Rendered format

The injected guidance block uses a Codex-style outer wrapper:

```text
# AGENTS.md instructions for /absolute/target/path

<INSTRUCTIONS>
<PROJECT_DOC path="/repo/AGENTS.md">
...
</PROJECT_DOC>

<PROJECT_DOC path="/repo/frontend/AGENTS.md">
...
</PROJECT_DOC>
</INSTRUCTIONS>
```

Notes:

- the heading remains `AGENTS.md instructions for ...` for parity with Codex’s
  visible wrapper shape
- the chosen family may still be CLAUDE-family under the hood
- inner path tags preserve provenance and scoped boundaries

### Guidance target path

Guidance must resolve against the task target path, not blindly against process
cwd.

Task defaults:

- `commit-msg`: current worktree path / repository root context
- `release-note`: current repository root unless a future `--path` override is
  added

## 5. Tool system

### Principles

- read-only only
- typed tool contracts
- no arbitrary shell access
- no generic “run any git command” escape hatch
- bounded output with explicit truncation markers

### Shared repository tools

Planned shared tools:

- `repo_summary`
- `list_files`
- `read_file`
- `search_files`

### Commit message tools

Planned commit message tools:

- `git_staged_paths`
- `git_staged_status`
- `git_staged_stat`
- `git_staged_diff`
- `git_recent_commits`
- `git_head_show`
- `git_diff_against_parent`
- `git_amend_delta`
- `git_show_file_at_rev`

### Release note tools

Planned release-note tools:

- `resolve_ref`
- `git_log_range`
- `gitmodules_table`
- `submodule_gitlink_range`
- `submodule_log_range`
- `repo_kind`

### Tool I/O expectations

Each tool definition must provide:

- stable tool name
- description
- JSON schema for arguments
- plain JSON or plain text result with stable fields
- explicit truncation metadata when output is capped

### Limits

Each tool result must honor caps for:

- bytes
- lines
- entries
- nested commit/submodule log counts

The model must be told when output was truncated so it can request narrower
follow-up reads.

## 6. Task behavior

### Commit message: normal mode

Behavior:

- inspect the staged diff only
- treat staged paths as authoritative scope
- use recent commit history as style reference only
- allow the model to request extra related file reads when the diff is
  ambiguous

Output rules:

- subject line first
- blank line before body only when body exists
- no fences
- no explanations
- body lines wrapped to target width

### Commit message: amend mode

Behavior:

- describe the final amended commit as one commit versus its parent
- never narrate the amended result as “previous commit plus extra changes”
- treat the full amended diff versus parent as authoritative when available
- use current HEAD and staged-vs-HEAD views only as diagnostic inputs

Output rules:

- one narrative only
- no delta/process phrasing such as “also”, “this amend”, or “in addition”
- preserve task IDs or scope markers only when still supported by the final
  diff

### Release note generation

Behavior:

- peel and validate both refs
- generate a parent-repository commit log for the selected range
- inspect submodule gitlink changes
- include submodule commit groups only when the gitlink moved and local commit
  history is available
- optimize prose for deployers/operators rather than developers

Output rules:

- first printable line starts with `### `
- no preamble
- no duplicate section narratives
- include `### Full Changelog` when the range touched code
- parent-repo commits appear first in the full changelog
- submodule groups appear after parent commits
- commit/repo links must follow repository ownership rules

### Validation

Each task owns a validator.

Commit message validator checks at minimum:

- non-empty output
- no code fences
- subject present
- no stray commentary

Release note validator checks at minimum:

- first printable line starts with `### `
- no forbidden preamble
- heading/content structure valid
- `### Full Changelog` included when required

### Repair strategy

If validation fails:

1. summarize the validation errors
2. run one repair pass through the model
3. revalidate
4. return an error if still invalid

## 7. Implementation phases

### Phase 1: skeleton and spec

- create module and directory structure
- define CLI surface
- document package boundaries
- author this spec

### Phase 2: config and CLI materialization

- parse env and flags into `config.Config`
- add shared error shaping
- add debug plumbing

### Phase 3: Git context layer

- open repository/worktree
- collect branch/head metadata
- implement staged and range inspection helpers
- add submodule traversal helpers

### Phase 4: guidance resolver

- implement family selection
- implement scoped discovery
- implement Codex-style rendering
- add provenance-aware tests

### Phase 5: OpenAI-compatible client

- build Responses API request/response layer
- support tool calls
- add timeout and retry boundaries where appropriate

### Phase 6: tool loop

- register typed tools
- run bounded dispatch loop
- append tool results back into the conversation

### Phase 7: task validators and prompts

- implement commit message prompts and validation
- implement release note prompts and validation
- add one-pass repair flow

### Phase 8: Fish migration

Outside this repo, update Fish wrappers to:

- point default `gen-*` commands to `git-agent`
- rename old defaults to `*-claude`
- remove `*-codex`
- keep temporary `*-agent` compatibility shims if desired

## 8. Testing strategy

### Unit tests

Unit coverage should include:

- prompt normalization
- CLI parsing
- guidance family selection
- guidance scoped ordering
- validator rules
- truncation metadata

### Golden tests

Golden tests should cover:

- commit message prompt/context assembly
- amend prompt/context assembly
- release note prompt/context assembly
- guidance rendering blocks

### Fake API server tests

Use a local fake OpenAI-compatible server to test:

- tool-call round trips
- finish states
- validation repair pass behavior
- malformed provider responses

### Integration tests

Use temporary repositories to test:

- staged commit message generation scenarios
- amend scenarios
- detached HEAD
- root commit handling
- release-note tag/range handling
- submodule gitlink movement and missing checkout cases

## 9. Risks and open constraints

### go-git fidelity risk

Index and diff fidelity may not perfectly mirror `git` CLI behavior. This is
most likely to affect amend and submodule-heavy scenarios.

Mitigation:

- write integration tests around real temp repositories
- validate behavior, not raw textual parity

### Provider drift risk

“OpenAI-compatible” providers may diverge in tool-call or Responses API
details.

Mitigation:

- keep the client thin
- isolate provider translation in `internal/openai`
- test against a fake server and at least one real provider

### Release-note formatting regressions

Current Fish prompts encode many hard-earned formatting constraints.

Mitigation:

- carry those constraints into validators
- lock output with golden tests

### Token growth risk

Generic file reads can inflate context quickly.

Mitigation:

- typed tools first
- strict tool output caps
- encourage narrow follow-up reads

## 10. Immediate acceptance criteria for the skeleton

The skeleton phase is complete when:

- repository exists at `~/projects/git-agent`
- module path is `github.com/yusing/git-agent`
- `go test ./...` passes
- CLI entrypoint builds
- internal package boundaries exist
- `docs/spec.md` captures all locked architecture and migration decisions
