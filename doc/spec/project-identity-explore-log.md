# Project identity and explore disposition-log requirements

This document specifies the public project-ID command and private exploration
disposition log. The shipped command and record contract is normative in
`docs/spec.md`.

## REQ-PROJECT-ID-001 — Reuse search project identity

Git Agent must derive one lowercase 64-character identifier through the same
normalized-origin or cleaned-absolute-path hash used by search metadata.
Origin-equivalent clones must share the identifier; projects without an origin
must remain path-specific.

## REQ-PROJECT-ID-002 — Print only the project identifier

`git-agent project_id` must accept no arguments and write only the identifier
and one trailing newline to stdout. It must not create an index or contact a
provider. Global `--cwd` selection must apply before identity resolution.

## REQ-EXPLORE-LOG-001 — Use private XDG project state

Explore dispositions must be stored at
`${XDG_STATE_HOME:-$HOME/.local/state}/git-agent/<project_id>/explore.log`.
Application and project directories must use mode `0700`; the log and its lock
must use mode `0600`.

## REQ-EXPLORE-LOG-002 — Record batch and branch disposition independently

After a batch is sealed, its leader must append one record per item containing
the exact fields defined by `docs/spec.md`. Batch mode must distinguish batched
from unbatched execution, while branch state, parent ID, and depth independently
describe follow-up ancestry. A depth-limit reset must appear as a fresh item.

## REQ-EXPLORE-LOG-003 — Serialize and redact records

Concurrent processes writing the same project log must not interleave, lose, or
overwrite records. Every record must redact the question rather than writing
question, answer, transcript, or tool content.

## REQ-EXPLORE-LOG-004 — Keep disposition logging non-fatal

Disposition logging must happen before provider execution and must not change
explore success or failure. A sealed item may therefore remain recorded when
provider execution or later result handling fails, while an unavailable log
must not fail an otherwise successful exploration.
