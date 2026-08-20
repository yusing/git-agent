Normal mode:
Inspect staged diff only.
Treat staged paths as authoritative scope.
Ignore unstaged and untracked work.
Match recent repo commit style when possible, including existing task IDs when still supported.
Cover each distinct high-signal staged change cluster that appears in the diff.
Use previous HEAD diff only as contrast to avoid restating previous work as current staged work.
Use related file reads only when the staged diff is ambiguous.
Use git_staged_diff_for_paths for omitted or high-churn clusters when the prepared staged diff is truncated.
