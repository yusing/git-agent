# git-agent specification

## 1. Purpose and non-goals

### Purpose

`git-agent` is a standalone Go binary for Git-related generation workflows.
It:

- gathers Git and repository context without shelling out to ad hoc scripts
- uses the official OpenAI Go SDK against an OpenAI-compatible Responses API
  endpoint
- runs a bounded, read-only, tool-calling agent loop
- emits only the final commit message or release note on stdout for
  message-generation commands
- can optionally create the Git commit after generating a message
- preserves project guidance behavior close to Codex for AGENTS-family files

Supported workflows:

- `git-agent commit-msg`
- `git-agent commit-msg --amend`
- `git-agent commit`
- `git-agent commit --amend`
- `git-agent pr-message`
- `git-agent release-note [--out <file>] <base> <release>`
- `git-agent release-note [--out <file>] patch|minor|major`
- `git-agent search [flags] <query...>`
- `git-agent search --ls [--remote <url>] [--format text|json]`
- `git-agent search --ls-remotes [--format text|json|completion]`
- `git-agent search --ls-files [--format tree|json] [--remote <url>] [--rev <rev>] [--scope <paths>] [--no-tests]`

### Non-goals

`git-agent` must not:

- execute arbitrary shell commands on behalf of the model
- merge AGENTS-family and CLAUDE-family guidance into the same prompt
- implement provider-specific plugins beyond OpenAI-compatible Responses API
  options exposed through the official SDK
- add write-capable repository tools
- preserve exact raw `git` CLI output byte-for-byte when a typed Go equivalent
  is clearer and stable

## 2. User-facing commands

### Commands

#### `git-agent commit-msg`

Generate a commit message from the staged diff in the current repository.
The command precomputes staged paths, status, stats, recent style commits, and
the bounded staged diff before generation so the authoritative staged scope is
visible before any optional follow-up tool calls. For generated-heavy staged
changes, the request may compact dominant generated hunks into a context pack,
but it must still include raw outlier diffs for small handwritten change
clusters. Large or capped staged diffs expose a path-filtered staged-diff tool
so the model can inspect omitted high-churn or secondary clusters without
reading unrelated hunks.

When the staged changes are exclusively submodule gitlink updates, normal
`commit-msg` does not call the model or require provider auth. It formats a
deterministic message from prepared submodule history, using recent commits to
choose conventional style (`chore(deps): update ... submodule`) or Title-case
style (`Update ... submodule`). The body mirrors the release-note submodule
changelog shape with each submodule heading followed by indented
`short-sha: summary` entries. If more than three submodules are staged, the
subject says `submodules` instead of listing every path.

#### `git-agent commit-msg --amend`

Generate a commit message for the final post-amend commit result, not a delta
note about the newly staged changes. The current HEAD commit message is the
anchor for subject, scope, task IDs, and high-level intent; staged cleanups or
refinements must not replace a broad original message with a narrow delta
message. The command precomputes amend context before generation: original HEAD
message, latest HEAD commit metadata, HEAD-vs-parent paths/stats/diff, staged
diagnostics, recent style commits, and the bounded final amended diff versus
HEAD's first parent. This gives the model enough latest-commit context before
any optional follow-up tool calls.

#### `git-agent commit`

Generate a commit message from staged changes using the same prompt,
validation, shaping, guidance, and read-only model tools as `commit-msg`, then
create the commit by running `git commit --file -` in the repository root. This
mode writes no on-disk NDJSON trace. On success, stdout streams a human console
trace while generating the message, then prints Git's raw commit summary after `git commit`
succeeds. Trace lines use short local times such as `15:04:05 INF final`, color
field keys when stdout is a terminal, and render long or multiline values as
indented preview blocks. Because commit creation is delegated to Git, normal Git
config,
hooks, `commit.gpgSign`, system `gpg`, and `gpg-agent` behavior apply. If commit
creation fails after message generation, including because signing fails or a key
is locked, the command returns nonzero, keeps the streamed trace events on
stdout, and reports both the generated message and the Git error so the user can
commit manually.

For normal submodule-only staged changes, `commit` uses the same deterministic
local formatter as `commit-msg`, skips provider auth and trace generation, then
passes the formatted message directly to `git commit --file -`.

#### `git-agent commit --amend`

Generate the final amended commit message using the same semantics as
`commit-msg --amend`, then amend the commit by running
`git commit --amend --file -` in the repository root. The
success stdout contract matches `git-agent commit`: human console trace lines
followed by Git's raw commit summary.
Amend mode preserves the original HEAD author and uses the current configured
committer. The original HEAD subject is validated as the amend message anchor so
model output cannot silently replace it with a staged-delta-only subject. The
message-generation request is seeded with the same prepared amend context as
`commit-msg --amend`.

#### `git-agent pr-message`

Generate a squash merge commit message for the current branch versus
`origin/HEAD`. The command treats the diff from `origin/HEAD` to `HEAD` as the
authoritative scope, precomputes branch evidence before generation, and uses
branch commits as supporting evidence.

#### `git-agent release-note [--out <file>] <base> <release>` or `git-agent release-note [--out <file>] patch|minor|major`

Generate a GitHub release body for the range from `<base>` to `<release>`.
As a shortcut, `patch`, `minor`, or `major` finds the latest reachable semantic
version tag, accepts either `vX.Y.Z` or `X.Y.Z`, strips any `v` prefix, bumps the
requested component, and uses `HEAD` as the release revision for evidence. For
example, `v1.0.0` plus `patch` and `1.0.0` plus `patch` both infer release
version `1.0.1`.
The command precomputes release-note evidence in Go before generation and then
asks the model to write from that prepared context, with only a minimal
read-only fallback tool available for rare gaps.
By default the rendered Markdown is printed to stdout and a JSON trace is
written under `~/.git-agent/<path-sha>/sessions/`. With `--out <file>`, the
command checks the target is writable before generation, streams the human
console trace to stdout, writes the rendered Markdown to the file, and does not
write a JSON trace session.

#### `git-agent search [flags] <query...>`

Run local embedding-backed context search and print machine-readable JSON by
default.
Filesystem mode is the default: it searches current files under the current
working directory exactly as they exist on disk and does not require a Git
repository. Staged, unstaged, and untracked files are included when physically
under the search root unless skipped by dot-path rules, built-in low-signal
ignore patterns, `.gitignore`, `.gitagentignore`, non-text MIME type, or binary,
oversized-file, and symlink safety checks. Built-in search ignores exclude paths
matching `*.lock`, `*.lockfile`, `bun.lock`, `bun.lockb`,
`Cartfile.resolved`, `cabal.project.freeze`, `Cargo.lock`, `composer.lock`,
`conda-lock.yaml`, `conda-lock.yml`, `cpanfile.snapshot`, `deno.lock`,
`flake.lock`, `Gemfile.lock`, `go.sum`, `mix.lock`, `npm-shrinkwrap.json`,
`package-lock.json`, `Package.resolved`, `packages.lock.json`, `pdm.lock`,
`Pipfile.lock`, `pixi.lock`, `Podfile.lock`, `poetry.lock`, `pnpm-lock.yaml`,
`pubspec.lock`, `renv.lock`, `shard.lock`, `stack.yaml.lock`, `uv.lock`,
`yarn.lock`, `*.bazel`, `*.sha256`, `LICENSE`, `COPYING`, or `NOTICE`.
`.gitagentignore` uses the same pattern syntax and per-directory base behavior
as `.gitignore`, but only affects `git-agent search` discovery.
`--scope` accepts comma-separated root-relative file or directory paths and
limits filesystem or revision discovery to those paths. Ignore files are still
resolved from the search root or committed tree, so root `.gitagentignore`
patterns apply normally to scoped paths such as `--scope foo/`. Visible scopes
share the same physical cache as unscoped search for the same source. Scopes
that include paths normally skipped by default discovery, such as dot/hidden
paths, use a separate `scope-*` cache because they opt into a different physical
candidate universe.

Go files with a pre-package heading comment containing `DO NOT EDIT` are indexed
as path-only chunks. Search embeds the filename/language metadata for those
files but excludes generated body content.

`--rev <rev>` switches to revision mode. The command must be inside a Git
repository, resolves the revision to a commit, searches only that committed tree,
and ignores current filesystem contents. Revision mode reads `.gitignore` and
`.gitagentignore` from the resolved commit tree, not from the working tree.

`--remote <url>` switches to remote mode. The command caches the sanitized remote
URL under `~/.git-agent/remotes/<remote-sha>/`, keeps a bare repository at
`repo.git`, resolves `--rev` against that cached repository, and searches the
resolved committed tree. When `--rev` is omitted, remote mode resolves `HEAD`
from the remote default branch and reports `rev` as `HEAD`. Remote mode never
checks out a worktree and never includes untracked, staged, unstaged, or
submodule working-tree content. Cached remote URLs are sanitized before they are
written to manifests, output, debug logs, or completion metadata.

Remote repositories are fetched on first use, when the last successful fetch is
at least 15 minutes old, or whenever `--reindex` is set. Fresh cache hits do not
touch the network. If a requested revision cannot be resolved from the cached
repository, the command fetches and retries before failing. Fetch failures fail
the command clearly rather than silently using stale data.

Search does not run the Responses API, does not call model tools, does not
create `~/.git-agent/<path-sha>/sessions/` traces, does not generate
explanations, and does not use lexical fallback. It frames and embeds the query
as an implementation-location search when the configured embedding input cap can
include the framing; otherwise it embeds the raw query so user query text is not
truncated away. Search embeds local chunks, performs an exact cosine scan over
the local binary vector cache, filters candidates by vector relatedness, then
ranks surviving candidates with a hybrid score that combines vector relatedness,
normalized BM25-style body text overlap, path token overlap, and indexed symbol
token overlap. Output and replay history keep the original query string, not the
framed embedding input.

`--format json` is the default stdout contract. `--format brief` first writes a
header line as `# mode=<filesystem|revision|remote> index=<fresh|refreshed|built|empty>`,
then writes one result per line as `<score> <path>:<start-line> <summary>`, with
the score rounded to two decimals. The score is the final hybrid rank; JSON
results expose the vector relatedness, text, path, symbol, lexical, cosine, and
rank components in `scores`. The summary is the indexed symbol name when
available, otherwise the first excerpt line without its excerpt line-number
prefix. Brief output suppresses low-information Go `package <name>` results when
another result for the same file has an indexed symbol. `--index --format brief`
writes only the header line because indexing skips scoring.

When stderr is an interactive terminal and `--debug` is not enabled, search
shows transient indexing progress while missing embeddings are built or updated.
The progress line is rewritten and cleared with ANSI control sequences before
stdout is written. Non-interactive stderr receives no progress output.
`--agent` starts a local progress probe server instead of terminal progress, but
only when embeddings need to be built or rebuilt. The server listens on
`127.0.0.1:0`, prints its actual `http://127.0.0.1:<port>/progress` URL to stderr,
and returns JSON for `GET /progress` with status, completed chunk count, total
chunk count, reused chunk count, percent, elapsed milliseconds, and last update
time. When `--format` is omitted, `--agent` changes the output format default
from JSON to brief. The server shuts down when the search command exits. Cache-hit
searches do not start the server and do not print a progress URL.

Persistent metadata is stored under `~/.git-agent/<path-sha>/`, where
`<path-sha>` is the SHA-256 of the cleaned absolute project root. Search writes
indexes under `~/.git-agent/<path-sha>/search/`. When a legacy
`<project>/.git-agent/` directory exists, the next project run migrates its
contents into the home metadata directory before writing new data.
Remote metadata is stored under `~/.git-agent/remotes/<remote-sha>/`, where
`<remote-sha>` is the SHA-256 of the sanitized remote URL. Remote search indexes
are stored under that remote metadata root and are keyed by resolved commit SHA,
so moving branches create new revision indexes while old commit indexes remain
reusable.

Chunk embedding text clamps each physical source line to `4000` characters
before applying the per-input embedding character cap. This bounds minified or
single-line generated files without changing file discovery, chunk ranges, or
result excerpts.

`--code` narrows the candidate set for the current search or indexing run to
source-code files before chunking and embedding missing chunks. It is intended
for implementation-location searches where docs would otherwise rank above code.
The filter is extension-based and currently includes:
`.go`, `.js`, `.jsx`, `.ts`, `.tsx`, `.mjs`, `.cjs`, `.py`, `.rb`, `.rs`,
`.java`, `.kt`, `.kts`, `.c`, `.h`, `.cc`, `.hh`, `.cpp`, `.hpp`, `.cs`,
`.php`, `.swift`, `.scala`, `.sh`, `.bash`, `.zsh`, `.fish`, `.ps1`, `.sql`,
`.html`, `.css`, `.scss`, `.sass`, `.vue`, and `.svelte`.
`--code` runs after normal filesystem or revision discovery, ignore matching,
and safety checks. It does not exclude test files or test directories by name,
so files such as `foo_test.go` and `*.spec.ts` are included when their extension
matches. In filesystem mode, staged, unstaged, and untracked matching files are
included when physically under the search root and not skipped or ignored. In
revision mode, only matching files from the resolved committed tree are
included. Generated Go files with a pre-package heading comment containing
`DO NOT EDIT` are still included by `--code`, but they are indexed as path-only
chunks; their generated body content is not embedded. `--code` shares the same
physical vector cache as default search for the same physical source cache:
default searches can reuse code vectors written by `--code`, and `--code` can
reuse code vectors written by default search. Replay history remains
filter-aware, so default result history is not replayed as a `--code` result
history entry.
`--code` does not introduce a lexical fallback.

`--no-tests` filters common test files from search results and `--ls-files`
output without changing the physical vector cache. It filters path segments named
`test`, `tests`, `__tests__`, or `spec`, Go files ending in `_test.go`, and
basenames matching `*.test.*`, `*.spec.*`, `test.*`, or `spec.*`.

`--index` builds missing embeddings for the selected filesystem or revision
source, including any `--scope` and `--code` candidate filters, writes the same
JSON envelope with an empty result list, and skips query embedding, scoring,
replay history, and semantic search. `--no-tests` does not change the indexed
candidate set. `--index --reindex` rebuilds embeddings for the selected
candidate set even when cache entries already exist. Successful indexing writes
the local cache after all missing embeddings complete. Cache writes replace the
stored chunk and vector lists with the current candidate set, dropping entries
for deleted or newly ignored files. `--code` writes still preserve current
non-code entries in the shared physical cache so default searches can reuse
them. Visible `--scope` writes similarly preserve current out-of-scope entries
in the shared physical cache. `--no-tests` does not alter the indexed candidate
set, so cache writes retain test-file vectors even when `--no-tests` filters
results or `--ls-files` output. Empty candidate sets can be persisted so
`--reindex` can clear a stale index. Parallel searches for the same physical
index source use
one index writer. Other processes wait for the writer, reload the completed
cache, and skip embedding chunks that the writer just stored; parallel
`--reindex` waiters also reuse a cache completed after their command started.

#### `git-agent search --ls [--remote <url>] [--format text|json]`

List completed local search indexes for the current project. With `--remote
<url>`, list completed indexes for that cached remote instead. The command
resolves the metadata root the same way search does and walks its `search/`
directory for valid `manifest.json` files. Incomplete or incompatible index
directories are skipped.

Default `--format text` writes one human-readable entry per index with mode,
optional short revision, root, path-derived filters (`scope-*` only for scopes
that opt into normally skipped paths, plus legacy `code`), file count, chunk
count, embedding model, dimensions, created time, and the absolute index
directory path. `--format json` writes a JSON array of the same fields. The
command does not call embedding providers and does not require API keys.

#### `git-agent search --ls-remotes [--format text|json|completion]`

List cached remote repositories from `~/.git-agent/remotes/`. The command reads
remote metadata only; it does not clone, fetch, embed, query, or require API
keys. Default `--format text` writes one entry per remote with sanitized URL,
optional last resolved revision, last successful fetch time, and cache
directory. `--format json` writes a JSON array of the same fields.
`--format completion` writes one sanitized URL per line for shell completion
helpers.

#### `git-agent search --ls-files [--format tree|json] [--remote <url>] [--rev <rev>] [--scope <paths>] [--no-tests]`

List unique file paths stored in one selected search index. Filesystem indexes
use search-root-relative paths; revision and remote indexes use
repository-relative paths.
Index selection uses the same physical cache keying as search for filesystem or
`--rev`/`--remote` sources. Visible `--scope` values use the shared source cache
and filter listed output to the scoped paths. Scopes that include normally
skipped paths use their separate `scope-*` cache. With `--no-tests`, the command
uses the same index and filters test paths from the listed output. The command
does not clone or fetch remote repositories. When no usable index is present, the
command fails with an error that points at the expected index directory and
suggests `git-agent search --index`.

Default `--format tree` writes a rooted tree of indexed files using box-drawing
characters. `--format json` writes an object with the selected index summary and
a sorted `files` array. The command reads index metadata and path lists only; it
does not load embedding vectors or call providers.

With `--debug`, search writes live human console diagnostic events to stderr
using the same renderer as streamed traces. It writes one `search_skip` event per
file or directory skipped by git-agent's own safety rules, including dot paths,
symlinks, oversized files, binary files, non-text MIME types, unreadable paths,
and non-regular files. Paths skipped only by built-in low-signal ignores,
`.gitignore`, or `.gitagentignore` patterns are not reported. While embedding
missing index chunks, `--debug`
writes live `search_timing`, `search_embed_plan`, and `search_embed_progress`
events. `search_embed_plan` includes the number of embedding batches and the
concurrent request limit chosen for the run. `search_embed_progress` includes
compact completed/total percent progress, elapsed embedding time, average
elapsed time per embedded chunk, and client-side embedding call duration for
that batch.

### Flags

Message-generation subcommands reserve this shared flag surface:

- `--model`
- `--fast`
- `--low`
- `--medium`
- `--high`
- `--xhigh`
- `--base-url`
- `--timeout`
- `--max-steps`
- `--guidance-family`
- `--append-prompt <text>`
- `--debug`
- `--pprof <addr>`

`release-note` additionally supports:

- `--out <file>`: write rendered Markdown to file and stream human console trace
  to stdout instead of writing an on-disk JSON trace session

`search` additionally supports:

- `--scope <paths>`: comma-separated root-relative paths to search or index
- `--limit <n>`: default `20`, valid `1..100`
- `--format`: search accepts `json|brief` and defaults to `json`; `--ls`
  accepts `text|json` and defaults to `text`; `--ls-remotes` accepts
  `text|json|completion` and defaults to `text`; `--ls-files` accepts
  `tree|json` and defaults to `tree`
- `--code`: search source-code files only
- `--no-tests`: exclude common test files and test directories from results and
  `--ls-files` output
- `--agent`: serve current indexing progress on `127.0.0.1:<port>/progress`
  when embeddings need to be built or rebuilt; defaults output to brief unless
  `--format` is set
- `--index`: build embeddings for the selected source without searching
- `--reindex`: rebuild embeddings for the selected source and drop stale cache
  entries
- `--ls`: list search indexes for the current project or `--remote` cache
  without embedding or querying
- `--ls-remotes`: list cached remote repositories without embedding, fetching,
  or querying
- `--ls-files`: list files in the selected search index without embedding or
  querying
- `--remote <url>`: search or inspect a cached remote Git repository URL
- `--rev <rev>`: search a committed Git tree instead of current filesystem files
- `--min-relatedness <score>`: minimum vector relatedness candidate threshold;
  default `0.70`, valid `0 < score <= 1`
- `--embedding-model <model>`: default `text-embedding-3-small`
- `--embedding-dimensions <n>`: default `1024`, valid positive integer
- `--base-url <url>`: override provider base URL
- `--timeout <duration>`: override default request timeout
- `--debug`: enable diagnostics on stderr
- `--pprof <addr>`: serve Go pprof endpoints on the requested address

Flag behavior:

- `--fast`: send `service_tier=priority`
- `--low`: send `reasoning.effort=low`
- `--medium`: send `reasoning.effort=medium`
- `--high`: send `reasoning.effort=high`
- `--xhigh`: send `reasoning.effort=xhigh`
- `--append-prompt <text>`: append a bounded `## Operator hint` section to the
  task user prompt. The hint is escaped inside `<operator_hint>` tags and is
  explicitly lower priority than task instructions, tool policy, project
  guidance, and authoritative repository evidence.
- `--pprof <addr>`: bind the requested address and serve `/debug/pprof/`
  endpoints until the command exits
- default: omit both `service_tier` and `reasoning`

`commit-msg` and `commit` additionally support:

- `--amend`

### Authentication and environment variables

Default auth uses ChatGPT/Codex credentials from `~/.codex/auth.json`.
The file must set `"auth_mode": "chatgpt"` and include
`tokens.access_token` plus `tokens.account_id`. ChatGPT auth defaults the
provider base URL to `https://chatgpt.com/backend-api/codex` and sends
`Authorization: Bearer <access_token>` plus
`ChatGPT-Account-ID: <account_id>`. ChatGPT requests also send
`originator: codex_cli_rs` and `User-Agent: codex_cli_rs`; both client identity
headers are required for current model routing.

`OPENAI_API_KEY` is a legacy fallback for OpenAI-compatible providers when
`~/.codex/auth.json` is absent.
`OPENAI_BASE_URL` applies only to that legacy API-key path; ChatGPT auth uses
`https://chatgpt.com/backend-api/codex` unless `--base-url` is passed
explicitly.
`search` requires an embeddings API key. It reads `OPENAI_EMBEDDING_API_KEY`
first so embeddings credentials can stay separate from message-generation auth,
then falls back to `OPENAI_API_KEY`. Codex/ChatGPT auth is not used for
embeddings. It reads `OPENAI_EMBEDDING_BASE_URL` before `OPENAI_BASE_URL` for
the same isolation. `OPENAI_EMBEDDING_MODEL` changes the default search
embedding model without changing `OPENAI_MODEL`; `OPENAI_EMBEDDING_DIMENSIONS`
changes search embedding dimensions without changing non-search model usage.
`OPENAI_EMBEDDING_MAX_INPUT_CHARS` changes the per-input embedding cap from the
default `32000` characters. `OPENAI_EMBEDDING_BATCH_INPUTS` changes the maximum
inputs per embedding request from the default `32`;
`OPENAI_EMBEDDING_BATCH_MAX_CHARS` changes the maximum total characters per
embedding request from the default `700000`; `OPENAI_EMBEDDING_CONCURRENCY`
changes the concurrent embedding request limit from the default
`min(GOMAXPROCS, 8)`.
The selected account/backend must have embeddings access and quota; otherwise
search fails clearly and does not fall back to lexical retrieval.
Supported environment variables:

- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`
- `OPENAI_MODEL` (overrides the default message-generation model,
  `gpt-5.6-luna`)
- `OPENAI_EMBEDDING_API_KEY`
- `OPENAI_EMBEDDING_BASE_URL`
- `OPENAI_EMBEDDING_MODEL`
- `OPENAI_EMBEDDING_DIMENSIONS`
- `OPENAI_EMBEDDING_MAX_INPUT_CHARS`
- `OPENAI_EMBEDDING_BATCH_INPUTS`
- `OPENAI_EMBEDDING_BATCH_MAX_CHARS`
- `OPENAI_EMBEDDING_CONCURRENCY`

Resolution order:

1. explicit CLI flag
2. `~/.codex/auth.json` ChatGPT auth
3. environment variable fallback, including `OPENAI_API_KEY` auth
4. internal default when defined by that subsystem

For ChatGPT auth, message generation canonicalizes the public `gpt-5.6` alias
to `gpt-5.6-sol` because the ChatGPT Codex endpoint accepts the canonical model
identifier. `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna` pass through
unchanged. API-key providers retain the requested model identifier.

### stdout / stderr contract

- stdout for generation-only commands: final generated artifact only
- stdout for `search`: JSON result by default; brief header and result lines
  with `--format brief`
- stdout for `release-note --out <file>`: streaming human console trace lines
  while generating the release note; the rendered Markdown is written to the
  requested file after a preflight writable check
- stdout for `commit` / `commit --amend`: streaming human console trace lines
  while generating the message, followed by Git's raw commit summary after
  success
- stderr: diagnostics, console-formatted debug output, transient search
  progress, `--agent` progress probe URLs, validation failures, provider/tool
  loop summaries when `--debug` is enabled, and stderr emitted by a successful
  delegated `git commit`
- `search` writes errors and optional `--debug` diagnostics to stderr only and
  never writes a model trace session
- generation-only commands write a JSON trace session under
  `~/.git-agent/<path-sha>/sessions/`
  regardless of `--debug`, except `release-note --out <file>`; `--debug`
  prints the session directory on stderr when a JSON trace session is used
- `release-note --out <file>` does not write an on-disk NDJSON trace session;
  its human console trace lines are streamed to stdout
- `commit` / `commit --amend` do not write an on-disk NDJSON trace session;
  their human console trace lines are streamed to stdout
- `commit` / `commit --amend` delegate commit creation to `git commit`, so Git
  config, hooks, `commit.gpgSign`, system `gpg`, and `gpg-agent` behavior apply
- if commit creation fails after message generation, the command returns nonzero
  after streaming trace events to stdout; the final error includes the
  generated commit message plus the commit failure so the user can commit
  manually

### Exit behavior

Nonzero exit codes are returned for:

- invalid CLI arguments
- missing repository context
- missing required environment configuration
- provider/API failures
- embeddings auth/config/backend failures for `search`
- tool execution failures
- validation failures that cannot be repaired

### Build and install

The repository provides a `Makefile` with:

- `make build`: build `bin/git-agent`
- `make test`: run `go test ./...`
- `make install`: install the built binary to `$(DESTDIR)$(BINDIR)/git-agent`
  and, if the fish config dir exists, install fish completions

Defaults:

- `PREFIX ?= ~/.local`
- `BINDIR ?= $(PREFIX)/bin`
- `XDG_CONFIG_HOME ?= $(HOME)/.config`
- `FISH_CONFIG_DIR ?= $(XDG_CONFIG_HOME)/fish`
- `FISH_COMPLETIONS_DIR ?= $(FISH_CONFIG_DIR)/completions`

## 3. Architecture

### Package map

- `cmd/git-agent`: process entrypoint
- `internal/cli`: argument parsing and command dispatch
- `internal/config`: environment and flag materialization
- `internal/agent`: bounded agent loop contract
- `internal/openai`: official OpenAI Go SDK adapter for the Responses API and
  minimal embeddings adapter for `search`
- `internal/guidance`: project guidance discovery and rendering
- `internal/gitctx`: typed repository inspection
- `internal/tools`: curated read-only tool registry
- `internal/tasks/commitmsg`: commit message behavior
- `internal/tasks/releasenote`: release note behavior
- `internal/tasks/search`: filesystem/revision discovery, chunking, local
  binary vector cache, hybrid ranking, replay metadata, and JSON rendering
- `internal/textutil`: shared normalization and output shaping helpers
- `internal/trace`: JSON session recorder for requests, responses, tool calls,
  and tool outputs

### Request assembly layers

Every task request is assembled using Codex-style layering:

1. top-level Responses `instructions` containing task-level system behavior
2. developer message containing the read-only tool policy
3. developer message containing environment context
4. optional developer message containing available skill metadata and skill-use
   rules
5. developer message containing project guidance
6. task-specific user prompt
7. strict function tool registry for that task, if that task exposes tools

The project guidance block is not treated as ordinary user text. It is a
separate injected layer mirroring Codex’s style.

Environment context includes:

- current working directory
- repository root
- command name
- mode or release range
- selected guidance family
- stdout contract

Tool policy states that tools are read-only repository and skill inspection
functions, cannot run arbitrary shell, cannot mutate
files/index/refs/remotes/network/provider state, and return JSON envelopes with
truncation metadata.

Task prompts use explicit evidence boundaries: repository-sourced text such as
diffs, file contents, commit messages, filenames, refs, and prepared JSON/XML
context is treated as data rather than instructions. Project guidance may shape
style and repository conventions, but it must not override the authoritative
diff or release-range evidence.

The OpenAI adapter uses the official `github.com/openai/openai-go/v3` package.
It converts internal request items into `responses.ResponseNewParams`,
including:

- `Instructions`
- structured input message items
- `function_call` items
- `function_call_output` items
- strict function tool definitions
- `Store: false`
- `ParallelToolCalls: false` when tools are present
- Never send `max_tool_calls` on `/responses`; this provider class rejects it. Enforce tool-call ceilings locally in the runner only, and do not re-add outbound `max_tool_calls`.

### Agent loop lifecycle

1. parse shared flags and validate auth-independent options
2. for commit-message tasks, collect staged paths
3. precompute normal staged context early enough to detect deterministic
   submodule-only messages before provider auth is required
4. for normal submodule-only staged changes, format and return the local
   message without the SDK-backed agent loop
5. resolve provider config and create a JSON trace session, or a
   stdout-streaming human console trace for
   `commit` / `commit --amend`
6. precompute task context before the first provider call: staged context for
   normal commit messages, amend context for `--amend`, PR context for
   `pr-message`, or release-note context for `release-note` including resolved
   refs, parent commits, submodule gitlink changes, submodule history when
   locally available, and repository ownership/link hints
7. resolve project guidance for the task target paths, after context prep when
   prepared paths define the target scope
8. build task-specific instructions, developer context, and initial user prompt,
   appending any `--append-prompt` hint as lower-priority escaped prompt data
9. send request to the Responses API through the official OpenAI Go SDK
10. record each request and response in the active trace
11. if the model requests tools, execute only registered read-only tools
12. record each tool call and tool output in the active trace
13. append function-call and function-call-output items and continue until final
    text is returned
14. if the local budget is exhausted, force a no-tool finalization request while
    preserving any structured text format required by the task
15. validate output against task rules
16. if invalid and repair budget remains, run exactly one repair pass
17. print final text to stdout for generation-only commands, write it to
    `--out` for `release-note --out <file>`, or stream human console trace
    lines while generating the message and then print Git's raw commit summary
    after creating or amending through `git commit`

### Subcommand execution flow graphs

#### `git-agent commit-msg`

```mermaid
flowchart TD
    Start([git-agent commit-msg]) --> Parse[Parse shared flags]
    Parse --> LocalConfig[Validate auth-independent flags]
    LocalConfig --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> StagedPaths[Collect staged paths]
    StagedPaths --> Prepare[Precompute staged commit context]
    Prepare --> SubmoduleOnly{Only submodule gitlinks?}
    SubmoduleOnly -- yes --> LocalMessage[Format local submodule message]
    LocalMessage --> Stdout
    SubmoduleOnly -- no --> Resolve[Resolve provider config from flags, env, defaults]
    Resolve --> Guidance[Resolve project guidance for staged paths]
    Guidance --> Trace[Create home metadata session trace]
    Trace --> Registry[Register read-only commit-message tools plus optional skills_read]
    Registry --> Runner[Build OpenAI runner with validator and tool specs]
    Runner --> Request[Assemble request layers]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Tool calls?}
    ToolDecision -- yes --> ToolBudget{Within tool budget?}
    ToolBudget -- yes --> ExecuteTools[Execute allowed read-only tools]
    ExecuteTools --> RecordTools[Trace tool call and output]
    RecordTools --> Continue[Append function call and output items]
    Continue --> Model
    ToolBudget -- no --> Budget[Extend interactively or force final artifact]
    Budget --> Model
    ToolDecision -- no --> Shape[Shape body wrapping]
    Shape --> Validate[Validate shaped commit message]
    Validate --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass]
    Repair --> Reshape[Shape repaired output]
    Reshape --> Revalidate[Revalidate shaped repaired output]
    Revalidate --> Preserve
    Valid -- yes --> Preserve[Preserve supported task ID suffix]
    Preserve --> FinalValidate[Validate shaped output]
    FinalValidate --> FinalTrace[Trace final artifact]
    FinalTrace --> Stdout([Print artifact only to stdout])
```

#### `git-agent commit-msg --amend`

```mermaid
flowchart TD
    Start([git-agent commit-msg --amend]) --> Parse[Parse --amend and shared flags]
    Parse --> Resolve[Resolve config from flags, env, defaults]
    Resolve --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> StagedPaths[Collect staged paths]
    StagedPaths --> Prepare[Precompute amend context]
    Prepare --> Evidence[Collect original HEAD message, HEAD diff, final amended diff, staged diagnostics]
    Evidence --> Guidance[Resolve project guidance for final amended paths]
    Guidance --> Trace[Create home metadata session trace with amend mode]
    Trace --> Registry[Register read-only commit-message tools plus optional skills_read]
    Registry --> Runner[Build OpenAI runner with amend validator and tool specs]
    Runner --> Request[Assemble amend request layers with prepared amend context]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Tool calls?}
    ToolDecision -- yes --> ToolBudget{Within tool budget?}
    ToolBudget -- yes --> ExecuteTools[Execute allowed read-only tools]
    ExecuteTools --> FinalDiff[Model uses prepared final diff or narrower git_final_amended_diff as authoritative evidence]
    FinalDiff --> RecordTools[Trace tool call and output]
    RecordTools --> Continue[Append function call and output items]
    Continue --> Model
    ToolBudget -- no --> Budget[Extend interactively or force final artifact]
    Budget --> Model
    ToolDecision -- no --> Shape[Shape body wrapping]
    Shape --> Validate[Validate shaped amended commit message]
    Validate --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass]
    Repair --> Reshape[Shape repaired output]
    Reshape --> Revalidate[Revalidate shaped repaired output]
    Revalidate --> Preserve
    Valid -- yes --> Preserve[Preserve supported task ID suffix]
    Preserve --> FinalValidate[Reject delta or process phrasing]
    FinalValidate --> FinalTrace[Trace final artifact]
    FinalTrace --> Stdout([Print artifact only to stdout])
```

#### `git-agent commit` / `git-agent commit --amend`

```mermaid
flowchart TD
    Start([git-agent commit --optional-amend]) --> Parse[Parse --amend and shared flags]
    Parse --> LocalConfig[Validate auth-independent flags]
    LocalConfig --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> StagedPaths[Collect staged paths]
    StagedPaths --> Mode{Amend?}
    Mode -- no --> Prepare[Precompute staged commit context]
    Mode -- yes --> PrepareAmend[Precompute amend context]
    Prepare --> SubmoduleOnly{Only submodule gitlinks?}
    SubmoduleOnly -- yes --> LocalMessage[Format local submodule message]
    LocalMessage --> LocalGitCommit[Run git commit --file]
    LocalGitCommit --> Summary
    LocalGitCommit -- failure --> Manual
    SubmoduleOnly -- no --> Resolve[Resolve provider config from flags, env, defaults]
    PrepareAmend --> Resolve
    Resolve --> Guidance[Resolve project guidance for task paths]
    Guidance --> Trace[Create stdout-streaming console trace]
    Trace --> Registry[Register read-only commit-message tools plus optional skills_read]
    Registry --> Runner[Build OpenAI runner with validator and tool specs]
    Runner --> Request[Assemble request layers]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Tool calls?}
    ToolDecision -- yes --> ExecuteTools[Execute allowed read-only tools]
    ExecuteTools --> RecordTools[Stream trace event and update in-memory snapshot]
    RecordTools --> Continue[Append function call and output items]
    Continue --> Model
    ToolDecision -- no --> Validate[Validate and shape commit message]
    Validate --> FinalTrace[Record final artifact]
    FinalTrace --> Commit{Amend?}
    Commit -- no --> GitCommit[Run git commit --file]
    Commit -- yes --> GitAmend[Run git commit --amend --file]
    GitCommit --> Summary[Print raw Git commit summary]
    GitAmend --> Summary
    Summary --> Done([commit complete])
    GitCommit -- failure --> ErrorTrace[Trace commit error event]
    GitAmend -- failure --> ErrorTrace
    ErrorTrace --> Manual([Return error with generated message])
```

#### `git-agent pr-message`

```mermaid
flowchart TD
    Start([git-agent pr-message]) --> Parse[Parse shared flags]
    Parse --> Resolve[Resolve config from flags, env, defaults]
    Resolve --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> Prepare[Precompute PR context for origin/HEAD..HEAD]
    Prepare --> Evidence[Collect base, changed paths, stats, branch commits, recent commits, bounded diff]
    Evidence --> Guidance[Resolve project guidance for changed paths]
    Guidance --> Trace[Create home metadata session trace]
    Trace --> Registry[Register optional skills_read tool]
    Registry --> Runner[Build OpenAI runner with prepared context and optional skill tool specs]
    Runner --> Request[Assemble request layers with prepared PR context]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Skill tool call?}
    ToolDecision -- yes --> ExecuteSkill[Execute skills_read]
    ExecuteSkill --> RecordSkill[Trace tool call and output]
    RecordSkill --> Continue[Append function call and output items]
    Continue --> Model
    ToolDecision -- no --> Shape[Shape body wrapping]
    Shape --> Validate[Validate shaped squash commit message]
    Validate --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass without tools]
    Repair --> Reshape[Shape repaired output]
    Reshape --> Revalidate[Revalidate shaped repaired output]
    Revalidate --> FinalValidate
    Valid -- yes --> FinalValidate[Validate shaped output]
    FinalValidate --> FinalTrace[Trace final artifact]
    FinalTrace --> Stdout([Print artifact only to stdout])
```

#### `git-agent release-note <base> <release>` or `git-agent release-note patch|minor|major`

```mermaid
flowchart TD
    Start([git-agent release-note args]) --> Parse[Parse shared flags, optional --out, and release range or version bump]
    Parse --> OutCheck{--out set?}
    OutCheck -- yes --> Preflight[Preflight output file writable]
    OutCheck -- no --> Resolve
    Preflight --> Resolve
    Resolve --> Floors[Raise max steps and timeout to release-note minimums]
    Floors --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> Guidance[Resolve project guidance for repository root]
    Guidance --> Trace{--out set?}
    Trace -- no --> JSONTrace[Create home metadata session trace]
    Trace -- yes --> StreamTrace[Create stdout-streaming console trace]
    JSONTrace --> Registry[Register repo_summary plus optional skills_read]
    StreamTrace --> Registry
    Registry --> Infer{Version bump shortcut?}
    Infer -- yes --> Bump[Find latest reachable semver tag and bump patch/minor/major; use HEAD for evidence]
    Infer -- no --> Prepare
    Bump --> Prepare[Precompute release-note context]
    Prepare --> Refs[Resolve base and release revision]
    Refs --> ParentLog[Collect parent repository commits]
    ParentLog --> Submodules[Inspect submodule gitlink changes]
    Submodules --> SubHistory[Collect local submodule history when available]
    SubHistory --> Runner[Build OpenAI runner with release-note validator and JSON format]
    Runner --> Request[Assemble request layers with prepared release context]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Fallback or skill tool call?}
    ToolDecision -- yes --> ToolBudget{Within tool budget?}
    ToolBudget -- yes --> ExecuteTool[Execute repo_summary or skills_read]
    ExecuteTool --> RecordTools[Trace tool call and output]
    RecordTools --> Continue[Append function call and output items]
    Continue --> Model
    ToolBudget -- no --> Budget[Extend interactively or force final artifact]
    Budget --> Model
    ToolDecision -- no --> ValidateJSON[Validate structured release-note JSON]
    ValidateJSON --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass]
    Repair --> Revalidate[Revalidate repaired JSON]
    Revalidate --> BuildDoc[Build Markdown document locally]
    Valid -- yes --> BuildDoc
    BuildDoc --> ValidateDoc[Validate rendered document requirements]
    ValidateDoc --> Render[Render final Markdown]
    Render --> FinalTrace[Trace final artifact]
    FinalTrace --> Output{--out set?}
    Output -- no --> Stdout([Print artifact only to stdout])
    Output -- yes --> File([Write artifact to --out file])
```

### Bounded execution

The runtime must enforce:

- maximum model steps
- maximum tool calls
- maximum bytes/lines per tool result
- per-request timeout
- overall task timeout for message-generation commands

### Session trace format

Generation-only commands store persistent traces under:

```text
~/.git-agent/<path-sha>/sessions/<timestamp>-<command>/
```

`<path-sha>` is the SHA-256 of the cleaned absolute project root. Existing
`<project>/.git-agent/` metadata is migrated into this home directory on the
next project run.

Persistent trace directories contain:

```text
events.ndjson
session.json
artifacts/<sha256>.txt
```

`events.ndjson` is an append-only NDJSON event stream. `session.json` is a
compacted snapshot for humans and test diagnostics. Large string values are
stored under `artifacts/` and replaced by artifact metadata in both files.

`git-agent commit` and `git-agent commit --amend` do not create an on-disk trace
session. They stream readable console trace lines to stdout. Console trace lines
are summarized, so they keep the event type and useful counters without dumping
raw request/response JSON or full diffs. Large or multiline string values in
stdout stream traces are rendered as indented preview blocks with truncation
metadata because there is no artifact directory and raw multiline patches are
not console-friendly.

Each stdout trace line has this shape:

```text
15:04:05 INF session.started command=commit
```

On-disk trace contents include:

- session metadata: command, mode/range, repository summary, staged paths when
  relevant
- every Responses request sent to the provider, with API keys redacted
- every provider response, including raw response JSON when available from the
  SDK
- every model-requested tool call
- every tool output returned to the model
- final generated artifacts and commit errors when relevant

Trace files are diagnostic artifacts and live outside the repository. The
legacy `/.git-agent/` path remains ignored by Git for projects that have not
run migration yet.

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

AGENTS-family candidates:

- `AGENTS.override.md`
- `AGENTS.md`

CLAUDE-family candidates:

- `CLAUDE.md`


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
<PROJECT_DOC path="AGENTS.md">
...
</PROJECT_DOC>

<PROJECT_DOC path="frontend/AGENTS.md">
...
</PROJECT_DOC>
</INSTRUCTIONS>
```

Notes:

- the heading remains `AGENTS.md instructions for ...` for parity with Codex’s
  visible wrapper shape
- the chosen family may still be CLAUDE-family under the hood
- inner path tags preserve provenance and scoped boundaries using
  repository-relative paths to avoid leaking absolute machine paths

### Guidance target path

Guidance must resolve against the task target path, not blindly against process
cwd.

Task defaults:

- `commit-msg`: staged paths when present in normal mode; final amended paths
  for `--amend`; if no task paths are available, current repository root
- `pr-message`: changed paths between `origin/HEAD` and `HEAD`; if no changed
  paths are available, current repository root
- `release-note`: current repository root

For `commit-msg`, guidance is resolved across all task paths. Normal mode uses
staged paths; amend mode uses the final amended paths so guidance can cover the
latest HEAD commit being amended as well as staged refinements. Family selection
remains global for the task: if any task path has AGENTS-family guidance,
AGENTS-family is selected and CLAUDE-family files are ignored for the whole
request. Sources are de-duplicated while preserving root-to-leaf order.
`pr-message` uses the same family-selection behavior, but its target paths come
from the current-branch diff against `origin/HEAD`.

## 4.1 Skill discovery

Message-generation commands discover reusable Codex-style skills from local
`SKILL.md` files before request assembly. A valid skill has YAML frontmatter
with nonempty `name` and `description`, parsed with `github.com/goccy/go-yaml`.
Invalid or incomplete skill files are skipped.

Discovery roots:

- `.agents/skills` in each directory from the current working directory up to
  the repository root
- `$HOME/.agents/skills`
- `$CODEX_HOME/skills`, defaulting to `$HOME/.codex/skills`
- `/etc/codex/skills`
- plugin cache skills under `$CODEX_HOME/plugins/cache/**/skills/*/SKILL.md`

The injected `## Skills` developer message lists only metadata and source
locators. The model must call `skills_read` before applying a listed skill.
Skill instructions are lower priority than task instructions, tool policy,
output contracts, validators, project guidance priority, and authoritative
Git/release evidence.

## 5. Tool system

### Principles

- read-only only
- typed tool contracts
- no arbitrary shell access
- no generic “run any git command” escape hatch
- bounded output with explicit truncation markers

### Shared repository tools

Shared tools:

- `repo_summary`
- `list_files`
- `read_file`
- `search_files`

### Skill tools

Message-generation commands discover Codex-style skills before the provider
request and inject a developer `## Skills` section containing skill names,
descriptions, and source locators. Skill bodies are not inlined. When at least
one valid skill is discovered, the model receives:

- `skills_read`

`skills_read` reads `SKILL.md` or a text file under the selected skill's
`references/` directory. It requires the exact source locator from the initial
Skills section, rejects unknown locators, absolute paths, traversal, symlink
escapes, non-`references/` resource paths, and binary content, and applies the
standard byte/line caps. It cannot execute skill scripts or expose arbitrary
host files.

### Commit message tools

Commit message tools:

- `git_staged_paths`
- `git_staged_status`
- `git_staged_stat`
- `git_staged_diff`
- `git_staged_diff_for_paths`
- `git_recent_commits`
- `git_head_show`
- `git_diff_against_parent`
- `git_final_amended_diff`
- `git_amend_delta`
- `git_show_file_at_rev`

`commit-msg` and `commit` expose these tools plus `skills_read` when skills are
available. `pr-message` exposes no PR/repository diff tools; when skills are
available, it exposes only `skills_read`. It precomputes `origin/HEAD` base
metadata, changed paths, diff stats, branch commits, recent style commits, and
a bounded full diff in Go before the first provider call.

### Release note tools

Release-note generation precomputes ref resolution, parent logs, submodule
gitlink changes, submodule history, and repository ownership in Go before the
first provider call. The model receives only the `repo_summary` fallback tool
for rare metadata gaps, plus `skills_read` when skills are available; legacy
range/submodule tools are intentionally not exposed to the model. `resolve_ref`,
`git_log_range`, `gitmodules_table`, `submodule_gitlink_range`,
`submodule_log_range`, and `repo_kind` remain in the registry only as deprecated
legacy tools.

### Tool I/O expectations

Each tool definition must provide:

- stable tool name
- description
- strict JSON schema for arguments using `additionalProperties: false`
- required fields for mandatory arguments
- bounds for numeric cap arguments
- JSON result envelope with stable fields
- explicit truncation metadata when output is capped

Tool result envelope:

```json
{
  "ok": true,
  "tool": "git_staged_diff",
  "data": {},
  "truncated": false
}
```

The tool loop records both the model's function-call arguments and the exact
tool-output envelope sent back to the model.

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
- precompute staged context before generation, with changed paths, status,
  stats, recent style commits, previous HEAD paths/stats/diff for contrast,
  and a bounded staged diff
- when the bounded staged diff is truncated, precompute an additional focus
  diff for high-churn paths that were omitted or cut off, unless the change is
  handled by generated-heavy compaction/outlier rules
- compact generated-heavy staged changes with a context pack only when raw
  outlier diffs for small handwritten change clusters remain visible in the
  initial request
- use recent commit history as style reference only
- use previous HEAD paths/stats/diff only as contrast to understand what was
  already done, not as current staged scope; for large previous diffs, paths
  and stats preserve contrast shape even when the previous diff text is capped
- allow the model to request extra related file reads when the diff is
  ambiguous
- allow the model to request path-filtered staged diffs for omitted or
  high-churn clusters when the bounded full staged diff is large or truncated
- avoid tool calls that merely repeat prepared context; use narrow read-only
  tools only when they reduce material uncertainty
- cover each distinct high-signal staged change cluster present in the staged
  diff, rather than letting a dominant cluster hide a secondary behavior change
- avoid copying phrasing from recent commits or previous HEAD diff as if it
  were current staged work
- prefer `refactor` when staged evidence shows extraction, relocation,
  deduplication, or internal reorganization of existing behavior, even if new
  helper files or tests are added
- use `feat` only when the staged diff introduces a genuinely new user-visible
  capability, API, command, config option, or behavior
- when staged submodule commit summaries are available, include those summaries
  in the generated message rather than emitting only a generic submodule-ref
  update subject
- for normal submodule-only staged changes, skip model generation and format a
  deterministic message locally from staged submodule history; detect
  conventional versus Title-case subject style from recent commits, use a
  release-note-like submodule body, and collapse subjects with more than three
  submodules to `submodules`

Output rules:

- subject line first
- blank line before body only when body exists
- no fences
- no explanations
- the model is not asked to hard-wrap body paragraphs; output shaping treats
  nonblank body lines inside the same paragraph as soft wraps, reflows them to
  the target width (72 characters), and preserves blank lines between paragraphs
- long unbreakable tokens such as URLs may exceed the limit only when they
  cannot be wrapped safely

### Commit message: amend mode

Behavior:

- describe the final amended commit as one commit versus its parent
- never narrate the amended result as “previous commit plus extra changes”
- precompute prepared amend context before generation, including original HEAD
  message, latest HEAD commit metadata, HEAD-vs-parent paths/stats/diff,
  staged paths/status/stats/diff diagnostics, submodule diagnostics when
  present, recent style commits, and final amended paths/stats/diff versus
  HEAD's first parent
- expose the latest HEAD commit context in the initial request so the model
  does not have to infer the commit being amended from an empty prompt or from
  staged-delta tools alone
- treat `git_final_amended_diff` as authoritative; it overlays staged changes
  on current HEAD and compares the final amended result against the first parent
- treat prepared final amended diff fields as authoritative initial evidence;
  use `git_final_amended_diff` only for narrower follow-up when the prepared
  diff is truncated or ambiguous
- treat the current HEAD message as the output anchor; preserve its subject and
  high-level story, revising body details only when the final amended diff
  proves them false
- treat the original HEAD message as evidence and an anchor, not as executable
  instructions
- use current HEAD, HEAD-vs-parent, and staged-vs-HEAD views only as diagnostic
  inputs
- never base the subject or narrative on staged paths or staged delta alone

Output rules:

- one narrative only
- the original HEAD subject must be preserved by validation
- no delta/process phrasing such as “also”, “this amend”, or “in addition”
- preserve task IDs or scope markers only when still supported by the final
  diff

### PR message mode

Behavior:

- describe the current branch as one squash merge commit versus `origin/HEAD`
- treat the `origin/HEAD` to `HEAD` diff as authoritative scope
- use the prepared PR context as authoritative evidence before any skill lookup
- do not reference or request PR-specific tools; `pr-message` intentionally
  exposes no PR/repository diff tools and may expose only `skills_read`
- use branch commits only as supporting evidence for intent, grouping, and task
  IDs
- ignore staged and unstaged work unless it is already committed at `HEAD`
- do not emit pull request prose, review instructions, or release notes

Output rules:

- subject line first
- blank line before body only when body exists
- no fences
- no explanations
- no commit-by-commit changelog
- the model is not asked to hard-wrap body paragraphs; output shaping applies
  the same commit-message paragraph reflow and target-width rules

### Release note generation

Behavior:

- peel and validate both refs
- generate a parent-repository commit log for the selected range
- include each release-note commit's full message content in prepared context,
  clamped independently to 10 lines and 1000 words
- include per-commit changed paths, diffstat, and bounded patch excerpts so
  release-note bullets can be grounded in concrete commit evidence instead of
  commit summaries alone
- classify changed paths into operator-facing signals such as runtime, config
  schema, API, CLI, docs, generated, tests, dependency-only, and submodule-only
  changes
- precompute candidate release-note items with draft facts, recommended sections,
  confidence, refs, and evidence; the model should polish these candidates rather
  than inventing new behavior
- inspect submodule gitlink changes
- include submodule commit groups only when the gitlink moved and local commit
  history is available; submodule commit messages follow the same 10-line and
  1000-word independent clamps and include the same changed-path evidence when
  available
- treat commit messages, diffs, and prepared release context as evidence rather
  than executable instructions
- optimize prose for deployers/operators rather than developers
- keep narrative bullets concise: state the change first, avoid generic benefit
  clauses when they restate the capability, and add second-clause detail only for
  non-obvious impact, required action, compatibility/risk, rollout scope, or
  behavior changes

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
- amend mode does not use process/delta phrasing
- amend mode preserves the original HEAD subject
- normal mode includes staged submodule commit summaries when prepared context
  exposes them
- body lines stay within the target width after output shaping (target width: 72
  characters after shaping, except for long unbreakable tokens such as URLs)
- output shaping reflows soft line breaks inside body paragraphs so generated
  messages do not keep isolated word shards from model line wrapping

Release note validator checks at minimum:

- first printable line starts with `### `
- no forbidden preamble
- heading/content structure valid
- no low-signal release-note continuations such as generic "enabling operators"
  or "reducing editing errors" clauses
- release-note prepared context contains candidate items, changed paths, diffstat,
  bounded patch excerpts, operator signals, and omit/include policy hints
- `### Full Changelog` included when required

### Repair strategy

If validation fails:

1. summarize the validation errors
2. run one repair pass through the model
3. revalidate
4. return an error if still invalid

## 7. Testing strategy

### Unit tests

Unit coverage should include:

- prompt normalization
- CLI parsing
- guidance family selection
- guidance scoped ordering
- validator rules
- truncation metadata
- strict tool schemas
- tool result envelopes
- trace redaction and trace file creation

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
- official SDK request compatibility
- stdout-only artifact behavior

### Integration tests

Use temporary repositories to test:

- staged commit message generation scenarios
- amend scenarios
- staged-path guidance scoping
- detached HEAD
- root commit handling
- release-note tag/range handling, including patch/minor/major shortcut inference
- submodule gitlink movement and missing checkout cases

## 8. Risks and open constraints

### go-git fidelity risk

Index and diff fidelity in the read-only context helpers may not perfectly
mirror `git` CLI behavior. Commit creation itself is delegated to `git commit`,
so this risk is limited to generated context for amend and submodule-heavy
scenarios.

Mitigation:

- write integration tests around real temp repositories
- validate behavior, not raw textual parity

### Provider drift risk

“OpenAI-compatible” providers may diverge in tool-call or Responses API
details.

Mitigation:

- keep the SDK adapter thin
- isolate provider translation and SDK type conversion in `internal/openai`
- test against a fake server and at least one real provider
- keep full JSON session traces for request/response debugging

### Release-note formatting regressions

Release-note output has strict deployer-facing formatting constraints.

Mitigation:

- carry those constraints into validators
- lock output with golden tests

### Token growth risk

Generic file reads can inflate context quickly.

Mitigation:

- typed tools first
- strict tool output caps
- encourage narrow follow-up reads

### Trace data sensitivity risk

Session traces intentionally store prompts, provider responses, tool arguments,
and tool outputs. They are useful for debugging but may include repository
content. For `git-agent commit` and `git-agent commit --amend`, stdout streams
summarized console trace events instead of writing an on-disk session under
`~/.git-agent/<path-sha>/sessions/`. Console request events omit raw request
fields such as `instructions`, but generated text and repository-derived
summaries or previews may still appear in stdout; large string values are
compacted inline with preview metadata.

Mitigation:

- redact API keys from request traces
- store generation-only traces under `~/.git-agent/<path-sha>/`
- keep legacy `.git-agent/` ignored in Git until migration removes it
- print trace directory only when `--debug` is enabled
- document that commit-command stdout may contain repository context and should
  be handled like trace data

## 9. Current acceptance criteria

The in-repository implementation is complete when:

- `make build` succeeds and writes `bin/git-agent`
- `make test` / `go test ./...` pass
- `make install DESTDIR=<tmp> PREFIX=/usr/local` installs an executable binary
- `git-agent commit-msg` and `git-agent commit-msg --amend` route through the
  bounded SDK-backed agent loop except for normal submodule-only staged changes,
  which are formatted locally without provider auth
- `git-agent commit` and `git-agent commit --amend` route through the same
  bounded SDK-backed commit-message loop except for normal submodule-only
  staged changes, stream human console trace lines to stdout for SDK-backed
  generation, write no on-disk NDJSON session trace, create or amend the commit
  through `git commit`, and print Git's raw commit summary after success
- `git-agent pr-message` routes through the bounded SDK-backed agent loop,
  targets `origin/HEAD..HEAD`, and sends prepared branch context without
  exposing model tools
- `git-agent release-note [--out <file>] <base> <release>` resolves explicit refs
  before generation; `git-agent release-note [--out <file>] patch|minor|major`
  resolves the latest reachable semantic version tag and uses `HEAD` as the
  release revision
- guidance rendering uses repository-relative `<PROJECT_DOC path="...">` tags
- normal commit-message guidance resolves against staged paths, while amend
  guidance resolves against the final amended paths
- tools are read-only and exposed as strict function tools
- tool outputs use the stable JSON envelope
- generation-only commands write a
  `~/.git-agent/<path-sha>/sessions/<timestamp>-<command>/`
  trace, except `release-note --out <file>`
- generation-only stdout contains only the final generated artifact, except
  `release-note --out <file>` streams human console trace lines and writes the
  artifact to the requested file
