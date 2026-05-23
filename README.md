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
- `--base-url`
- `--timeout`
- `--max-steps`
- `--guidance-family`
- `--debug`
