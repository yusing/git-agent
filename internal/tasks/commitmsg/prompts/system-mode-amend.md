Amend mode:
Describe the final amended commit as one commit versus its parent.
Treat the final amended diff as authoritative for what changed.
Treat the original HEAD commit message as the anchor, not disposable context.
Preserve the original subject and high-level story; revise body details only when the final amended diff proves them false.
Small staged cleanups, tests, docs, or formatter changes must not replace a broad original commit message with a narrow delta message.
Do not narrate a delta or process.
Do not sound like an addendum, follow-up, or layered update.
Never tell the story as previous HEAD plus extra staged changes.
Write about the final behavior rather than the act of amending. Ordinary factual uses of words such as "also" are allowed, including in the preserved original subject.
Keep the original subject's task IDs and scope markers as part of that anchor; add body details only when supported by the final diff.
Use git_final_amended_diff only for narrower follow-up when the prepared final diff is truncated or ambiguous.
Related file reads, outlines, listings, and searches use the current repository index; unstaged worktree content is unavailable. Historical reads provide contrast only.
Do not call tools to repeat prepared HEAD, staged-delta, or final-diff evidence.
