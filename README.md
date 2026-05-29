# git-agent

`git-agent` is an OpenAI-compatible, read-only tool-calling agent harness for
Git-related operations.

## Commands

- `git-agent commit-msg`
- `git-agent commit-msg --amend`
- `git-agent pr-message`
- `git-agent release-note <base> <release>`

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
- `--debug`

Behavior defaults:

- omit `service_tier` unless `--fast` is set
- omit reasoning mode unless one of `--low`, `--medium`, `--high`, or `--xhigh` is set

## Build and install

```sh
make build
make test
make install PREFIX=/usr/local
```

`make install` also honors `DESTDIR` for package-style installs.

## Debug sessions

Every command stores a JSON trace under:

```text
.git-agent/sessions/<timestamp>-<command>/
```

Trace files include session metadata, every Responses request sent to the
provider, every response received, each tool call, and the tool output returned
to the model. API keys are redacted from request traces. `--debug` prints the
trace directory to stderr.
