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
- `git-agent search [--index] [--rev <rev>] [--min-relatedness <score>] [--limit <n>] <query...>`

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
- `OPENAI_EMBEDDING_MAX_INPUT_CHARS`
- `OPENAI_EMBEDDING_BATCH_INPUTS`
- `OPENAI_EMBEDDING_BATCH_MAX_CHARS`
- `OPENAI_EMBEDDING_CONCURRENCY`

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
- `--pprof <addr>`

`search` is embeddings-only semantic retrieval for agents. It searches the
current filesystem by default, or a committed tree with `--rev <rev>`, and
writes JSON results to stdout. Use `--code` to limit candidates to source-code
files. Use `--index` without a query to build or refresh missing embeddings
without searching; add `--reindex` to rebuild existing embeddings too. Search
uses `OPENAI_EMBEDDING_API_KEY` when set, then falls back to `OPENAI_API_KEY`;
Codex/ChatGPT auth is not used for embeddings. Successful indexing writes the
local cache after embedding completes. See `docs/spec.md` for exact file
discovery, ignore-file, skip, cache, and debug behavior.

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

Trace files include session metadata, provider requests/responses, tool calls,
and returned tool output. API keys are redacted. `--debug` prints the trace
directory to stderr.

`--pprof <addr>` serves Go pprof endpoints on the requested address, for example
`:7777`.

`release-note --out <file>` writes the rendered Markdown to the requested file
and skips the on-disk JSON trace session. `release-note patch|minor|major` finds
the latest reachable semantic version tag, bumps the requested component, and
uses `HEAD` as the release revision.

`commit` and `commit --amend` generate the same message as `commit-msg`, stream
the human console trace to stdout, then run `git commit --file -` or
`git commit --amend --file -`. Normal Git config, hooks, signing, and
`gpg-agent` behavior apply. If commit creation fails after message generation,
the command exits nonzero and includes both the generated message and Git error.
In amend mode, the current HEAD message is treated as the message anchor so
small staged refinements preserve the original subject.
