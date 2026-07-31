# Git Agent current-increment brief

## Global working-directory flag

### Outcome

An operator can run any Git Agent command against another directory without
changing the invoking shell's working directory first.

### First-draft scope

- `git-agent --cwd <directory> <command> [args...]` for every command.
- Relative directories resolved from the caller's original working directory.
- Absolute directories accepted unchanged.
- The selected directory applied before command dispatch and all
  working-directory-sensitive behavior.
- Existing command output preserved when directory selection succeeds.

### Non-goals

- A command-local `--cwd` spelling after the subcommand.
- A persistent configured working directory or environment-variable alias.
- Changing the invoking shell's working directory after Git Agent exits.

### Constraints and assumptions

- `--cwd` is a global flag and therefore precedes the subcommand.
- Directory selection fails before dispatch when the value is missing or the
  selected path cannot become the process working directory.
- Detached processes inherit the selected directory through the existing
  process-launch behavior.

## Project identity and explore disposition logging

### Outcome

An operator can retrieve search's stable project identifier and inspect a
private, greppable record of whether each exploration was batched, unbatched,
or created from a follow-up branch.

### First-draft scope

- `git-agent project_id` prints the existing search project hash.
- Explore writes one redacted disposition record per sealed item beneath the
  standard XDG state directory and that project hash.
- Batch mode and branch ancestry remain independently visible.
- Concurrent writers preserve complete records with owner-only permissions.

### Non-goals

- Logging question text, answers, provider transcripts, or tool output.
- Making explore success depend on operational-log availability.
- Adding log-path or project-identity configuration aliases.

### Constraints and assumptions

- Origin-backed clones share the identity already used by search; projects
  without an origin remain absolute-path-specific.
- The log follows `search_code` disposition timing and non-fatal behavior.
