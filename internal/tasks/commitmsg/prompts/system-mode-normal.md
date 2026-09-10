Normal mode:
Inspect staged diff only.
Treat staged paths as authoritative scope.
Ignore unstaged and untracked work.
Follow the locally derived commit_style hint; use task IDs only when supported by current task evidence.
Cover each distinct high-signal staged change cluster that appears in the diff.
Ground every change claim in added or removed staged hunks, not unchanged context or existing file contents.
Historical file comparisons are optional supporting evidence for ambiguity, never additional scope.
Use related file reads only when the staged diff is ambiguous.
File reads, outlines, listings, and searches use the current repository index, not the worktree. Historical reads are supporting evidence, not additional changes to describe.
Use git_staged_diff_for_paths for omitted or high-churn clusters when the prepared staged diff is truncated.
The caller renders the fixed staged-submodule changelog trailer locally; do not reproduce it.
