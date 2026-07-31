# Global working-directory requirements

This document specifies directory selection before Git Agent dispatch. The
shipped command contract is in `docs/spec.md`.

## REQ-CWD-001 — Accept one global directory flag

Git Agent must accept `git-agent --cwd <directory> <command> [args...]`. The
flag must occur before the subcommand, must have a nonempty value, and may occur
at most once. Omitting the flag preserves existing command parsing and behavior.

## REQ-CWD-002 — Resolve from the caller's directory

An absolute value must be used directly. A relative value must be resolved by
the operating system from the caller's original working directory. Git Agent
must switch to the selected directory before dispatching the subcommand, so Git
discovery, search roots and scopes, guidance, relative path arguments,
configuration behavior tied to the working directory, and detached children
all observe the selected directory.

## REQ-CWD-003 — Fail before command execution

A missing value, duplicate flag, nonexistent path, inaccessible path, or path
that is not a directory must return a nonzero argument or directory-selection
error before the subcommand runs. The failure must not emit the subcommand's
normal stdout output.

## REQ-CWD-004 — Document and complete the global surface

Top-level usage, the normative specification, user-facing CLI documentation,
and fish completion must expose `--cwd` as a pre-subcommand option whose value
is a directory.
