# Historical attribution fixture

`historical-attribution.json` is the deterministic prepared context captured
with HEAD at `99303cb7ced0c78f2bc829f74a399bf8ec8e05ba` and the index at
`df27022339a01ac8e8c7cb39dc2e47a5aa42136c`. It preserves the old request's
current diff, focus diff, inventory, recent summaries, and previous-HEAD patch.
It is not the complete original provider request.

The staged change covers 19 paths and no submodule updates. It fixes indexed
evidence, repository-state checks, focused-diff retention, amend/URL validation,
and automatic task-ID inheritance. Local recursive submodule changelog
formatting was already implemented in the parent.

The regression tests verify request composition, not model output quality.
A live evaluation should keep the observed `gpt-5.6-luna` model and compare old
and new requests. Accept messages describing the staged fixes without claiming
local or recursive submodule changelog formatting is newly introduced. Also
check secondary staged changes, genuine submodule updates, and amend behavior.
