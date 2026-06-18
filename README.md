# git-agent

`git-agent` is an OpenAI-compatible tool-calling agent harness for Git-related
operations. Model tools are read-only; the explicit `commit` command can run the
final Git commit after message generation.

## Commands

- `git-agent commit-msg`
- `git-agent commit-msg --amend`
- `git-agent commit`
- `git-agent commit --amend`
- `git-agent pr-message`
- `git-agent release-note [--out <file>] <base> <release>`
- `git-agent release-note [--out <file>] patch|minor|major`
- `git-agent search [--rev <rev>] [--min-relatedness <score>] [--limit <n>] <query...>`

Mermaid execution-flow graphs for each subcommand are documented in
[`docs/spec.md`](docs/spec.md#subcommand-execution-flow-graphs).

## Configuration

By default, `git-agent` uses ChatGPT/Codex auth from:

```text
~/.codex/auth.json
```

The auth file must include:

```json
{
  "auth_mode": "chatgpt",
  "tokens": {
    "access_token": "...",
    "account_id": "..."
  }
}
```

ChatGPT auth sends requests to `https://chatgpt.com/backend-api/codex` with
`Authorization: Bearer <access_token>` and `ChatGPT-Account-ID: <account_id>`.

`OPENAI_API_KEY` is a legacy fallback when `~/.codex/auth.json` is absent.
`OPENAI_BASE_URL` only applies to that legacy API-key path; ChatGPT auth uses
the ChatGPT backend unless `--base-url` is passed explicitly.
Supported environment variables:

- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`
- `OPENAI_MODEL`
- `OPENAI_EMBEDDING_API_KEY`
- `OPENAI_EMBEDDING_BASE_URL`
- `OPENAI_EMBEDDING_MODEL`
- `OPENAI_EMBEDDING_DIMENSIONS`

CLI flags override environment values.

Common flags:

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
- `--append-prompt`
- `--debug`

`search` is embeddings-only semantic retrieval for agents. It does not run the
Responses API, does not call tools, does not generate text, and has no lexical
fallback or keyword boost. By default it searches current files under the
current working directory exactly as they exist on disk, including staged,
unstaged, and untracked files. Filesystem mode skips dot files/directories,
honors `.gitignore` patterns, and skips binary files, oversized files, and
symlinks. It does not require a Git repository.

Go files with a pre-package heading comment containing `DO NOT EDIT` are indexed
as path-only chunks. Their filename can match semantically, but generated source
content is not embedded.

`git-agent search --rev <rev> <query...>` switches to a committed Git tree. In
that mode the command must run inside a Git repository, resolves `<rev>` to a
commit, searches that commit tree only, and ignores current filesystem
contents.

Use `git-agent search --code <query...>` for implementation searches. It keeps
the CLI simple by only filtering candidates to source-code files before
embedding/ranking; it does not add lexical matching or score boosts. Default
search still includes docs and code.

Search requires an embeddings API key. Set `OPENAI_EMBEDDING_API_KEY` to keep
embeddings credentials separate from normal message-generation auth; if it is
unset, search falls back to `OPENAI_API_KEY`. Codex/ChatGPT auth is not used for
embeddings. Use `OPENAI_EMBEDDING_BASE_URL` for an embeddings-only provider base
URL; otherwise search falls back to `OPENAI_BASE_URL` and then
`https://api.openai.com/v1`. The selected account/backend must have embeddings
access and quota; otherwise search fails clearly instead of falling back to
lexical retrieval. `--base-url`, `--timeout`, and `--debug` are supported.
The default embedding model is `text-embedding-3-small` with `1024` dimensions;
pass `--embedding-model text-embedding-3-large` when recall quality is worth the
extra cost, storage, and latency. `--embedding-dimensions <n>` or
`OPENAI_EMBEDDING_DIMENSIONS` changes the search embedding dimensions only,
without affecting non-search model usage. Lower dimensions reduce index size
and latency; higher dimensions can improve recall. `OPENAI_EMBEDDING_MODEL`
changes the search default without affecting `OPENAI_MODEL`. Results are
written as JSON only on stdout.
`relatedness` is always in `(0, 1]`, and results below
`--min-relatedness` are omitted. Defaults are `--min-relatedness 0.70`,
`--limit 20`, and maximum `--limit 100`.

Behavior defaults:

- omit `service_tier` unless `--fast` is set
- omit reasoning mode unless one of `--low`, `--medium`, `--high`, or `--xhigh` is set
- `--append-prompt` adds a bounded operator hint to the task prompt; it can
  steer style or emphasis only when consistent with the task contract and
  repository evidence

## Build and install

```sh
make build
make test
make install PREFIX=/usr/local
```

`make install` also honors `DESTDIR` for package-style installs.

If `$(FISH_CONFIG_DIR)` exists, `make install` also installs fish completions to
`$(FISH_COMPLETIONS_DIR)/git-agent.fish`. Defaults:

- `XDG_CONFIG_HOME ?= $(HOME)/.config`
- `FISH_CONFIG_DIR ?= $(XDG_CONFIG_HOME)/fish`
- `FISH_COMPLETIONS_DIR ?= $(FISH_CONFIG_DIR)/completions`

## Debug sessions

Message-generation commands store a JSON trace under:

```text
.git-agent/sessions/<timestamp>-<command>/
```

Trace files include session metadata, every Responses request sent to the
provider, every response received, each tool call, and the tool output returned
to the model. API keys are redacted from request traces. `--debug` prints the
trace directory to stderr.

`git-agent release-note --out <file> <base> <release>` checks the output target
is writable before generation, streams the human console trace to stdout, writes
the rendered Markdown to the requested file, and does not create an on-disk JSON
trace session. Release-note context is precomputed before the model runs and
includes commit messages, changed paths, diffstat, bounded patch excerpts,
operator-facing change signals, omit/include policy hints, and candidate release
note items so the model can ground bullets in concrete commit evidence. The
command also accepts a single `patch`, `minor`, or `major` argument: it finds the
latest reachable semantic version tag (`vX.Y.Z` or `X.Y.Z`), strips any `v`
prefix, bumps the requested component, and uses `HEAD` as the release revision
for evidence. For example, both `v1.0.0` + `patch` and `1.0.0` + `patch` infer
release version `1.0.1`.

`git-agent commit` and `git-agent commit --amend` generate the same message as
`commit-msg`. Stdout streams a human console trace while the message
is generated, then prints Git's raw commit summary after `git commit` succeeds.
Trace lines use short local times like `15:04:05 INF final`, color field keys
when stdout is a terminal, and render long or multiline values as indented
preview blocks so raw patches do not flood the console. No on-disk trace session
is written. Commit creation is delegated to
`git commit --file -` (or
`git commit --amend --file -`), so normal Git config, hooks, `commit.gpgSign`,
system `gpg`, and `gpg-agent` behavior apply. If commit creation fails after
message generation, including because signing fails or a key is locked, the
command exits nonzero and stdout still contains the streamed trace lines,
including the final event for the generated message. The final error includes
the generated message and Git error so the user can commit manually.
In amend mode, the current HEAD commit message is treated as the message anchor:
small staged cleanups or refinements must preserve the original subject instead
of replacing the commit with a narrow delta description. The amend request is
seeded with prepared context for the latest commit being amended, including its
HEAD-vs-parent diff, the final amended diff versus the parent, staged
diagnostics, and recent style commits before the model can ask for narrower
read-only follow-up tools.
