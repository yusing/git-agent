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

## Explore query targets

### Outcome

An agent or operator can select the kind of grounded answer needed from
`explore` without changing search behavior or using `--fast` as a semantic
shortcut.

### First-draft scope

- `explore --for diagnose|change|behavior|owner <question...>`.
- The current universal guidance remains the default.
- Context-preserving follow-ups inherit the active target.
- A changed follow-up target is appended once as replayable developer input.
- A depth reset starts fresh with the selected target.

### Non-goals

- Free-form, configured, or automatically inferred query targets.
- A separate review target or changes to the dedicated `review` command.
- Runtime access to Codex session history or `~/.codex`.
- Any model, reasoning, budget, cache, or search semantic attached to `--fast`.

### Query-target constraints

- Responses `instructions` remain stable within a cache branch.
- A changed-target developer message contains
  `Query target changed: <target>` followed by that target's instructions.
- The four values come from the accepted local-history analysis; the universal
  prompt covers mixed or unclear questions.
- Provider batching requires the same service tier, parent identity, and selected
  query target.

## Index storage garbage collection

### Outcome

An operator can reclaim derived local search storage and, when configured,
unreferenced current-tree packs in the shared index repository through one
explicit, observable operation without deleting valid index snapshots.

### First-draft scope

- `git-agent index gc [--dry-run]` always inventories every eligible local
  project and cached-remote search store under the standard metadata root.
- Local garbage collection compacts shared vector payloads from exact references
  in every valid completed local index and removes only recognized incomplete or
  superseded legacy payloads.
- When `index.remote` is configured, the same command also removes vector packs
  referenced by no valid manifest in the authoritative current shared tree.
- An unset `index.remote` skips the shared phase without preventing local
  cleanup.
- `--dry-run` validates and reports the same candidates without publishing,
  deleting, committing, or pushing.
- The remaining current-HEAD index-sync pack catalog uses a compact, versioned
  binary representation while retaining safe reconstruction from immutable
  packs.

### Non-goals

- Automatic, scheduled, age-based, recency-based, count-based, or size-based
  index retention.
- Deleting any valid local or shared revision manifest.
- Rewriting shared Git history, pruning historical Git objects, or running
  general Git garbage collection.
- A `--remote` selector, retention configuration, daemon, or alternate cleanup
  command.
- Embedding-provider calls or mutation of source repositories, cached bare
  repository contents, settings, authentication, or background-task state.

### Garbage-collection constraints

- Local cleanup always runs across the standard metadata root; `index.remote`
  is optional and controls only the shared phase.
- Every deletion derives from exact current references after strict validation
  under the owning locks.
- Each local vector store publishes a generation-bound payload before its
  catalog so interruption leaves a readable generation.
- Local stores publish before the optional shared cleanup. If a later phase
  fails, completed local compactions remain valid and a repeated command resumes
  from actual state.
- Shared cleanup preserves all valid manifests, validates the prospective tree,
  creates at most one unsigned commit, and pushes last.
- Long-running discovery, compaction, validation, fetch, and push report
  progress without changing the single-line stdout summary.
- Production Git access remains in-process and does not use `exec.Command*`.
