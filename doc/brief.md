# Global working-directory flag brief

## Outcome

An operator can run any Git Agent command against another directory without
changing the invoking shell's working directory first.

## First-draft scope

- `git-agent --cwd <directory> <command> [args...]` for every command.
- Relative directories resolved from the caller's original working directory.
- Absolute directories accepted unchanged.
- The selected directory applied before command dispatch and all
  working-directory-sensitive behavior.
- Existing command output preserved when directory selection succeeds.

## Non-goals

- A command-local `--cwd` spelling after the subcommand.
- A persistent configured working directory or environment-variable alias.
- Changing the invoking shell's working directory after Git Agent exits.

## Constraints and assumptions

- `--cwd` is a global flag and therefore precedes the subcommand.
- Directory selection fails before dispatch when the value is missing or the
  selected path cannot become the process working directory.
- Detached processes inherit the selected directory through the existing
  process-launch behavior.
