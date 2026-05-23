# git-agent

`git-agent` is an OpenAI-compatible, read-only tool-calling agent harness for
Git-related operations.

## Commands

- `git-agent commit-msg`
- `git-agent commit-msg --amend`
- `git-agent release-note <base> <release>`

## Configuration

v1 uses standard OpenAI-compatible environment variables:

- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`
- `OPENAI_MODEL`

CLI flags will override environment values.

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
