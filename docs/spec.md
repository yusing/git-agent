# git-agent specification

## 1. Purpose and non-goals

### Purpose

`git-agent` is a standalone Go binary for Git-related generation workflows.
It:

- gathers Git and repository context without shelling out to ad hoc scripts
- uses the official OpenAI Go SDK against an OpenAI-compatible Responses API
  endpoint
- runs a bounded, read-only, tool-calling agent loop
- emits final generation artifacts, exploration envelopes, or strict review
  JSON on stdout
- can optionally create the Git commit after generating a message
- preserves project guidance behavior close to Codex for AGENTS-family files

Supported workflows:

- `git-agent commit-msg`
- `git-agent commit-msg --amend`
- `git-agent commit`
- `git-agent commit --amend`
- `git-agent pr-message`
- `git-agent release-note [--out <file>] <base> <release>`
- `git-agent release-note [--out <file>] patch|minor|major`
- `git-agent review [--codebase|--uncommitted|--staged] [flags] [prompt...]`
- `git-agent review --wait <id>`
- `git-agent review [--fast] --follow-up <turn-id> <prompt...>`
- `git-agent explore [--debug] [--fast] [--for <diagnose|change|behavior|owner>] [--follow-up <search-id>] <question...>`
- `git-agent project_id`
- `git-agent simplify [--codebase|--uncommitted|--staged] [flags] [prompt...]`
- `git-agent simplify --wait <id>`
- `git-agent simplify [--fast] --follow-up <turn-id> <prompt...>`
- `git-agent search [flags] <query...>`
- `git-agent search --ls [--remote <url>] [--format text|json]`
- `git-agent search --ls-remotes [--format text|json|completion]`
- `git-agent search --ls-files [--format tree|json] [--remote <url>] [--rev <rev>] [--scope <paths>] [--no-tests]`
- `git-agent config index.remote [<git-url>]`
- `git-agent config --unset index.remote`
- `git-agent index sync`
- `git-agent index gc [--dry-run]`

### Non-goals

`git-agent` must not:

- execute arbitrary shell commands on behalf of the model
- merge AGENTS-family and CLAUDE-family guidance into the same prompt
- implement provider-specific plugins beyond OpenAI-compatible Responses API
  options exposed through the official SDK
- add write-capable repository tools
- preserve exact raw `git` CLI output byte-for-byte when a typed Go equivalent
  is clearer and stable

## 2. User-facing commands

### Commands

#### `git-agent commit-msg`

Generate a commit message from the staged diff in the current repository.
Stdout contains only the final message.
The command precomputes staged paths, status, stats, recent style commits, and
the bounded staged diff before generation so the authoritative staged scope is
visible before any optional follow-up tool calls. For generated-heavy staged
changes, the request may compact dominant generated hunks into a context pack,
but it must still include raw outlier diffs for small handwritten change
clusters. Large or capped staged diffs expose a path-filtered staged-diff tool
so the model can inspect omitted high-churn or secondary clusters without
reading unrelated hunks.

When the staged changes are exclusively submodule gitlink updates, normal
`commit-msg` does not call the model or require provider auth. It formats a
deterministic message from prepared submodule history, using recent commits to
choose conventional style (`chore(deps): update ... submodule`) or Title-case
style (`Update ... submodule`). The body mirrors the release-note submodule
changelog shape with each submodule heading followed by indented
`short-sha: summary` entries. If more than three submodules are staged, the
subject says `submodules` instead of listing every path.

#### `git-agent commit-msg --amend`

Generate a commit message for the final post-amend commit result, not a delta
note about the newly staged changes. The current HEAD commit message is the
anchor for subject, scope, task IDs, and high-level intent; staged cleanups or
refinements must not replace a broad original message with a narrow delta
message. The command precomputes amend context before generation: original HEAD
message, latest HEAD commit metadata, HEAD-vs-parent paths/stats/diff, staged
diagnostics, recent style commits, and the bounded final amended diff versus
HEAD's first parent. This gives the model enough latest-commit context before
any optional follow-up tool calls.

#### `git-agent commit`

Generate a commit message from staged changes using the same prompt,
validation, shaping, guidance, and read-only model tools as `commit-msg`, then
create the commit by running `git commit --file -` in the repository root. On
success, stdout streams a human console trace while generating the message,
then prints Git's raw commit summary after `git commit`
succeeds. Trace lines use short local times such as `15:04:05 INF final`, color
field keys when stdout is a terminal, and render long or multiline values as
indented preview blocks. Because commit creation is delegated to Git, normal Git
config,
hooks, `commit.gpgSign`, system `gpg`, and `gpg-agent` behavior apply. If commit
creation fails after message generation, including because signing fails or a key
is locked, the command returns nonzero, keeps the streamed trace events on
stdout, and reports both the generated message and the Git error so the user can
commit manually.

For normal submodule-only staged changes, `commit` uses the same deterministic
local formatter as `commit-msg`, skips provider auth and trace generation, then
passes the formatted message directly to `git commit --file -`.

#### `git-agent commit --amend`

Generate the final amended commit message using the same semantics as
`commit-msg --amend`, then amend the commit by running
`git commit --amend --file -` in the repository root. The
success stdout contract matches `git-agent commit`: human console trace lines
followed by Git's raw commit summary.
Amend mode preserves the original HEAD author and uses the current configured
committer. The original HEAD subject is validated as the amend message anchor so
model output cannot silently replace it with a staged-delta-only subject. The
message-generation request is seeded with the same prepared amend context as
`commit-msg --amend`.

#### `git-agent pr-message`

Generate a squash merge commit message for the current branch versus
`origin/HEAD`. The command treats the diff from `origin/HEAD` to `HEAD` as the
authoritative scope, precomputes branch evidence before generation, and uses
branch commits as supporting evidence.

#### `git-agent release-note [--out <file>] <base> <release>` or `git-agent release-note [--out <file>] patch|minor|major`

Generate a GitHub release body for the range from `<base>` to `<release>`.
As a shortcut, `patch`, `minor`, or `major` finds the latest reachable semantic
version tag, accepts either `vX.Y.Z` or `X.Y.Z`, strips any `v` prefix, bumps the
requested component, and uses `HEAD` as the release revision for evidence. For
example, `v1.0.0` plus `patch` and `1.0.0` plus `patch` both infer release
version `1.0.1`.
The command precomputes release-note evidence in Go before generation and then
asks the model to write from that prepared context, with only a minimal
read-only fallback tool available for rare gaps.
By default the rendered Markdown is printed to stdout. With `--out <file>`, the
command checks the target is writable before generation, streams the human
console trace to stdout, and writes the rendered Markdown to the file.

#### `git-agent review [--codebase|--uncommitted|--staged] [flags] [prompt...]`

Run an evidence-backed, read-only code review and print one strict JSON report.
Mode flags are mutually exclusive. No mode flag means `--uncommitted`.

- `--uncommitted` reviews final dirty worktree state against `HEAD`, including
  staged, unstaged, and untracked changes. A path changed in both index and
  worktree appears once as final worktree content against `HEAD`. It recursively
  expands initialized, registered submodules and their initialized descendants.
  Nested changed-file inventory and evidence paths are relative to invocation
  root; patch paths inside a labeled nested-repository diff section are relative
  to section repository prefix. Each descendant compares superproject-recorded
  base gitlink with current descendant worktree, so both committed gitlink ranges
  and dirty files are reviewed. If recorded base object is unavailable locally,
  gitlink evidence remains authoritative and locally dirty files are compared
  with descendant checkout `HEAD`. Clean, uninitialized, unregistered,
  malformed-path, and symlink-escaping repositories do not gain nested scope.
  Untracked `.git-agent/` and `.omx/` runtime state is excluded; tracked files
  under those names remain ordinary review scope. Filesystem status follows
  Git's ignore precedence across the configured or default global excludes
  file, `$GIT_COMMON_DIR/info/exclude`, and per-directory `.gitignore` files
  before descending into untracked directories. A descendant allowlist rule
  takes effect only after every excluded parent directory has been re-included.
  Ignored untracked subtrees are not inspected, while tracked files below
  ignored directories remain ordinary review scope. Access failures outside
  ignored subtrees fail preparation with an actionable error.
- `--staged` reviews index state against `HEAD` and ignores unstaged content.
- `--codebase` audits full repository without preloaded diff scope.
- `--depth fast|balanced|thorough` selects the lower bound, midpoint, or upper
  bound of the automatic inspection budget and the command-specific default
  reasoning effort. Omission means `balanced`. Review defaults are
  `fast=low`, `balanced=medium`, and `thorough=high`; simplify defaults are
  `fast=low`, `balanced=low`, and `thorough=medium`.
- `--max-steps <positive-n>` is an exact expert override and is mutually
  exclusive with `--depth`.
- `--orchestration-artifact <absolute-path>` validates an owner-only manifest
  and its declared immutable files beneath manifest directory. It enables no
  arbitrary filesystem access.
- `--dry-run` preserves repository preparation, detached launch, authenticated
  SSE, optional orchestration validation, and repeatable wait output while
  replacing provider execution with deterministic schema-valid reasoning, tool,
  and final events. Fifteen emitted events each wait an independent random
  500–1000 ms, keeping run observable for roughly 8–16 seconds.
- `--help-agent` returns help for automated coding agents: the launch synopsis,
  the three scope modes, `--depth`, and the mutually exclusive reasoning-effort
  flags `--low`, `--medium`, `--high`, and `--xhigh` rendered on one line. Its
  depth guidance tells agents to use `thorough` only for security-related issues
  or very complex logic, and to use `fast` or `balanced` otherwise. It omits
  operator, provider, retrieval, diagnostic, budget-override, dry-run, and
  orchestration flags. Like `--help`, it exits without launching a detached
  task.

Diff modes prepare paths, staged/worktree status, line stats, generated-heavy
context pack, bounded unified diff, and a best-effort previous-`HEAD` context
pack before the first provider request. The previous-`HEAD` pack summarizes
`HEAD` versus its first parent for contrast only; it does not expand the
authoritative review scope. The initial prompt contains bounded views of both
packs' groups, outliers, and artifacts plus the bounded current diff; it does
not duplicate the complete raw path, status, or stat lists. Truncation is
explicit. Full current scope remains authoritative for report validation and
read-only repository tools. Moved submodule gitlinks include bounded commit
summaries when referenced history is available in local checkout; unavailable
history leaves ordinary gitlink diff unchanged. In uncommitted mode, prepared
and tool-read diffs also include recursively expanded dirty submodule file
content under labeled repository prefixes. Diff preparation
also records a launch fingerprint from complete base and authoritative target
trees plus dirty-submodule state. Every diff-mode repository tool call and final
report validation recomputes that fingerprint; any worktree, index, `HEAD`, or
dirty-submodule drift fails with an explicit rerun error. Codebase mode remains
live and has no fingerprint guard. Empty diff scope fails before provider
resolution. Codebase mode provides no packed diff; model discovers
implementation, contracts, callers, and tests through read-only tools.
Positional text remaining after flag parsing is escaped and appended as a
lower-priority operator hint, using same precedence rules as `--append-prompt`.
Without a hint that identifies a narrower inspection focus, review reports every
actionable finding and simplify inspects the full authoritative scope. When a
hint identifies a focus, the model may inspect supporting repository context but
reports only findings or opportunities relevant to that focus. A focus may
narrow what is reported within the authoritative scope; it cannot broaden that
scope or weaken repository-evidence requirements.

In staged mode, repository guidance is read from index blobs, and `list_files`,
`read_file`, `inspect_file`, `jq`, `grep`, and `find` use index state.
Explicit worktree-source requests for `read_file`,
`inspect_file`, and `jq` are rejected. In all modes, `read_file` streams the
selected source and applies byte/line caps before materializing content. Report
validation verifies every evidence path and inclusive line end against the
authoritative worktree/index source, with HEAD fallback for deleted diff
evidence and one-line synthetic evidence for changed gitlinks. For a nonempty
text source ending in a newline, the immediately following blank EOF line is
also valid; later lines remain out of range.

Review examines correctness, security, reliability, performance,
maintainability, tests, and style. Style findings are preserved alongside other
findings and must use `LOW`. Findings are ordered from highest to lowest
severity. Recommendation is `FIX` when any `CRITICAL` or `HIGH`
finding exists, `COMMENT` for only `MEDIUM`/`LOW` findings, and `APPROVE` when
findings are empty.

Provider text format uses strict JSON Schema. Output object requires `summary`,
`recommendation`, and `findings`. Each finding requires `severity`, `aspect`,
`title`, `impact`, `evidences`, and `proposed_fix`. `severity` is one of
`CRITICAL`, `HIGH`, `MEDIUM`, or `LOW`; `aspect` is one of `correctness`,
`security`, `reliability`, `performance`, `maintainability`, `tests`, or
`style`. `evidences` contains at least one object with nonempty `title`,
repository-relative `path`, and positive inclusive `line_start`/`line_end`.
Validator rejects unknown fields, missing evidence, invalid paths/ranges,
severity-order violations, invalid style severity, and recommendation mismatch.
When orchestration input is present, Git-agent adds its validated manifest
SHA-256 to stored final report after model-schema validation.

After the validated provider review report (and after branch reports are merged,
when applicable), review selects the registered host checks that apply to the
authoritative review scope and runs them once. Inapplicable checks are omitted
from the report rather than represented as skipped. It publishes
`runtime.status` with `phase=running_static_checks` and the check name before
each runnable check. The terminal review report preserves the provider fields
and adds an ordered `checks` array, which is empty when no checks apply. Each
check result has status
`pass`, `findings`, `skipped`, or `error`; findings contain bounded normalized
diagnostics, skipped results contain a reason, and check-analysis failures
contain an error. Private checker start, wait, analysis, and output failures
produce an `error` check result; context cancellation remains a terminal task
error.

The built-in `golangci-lint` check applies only when it selects at least one Go
target from the authoritative review scope; otherwise it is absent from
`checks`. Uncommitted review uses the verified worktree; staged review uses the
verified materialized index snapshot; codebase review runs `./...` once for each
discovered Go module. Codebase module discovery applies ignore rules separately
within the top-level repository and each initialized submodule. Each repository
component uses its effective global exclude file, `$GIT_COMMON_DIR/info/exclude`,
nested `.gitignore` files, and private `.git-agent/` and `.omx/` exclusions
before traversal enters an untracked directory. Tracked paths remain in scope.
Access failures in unignored directories remain planning errors. In changed
modes, existing regular `.go`
paths select the nearest Go module without crossing a repository-component
boundary, then select their exact package
directories. Duplicate paths and mixed production/test paths in one directory
produce one package invocation. Non-Go, deleted, nonexistent, symlinked,
escaping, and module-less paths do not become linter arguments. Renames
therefore select the existing destination and skip a missing source.

Each selected changed package is passed to golangci-lint as an exact local
package pattern, not as individual `.go` files, so parsing and type checking see
all production and `_test.go` siblings in that package. Package selection does
not recurse into unrelated packages. The helper requests absolute diagnostic
paths; result normalization rejects paths outside the selected module or checker
workspace, resolves no symlink aliases, and retains only `.go` diagnostics in
the authoritative changed-path scope. Thus unchanged siblings provide analysis
context but cannot add report diagnostics. Unknown fields in golangci's JSON
issue and report objects remain ignored for forward compatibility; malformed,
missing, oversized, or internally inconsistent helper output yields an `error`
check result.

#### `git-agent simplify [--codebase|--uncommitted|--staged] [flags] [prompt...]`

Run a read-only simplification audit using same mode selection, prepared diff,
guidance, skill, tool, validation-repair, SSE, and trailing-prompt contracts as
`review`. It reports opportunities; it never edits files. Output object requires
`summary` and `opportunities`. Each opportunity requires `aspect`, `title`,
`body`, `evidences`, and `proposed_change`; `aspect` is one of `reuse`,
`clarity`, or `efficiency`. Evidence objects use same required location schema
as review findings. Only confirmed behavior-preserving opportunities belong in
output; empty opportunities is valid. Simplification explicitly audits for
overengineering, including unnecessary abstractions and wrappers, premature
generalization or extensibility, needless indirection or configuration,
redundant state or concurrency, and architecture disproportionate to current
requirements. Taste-only rewrites and speculative future simplifications are
excluded.

During an ordinary initial or follow-up provider step, either command may
expose strict `branch_help` and `branch` functions when the remaining
inspection can be split into at least two independently reviewable
responsibilities. `branch_help` has no arguments, its description is
``Use before deciding to use `branch` ``, and its ordinary local-tool result
contains the bounded model catalog, difficulty-to-reasoning-effort mapping, and
values allowed for the current command. It consumes one local function-call
budget unit and does not retire the conversation. `branch` remains the terminal
control function.

Both functions are absent from dry-run generation, forced finalization, schema
repair, aggregation, and conversations already at the selected depth limit.
`fast`, `balanced`, and `thorough` permit respectively `2`, `3`, and `4`
immediate children and maximum zero-based conversation depths `1`, `1`, and
`2`. A provider response may contain at most one `branch` call and may include
ordinary local calls beside it. Git-agent executes those ordinary calls
concurrently, waits for all of them, and appends their outputs in provider order
before accepting the branch. In diff modes, Git-agent revalidates the
authoritative review snapshot after the concurrent calls join and before it
emits their outputs or accepts the branch. Accepted children then start
immediately under the same detached task context; there is no separate branch
task, queue, or global concurrency setting. Each child receives a fresh copy of
the invocation's per-conversation step and local-tool ceilings.

An accepted branch call retires its calling conversation. A cancellation,
deadline, or authoritative review-snapshot drift from an ordinary call in the
same response fails the node before fan-out; recoverable ordinary-call failures
remain structured outputs in the completed parent continuation. Child scope is
a natural-language reporting responsibility; path hints accelerate discovery
but do not restrict repository inspection, evidence, or final validation. Child
input is the forked provider-visible conversation, including every ordinary
function-call output from the branch response, followed by the selected branch
function result; Git-agent appends no child-specific developer message.
The detached review tree assigns every request one cache key derived from its
task ID. For GPT-5.6-family models, Git-agent sends that key, marks the initial
conversation's last reusable input-text block with one explicit prompt-cache
breakpoint, uses explicit-only cache mode, and retains the original marker in
forked histories rather than moving it onto branch-specific output. These
controls are best-effort: a changed model, tool catalog, structured-output
schema, or dynamic instruction prefix can make a child ineligible for a hit.
Git-agent does not alter branch availability or depth semantics to preserve
cache identity. Models outside the GPT-5.6 family receive neither the explicit
breakpoint nor its request options and continue using provider-default caching,
but official OpenAI requests still carry the stable cache key. The authenticated
ChatGPT Codex endpoint also receives only that key because it does not accept
the explicit options or content-block breakpoint. Custom endpoints receive no
prompt-cache fields because Git-agent has no provider capability declaration
for them.
Child model and reasoning effort inherit by default or select from the bounded
model catalog returned by `branch_help` and enforced by the strict `branch`
function. Every required leaf must pass the ordinary report and
repository-evidence validators. A delegated scope that cannot be fully inspected
returns a validator-valid leaf which describes the concrete coverage limitation
only in its summary and contains no findings or opportunities. Git-agent treats
that leaf as completed, retains its summary and successful sibling items, then
concatenates leaf items in recursive child-array order, applies the existing
stable review severity ordering and recommendation rule, concatenates
scope-labeled summaries, and validates the assembled report without a reducer
model. Review static checks run once after that merge. A provider, transport,
parse, validation, cancellation, or deadline failure is instead a required-child
failure: it cancels remaining siblings and fails the one detached task without
publishing a partial report.

Both commands always bind an HTTP server to a private local Unix-domain socket
after local validation. The launcher publishes its socket network, absolute
address, and token-bearing `http://localhost/events` request URL in launch JSON
before the first provider request. Requests without that per-run token are rejected. SSE uses
`id`, `event`, and JSON `data` fields, buffers events for late clients, and
honors `Last-Event-ID`. Stream includes `session.started`, `session`, `request`,
`reasoning_summary.delta`, `reasoning_summary.done`, `response`, `tool-call`,
`tool-output`, `hosted-tool-call`, `hosted-capability`, `runtime.status`,
`budget`, and terminal `final` or `error` events as applicable. Hosted-search
events contain only bounded query, status, action, and source metadata, never
fetched page bodies. `runtime.status` reports phase, model step, tool-call
usage, elapsed runtime, latest provider input-token usage, estimated request
tokens, and context-token budget. Before a provider retry it uses phase
`retrying_provider` and additionally reports one-based `retry_attempt`,
`max_retry_attempts`, `abandoned_provider_attempt`, the next
`provider_attempt`, and bounded `retry_reason` without including the raw
provider error. That status event marks all reasoning-summary progress from the
abandoned attempt as superseded.
Reasoning delta values contain `item_id`, `output_index`, `summary_index`,
one-based `provider_attempt`, provider `sequence_number`, and `delta`; done
values contain the same identity fields and complete `text`. Terminal event
closes streams, then server shuts down. A truncated provider stream or HTTP/2
`INTERNAL_ERROR` or
`REFUSED_STREAM` received from the peer receives one semantically equivalent
streaming retry of that model step with a fresh accumulator. A stream that ends
without `response.completed` is truncated even when it contained partial text
or tool calls; partial first-attempt response text and tool calls are discarded,
while already-published reasoning progress remains identifiable by provider
attempt and is superseded by the retry status. Cancellation or
deadline prevents or aborts the retry. Other, local, unrelated, and unknown
provider stream failures remain terminal. If the retry fails, the terminal
error preserves both attempt failures.

The `session` event identifies `event_schema=git-agent.events/v2` and
`root_node_id=root`. Accepted fan-out adds globally sequenced
`branch.fanout`; non-root model activity appears only inside `branch.event`;
validated leaves and failed nodes add `branch.completed` and `branch.failed`.
Fan-out precedes child activity, lifecycle events do not terminate the stream,
and only the existing task-level `final` or `error` closes it. Node identity,
parent identity, depth, effective model/effort, and bounded untrusted display
text are replayed through the same SSE buffer and `Last-Event-ID` cursor.
Aggregate `runtime.status` remains unwrapped and uses `branches_running` and
`aggregating_branches`, so consumers that ignore branch event kinds still
receive useful task progress and the one final report.

Neither command has a request or overall task deadline by default. Explicit
`--timeout <duration>` applies that deadline to both the provider HTTP client
and the complete agent loop.

Model precedence is `--model`, then `OPENAI_MODEL`, then the command default.
Both commands request `reasoning.summary=auto` so summaries can stream as live
agent progress. `review` defaults to `gpt-5.6-sol`; `simplify` defaults to
`gpt-5.6-terra`. Review reasoning defaults by depth are `fast=low`,
`balanced=medium`, and `thorough=high`; simplify defaults are `fast=low`,
`balanced=low`, and `thorough=medium`. An explicit reasoning flag overrides
the depth-derived default.

Diff-based review and simplify calculate deterministic lower and upper model-step
bounds after preparing the authoritative snapshot and building the concrete
tool registry. Let `Lh` and `Lg` be handwritten and
standards-marker-generated added-plus-deleted lines, `B` binary files, `Fh` and
`Fg` handwritten and generated files, and `D` distinct top-level path scopes:

```text
Le = Lh + ceil(0.15 * Lg) + 50 * B
Fe = Fh + ceil(Fg / 4) + B
W  = 2 * ceil(sqrt(Le / 50))
     + ceil(sqrt(Fe))
     + ceil(log2(1 + max(0, D - 1)))
```

Only the standard Go generated-file marker classifies generated content;
deletions and additions otherwise have equal weight. Root-level paths form one
`.` scope. Binary files contribute a fixed line-equivalent because they have no
meaningful line stat.

Tool coverage `C` is a value in `[0,1]` based on concrete registered
capabilities, not raw tool count: bounded source read `0.30`, authoritative
change enumeration `0.20`, path-bounded diff `0.20`, search or structural
inspection `0.20`, and path discovery `0.10`. Codebase mode omits path-bounded
diff and renormalizes the other `0.80`. Missing bounded source reading, or
missing authoritative scope enumeration/discovery, fails budget planning rather
than granting more steps.

```text
Mlow  = 1 + 0.25 * (1 - C)
Mhigh = 1 + 0.75 * (1 - C)

review lower = 6 + ceil(0.5 * W * Mlow)
review upper = 6 + ceil(W * Mhigh) + 3

simplify lower = 5 + ceil(0.5 * W * Mlow)
simplify upper = 5 + ceil(W * Mhigh) + 2
```

Review clamps both bounds to `[8,60]`; simplify clamps them to `[6,45]`.
`fast` selects the lower bound, `balanced` selects
`ceil((lower+upper)/2)`, and `thorough` selects the upper bound. The automatic
local function-tool ceiling is `ceil(0.8*selected_steps)`, clamped to `[6,48]`
for review and `[5,36]` for simplify. An explicit `--max-steps` selects exactly
that positive model-step ceiling, may exceed the automatic hard cap, and retains
the command's fixed 48- or 36-call local tool ceiling for compatibility.

Codebase mode has no changed-line input and retains fixed 60/48 review and 45/36
simplify budgets for every automatic depth; `--max-steps` is the way to request
a smaller or larger codebase audit. The `session` event includes an
`inspection_budget` object containing policy, effective size, scope and work
units, capability coverage, lower/selected/upper steps,
local tool ceiling, hard caps, automatic/explicit state, and whether the fixed
codebase budget applied.

Every provider request states the selected step and remaining tool-call budget.
These local safety ceilings are never extended interactively for either command.
At a ceiling, the runner records a JSON `budget` SSE event and makes a tool-free
forced-finalization request using evidence already collected. On success, the
detached worker persists and publishes the terminal report; it writes no report
to stdout.

Every normal review and simplification model step enables provider-hosted
`web_search`. It uses existing provider authentication and requests both
`web_search_call.action.sources` and `reasoning.encrypted_content`, while keeping
`store:false`. API-key authentication defaults hosted `max_tool_calls` to `4`;
ChatGPT/Codex-plan authentication omits that cap. Explicit
`--max-web-searches <positive-n>` overrides either default. Hosted calls do not
consume local function-tool budget. Forced finalization removes hosted and local
tools.

Response continuation replays complete reasoning, web-search-call, assistant
message, and function-call output items in original provider order before local
function-call outputs. On a recognized rejection of `web_search`, its source or
encrypted-reasoning include, or hosted `max_tool_calls`, runner emits sanitized
capability failure, disables hosted search for remaining run, injects summary
disclosure requirement, and repeats rejected step once. Authentication,
authorization, rate-limit, transport, malformed-response, and unrelated
provider errors remain terminal. Because the ChatGPT/Codex-plan endpoint returns
an empty HTTP 400 for unsupported hosted `max_tool_calls`, that exact response is
recognized only when the rejected plan-auth request carried a positive hosted
call cap; an empty response without that request shape remains terminal.

Every `review` or `simplify` invocation without `--wait` starts a detached
process. The launcher waits until the event server is listening, then writes
exactly one JSON object and newline to stdout with string `command`, string
`id`, positive integer `pid`, and strict `endpoint` object containing string
`network`, `address`, and `url` fields. Successful launch writes nothing to
stderr. The detached worker closes inherited standard streams and runs through
the terminal SSE event.

`review --wait <id>` and `simplify --wait <id>` accept no mode, prompt, timeout,
model, generation, debug, or pprof option. A wait has no deadline, polls the
globally unique task ID across project metadata stores, verifies the producer
PID while running, and respects
process-context cancellation. A matching `final` event writes only its
`value.text` as strict report JSON to stdout. Retrieval remains repeatable after
completion. A stored `error`, unknown or malformed ID, corrupt record, dead
producer, or task created by the other command returns nonzero with empty
stdout.

`review [--fast] --follow-up <turn-id> <prompt...>` and
`simplify [--fast] --follow-up <turn-id> <prompt...>` start a new detached turn
that targets a successful real-provider report from the same command and
current project. The prompt is required; after flag parsing, its argv elements
are joined with one ASCII space. `--` permits a prompt whose first element
starts with `-`. `--fast` sends `service_tier=priority` for the new provider
conversation. `--follow-up` is isolated from `--wait`, scope modes, ordinary
trailing focus, `--append-prompt`, orchestration artifacts, and every other
provider or execution override.

The new turn inherits only the parent's uncommitted, staged, or codebase mode
and otherwise uses current configuration, guidance, skills, read-only tools,
validation, and repository state. It starts a fresh provider conversation whose
first user message is one strict JSON object containing only
`previous_findings` plus `prompt` for review, or `previous_opportunities` plus
`prompt` for simplify. The prior summary, recommendation, checks, prepared
context, reasoning, and tool transcript are not reconstructed.

Every follow-up re-evaluates the named provider report's items against current
authoritative repository state. Resolved or inapplicable items are omitted;
review may add a regression directly caused by the attempted fix.
Uncommitted and staged modes retain their current-turn fingerprint guard, staged
mode still excludes unstaged bytes, codebase mode remains live, and an empty
current diff is valid. Review reruns current host checks after the provider
report.

Each accepted follow-up allocates a new task ID, launch object, replayable
authenticated SSE endpoint, durable report, and repeatable `--wait` result.
The `session` event records the parent but never the prompt or prior report.

`--dry-run` is valid only on initial review/simplify launch and is mutually
exclusive with `--wait` and `--follow-up` through normal flag conflict
validation.

Global review and simplification lifecycle settings are read once per detached
worker from `~/.git-agent/settings.json`. The v1 schema is a strict JSON object
with optional `hooks`; `hooks` is a strict object with optional string-array
`post_inspection`:

```json
{"hooks":{"post_inspection":[""]}}
```

Unknown fields, malformed JSON, multiple JSON values, and non-string hook
entries fail the task. A missing file, omitted fields, an empty array, and
blank array entries configure no corresponding work. This file is distinct
from the XDG index configuration because it owns user-level inspection
lifecycle behavior rather than `git-agent config` command state.

After a non-dry-run inspection has produced and validated its report, and after
review static checks have completed, each nonblank `post_inspection` entry runs
sequentially through `sh -c`. Before execution, Git-agent parses it as a Go
`text/template` with `missingkey=error`. Template data is the payload described
below. Function `format_markdown <payload>` renders session metadata, aggregate
and per-branch usage, findings or opportunities, evidence, proposed changes,
and checks as Markdown while escaping dynamic Markdown syntax. Functions
`json <value>` and `shellquote <value>` encode JSON and quote one POSIX-shell
argument respectively. The same compact JSON payload is passed
to every hook on stdin. Hook stdout is discarded. A template error, inability
to start `sh`, context cancellation, or nonzero exit stops the sequence and
publishes non-terminal `runtime.status` with
`phase=post_inspection_hook_failed` and a bounded error message; up to 4096
bytes of trimmed hook stderr may be included. Hook failures never replace,
modify, or prevent publication of the already validated final report, so
`--wait` continues to return that report as strict JSON without printing the
hook diagnostic. Earlier successful hooks are not rolled back. After the shell
exits or its context is canceled, inherited
stdin or stderr pipes are forcibly closed after one second so a background
descendant cannot indefinitely block task completion. Dry runs never execute
hooks.

The stdin and template-data object has `schema_version: 2`, a `session` object,
a `metrics` object, and the exact final `report`. `session` contains task `id`, a
derived `title` of `<command> <repository-directory> (<mode>)`, `command`,
`mode`, `model`, `reasoning_effort`, UTC `started_at` and `completed_at`,
`elapsed_ms`, `tool_calls`, `repair_calls`, and the repository summary already
used by the session event. `report` therefore contains review `findings` or
simplification `opportunities`, including their evidence.

`metrics.usage` sums provider-reported usage across every completed response in the
root conversation, branch conversations, schema repair, and forced
finalization. It contains `input_tokens`, `cached_input_tokens`,
`cache_write_input_tokens`, derived nonnegative `uncached_input_tokens`,
`output_tokens`, `reasoning_tokens`, and `total_tokens`. Providers that omit a
counter contribute zero for that counter.
`metrics.used_skills` lists each distinct skill successfully read through
`skills_read`, in deterministic conversation traversal order. Reading a
skill-relative reference records the leading skill name. `metrics.tool_calls` lists local model
tools that completed or returned a recoverable error envelope, sorted by tool
name; each entry contains `name` and `count`. Control calls such as branch
fanout are included. Provider-hosted tools are not local model tool calls and
are not included.
`metrics.branches_created` is the number of child conversations created by
branch fanout. `metrics.branches` lists those conversations in creation order;
each entry contains `id`, `parent_id`, resolved `model`, resolved
`reasoning_effort`, and a `usage` object with the same counters accumulated only
from that branch conversation. Root-conversation usage remains represented in
the aggregate and is not counted as a created branch.
The session completion time and elapsed duration are captured immediately
before hooks begin, so hook runtime is not inspection runtime. Before running
configured hooks the SSE stream publishes `runtime.status` with
`phase=running_post_inspection_hooks` and the nonblank `hook_count`.

The detached producer creates a versioned running record before publishing its
launch JSON, refreshes its update timestamp with a heartbeat while running, then
atomically replaces it with a `0600` record containing task ID, command, PID,
start/update timestamps, and the exact terminal `final` or `error` trace event.
Version 2 failure records additionally contain model, mode, step/tool budgets,
launch repository fingerprint when applicable, and the last eight sanitized
tool-call/tool-output summaries. Version 3 records may also contain the turn's
mode and parent task ID. Each diagnostic payload is capped at 4 KiB and 40
lines. Successful records contain
no failure diagnostic. Readers continue to accept versions 1 and 2.
Diagnostics never contain API credentials, provider endpoints, full
requests/responses, or unbounded repository content; they are not full traces.
Terminal events are written without trace compaction and published to SSE.
Records live under
`~/.git-agent/<project-identity-sha>/background/<task-id>.json` and are retained
indefinitely. The containing directory is `0700`.

`git-agent review-test` is an internal integration fixture. It requires no
arguments, provider authentication, or repository access. It uses the same
detached launch JSON and authenticated local-socket SSE transport as `review`, then
publishes deterministic reasoning-summary, tool-call, tool-output, and final
events. It creates no durable background record and is intentionally omitted
from normal command help and shell completion.

All agent loops use a 217,600-token context budget, 80% of the common
272,000-token model context window. Before the first provider call, a serialized
request estimate at or above that budget fails locally without contacting the
provider. After a successful response, provider-reported input tokens take
precedence over serialized-request estimates. At threshold, runner immediately
makes one tool-free forced-finalization request so model reports all findings
gathered so far. Exact repeated tool calls force finalization because they add
no evidence. Distinct calls may return identical output and still continue
because invocation identity, not result content, defines repeated work. These
progress guards do not reduce configured model-step or tool-call ceilings.

#### `git-agent project_id`

Print exactly one lowercase 64-character project identifier followed by a
newline. The command accepts no arguments. It uses the same project identity as
search metadata: when the containing Git repository has an `origin`, normalize
the first origin URL and print its SHA-256 identity hash; otherwise hash the
cleaned absolute project path. Clones sharing a normalized origin therefore
share an identifier, while repositories without an origin and non-Git
directories remain path-specific. The command does not create an index or call
a provider.

#### `git-agent explore [--debug] [--fast] [--for <diagnose|change|behavior|owner>] [--follow-up <search-id>] <question...>`

Run a synchronous, read-only codebase exploration and write exactly one
newline-terminated JSON object to stdout:

```json
{"id":"opaque-search-id","answer":"agent-ready context pack"}
```

The initial form works in Git repositories and ordinary directories. It first
runs filesystem semantic retrieval with the existing search index, default
retrieval limits, and the code-only filter. It then gives those unverified leads
to a bounded Responses API agent with `repo_summary`, `list_files`, `read_file`,
`inspect_file`, `jq`, `grep`, and `find`. In a Git repository the agent also
receives `git_recent_commits`, `git_head_show`, `git_diff_against_parent`,
`git_show_file_at_rev`, and `git_log_range`. The history tools return bounded
commit metadata, HEAD patches, revision-range logs, and file content from a
specified revision. Commit lists include only commits that change paths beneath
the exploration root. `git_head_show` returns no metadata when HEAD has no
change beneath that root. Patch and file-content results include only paths
beneath the exploration root and render those paths relative to that root.
The tools retain Git-aware repository metadata and tracked internal-path handling, but
the cleaned absolute working directory remains the complete exploration root
even when an ancestor contains `.git`. Semantic results, guidance, agent
environment, and every tool path are relative to that working directory.
Git-backed worktree, index, HEAD, and revision reads rebase those paths through
the containing repository without exposing files above the exploration root.
In an ordinary directory the tools use the same working-directory root, omit
Git metadata and history tools, reject `index` and `head` file sources, and
exclude internal state directories.
The agent must inspect primary implementation owners and contract-defining
tests, return direct answers with exploration-root-relative path-and-line
evidence, and avoid delegating ownership or blast-radius rediscovery to the
caller. Indexing, batch-wait, tool, and provider progress is written only to
stderr. With `--fast`, the Responses API request sends only
`service_tier=priority`; it does not select a different prompt, model, reasoning
effort, budget, cache policy, or search path. Without it, `service_tier` is
omitted.

`--for` selects a compact, target-neutral exploration system prompt and supplies
the selected target as a separate developer instruction. `diagnose` prioritizes
the reproducer, immediate failure mechanism, bottleneck, or regression cause;
`change` prioritizes the implementation boundary, affected behavior, and focused
validation; `behavior` prioritizes current semantics, contracts, and invariants;
and `owner` prioritizes authoritative implementation, callers, and subsystem
boundaries. Every target uses the same system prompt, so changing between target
values cannot leave conflicting target priorities in system instructions.
Omitting `--for` retains the full universal prompt. A missing or unsupported
value fails before semantic retrieval or a provider request. Query-target
selection uses this fixed vocabulary and never reads Codex session history or
`~/.codex` at runtime.

Without `--debug`, stderr contains progress and per-request `llm.usage` metrics.
With `--debug`, every invocation starts one stderr console trace and writes
`explore.phase` events
throughout its owned foreground path. Each event contains `phase`, nonnegative
integer `duration_ms`, and cumulative integer `elapsed_ms` measured from command
entry. Provider-request, tool-batch, and individual-tool timings additionally
contain the one-based model `step`; individual-tool timings also contain `tool`.
The command reports `setup`, `reservation`, `semantic_search`, `join_grace`,
`batch_join`, `batch_collection`, `prompt_setup`, `provider_request`, `tool`,
`tool_batch`, `validation`, `repair` when used, `agent`, `answer_processing`,
`persistence`, `result_wait`, and `output` when those phases execute.
Fresh-search diagnostics also report `semantic_search.<step>` for the
search-owned `sync`, `discover`, `chunk`, `cache`, `embed_index`, `persist`,
`embed_query`, `score`, and `replay` phases that complete. Timings are emitted
after their measured action and may therefore overlap their enclosing
`semantic_search`, `repair`, or `agent` timing.

Each process reports only work it owns. A debug batch leader reports collection,
provider, tool, answer-processing, and persistence timings; a debug follower
instead spends that interval in `result_wait` and does not duplicate the
leader's events. Debug timing and trace output never changes the strict JSON
stdout result.

Explore is a foreground workflow. It does not detach, create a wait endpoint,
or support `--wait`. It imposes no internal wall-clock or HTTP timeout on
semantic retrieval, provider requests or streams, or the multi-turn agent loop;
it runs until completion or caller cancellation, subject to the agent's bounded
step and tool-call budgets. Independently launched processes reserve their
intent before semantic retrieval. Compatible ready intents elect one foreground
leader and form batches of at most three; followers wait for the leader and
receive only their own result. A batch is confined to one cleaned absolute
working directory even when project metadata is shared by clones with the same
origin.
Initial searches in that workspace are mutually compatible only when
they use the same service tier and selected query target. Follow-ups are
compatible only when they name the same parent search ID and use the same
service tier and selected query target.
Every successful batch item receives a distinct opaque ID even when its provider
conversation was
shared with sibling items.

Successful sessions persist in the current project's owner-only metadata
directory. A session records its selected answer, parent ID, follow-up depth,
stable-instruction target, active target, one prompt-cache key, and replayable
Responses API item history. Missing target fields in an existing session mean
the universal target.
An initial batch derives one key from its first sorted item ID and persists that
key for every sibling. Every model request within one agent run keeps
`instructions` byte-stable. Changing model-step and remaining-tool budgets are
appended as developer input and persisted in replay history, so each completed
request input is an exact prefix of the next request input. Hosted-capability
failure notices are also appended instead of rewriting instructions. For
GPT-5.6-family models, each appended budget message is an explicit cache
breakpoint and requests use explicit-only cache mode. Follow-ups inherit the key
and replayable input; a depth reset creates a new key. Official OpenAI models
outside the GPT-5.6 family send the key while retaining provider-default caching.
The authenticated ChatGPT Codex endpoint sends the stable key without explicit
breakpoint options, captures the opaque `x-codex-turn-state` response header, and
replays it on every later request in that agent run to preserve sticky routing.
Custom endpoints receive no prompt-cache fields or Codex turn-state header.
Provider prefix-length, retention, routing, and eviction rules remain
authoritative, so a nonzero cached-token count is not guaranteed.
`--follow-up <search-id>` appends the new natural-language question to stored
context only when invoked from the same cleaned absolute workspace that created
the session. A follow-up without `--for` inherits the parent's active target.
Selecting the already-active target adds no target message. Selecting a
different target appends exactly one replayable developer message beginning
`Query target changed: <target>` followed by that target's guidance before the
new user question. When the parent already uses the target-neutral system
prompt, its `instructions`, input history, and prompt-cache key remain unchanged.
Adding `--for` to a parent that used the full universal prompt replaces
`instructions` with the target-neutral prompt while preserving replayable input
history and the prompt-cache key; the resulting prefix cache miss is accepted.
A target change alone does not run semantic retrieval. A session ID from another
workspace under the same project identity fails before semantic or provider
work, preventing stored context from crossing exploration boundaries.
Sessions created by versions without workspace provenance or a prompt-cache
identity are not follow-up eligible. The parent remains
immutable and reusable, so simultaneous follow-ups from one parent create
distinct sibling IDs rather than serializing through shared mutable state.
Concurrent sibling questions may batch and each resulting ID can itself be
used as the parent of another concurrent batch.

Each branch permits three context-preserving follow-ups after its initial
search. Follow-up depths one through three reuse stored context. A follow-up
against a depth-three ID still succeeds, but it performs a new semantic search,
has no parent, returns a new ID at depth zero, resets the three-follow-up
allowance, and inherits the exhausted session's active target unless `--for`
explicitly selects another target. The reset receives a new prompt-cache key.
An unknown, malformed, unsuccessful, or unreadable ID fails before a provider
request. Any semantic, provider, validation, persistence, leader, or
batch-splitting failure returns nonzero and does not emit a success object on
stdout.

After each batch is sealed, its leader best-effort appends one disposition line
per item to:

```text
${XDG_STATE_HOME:-$HOME/.local/state}/git-agent/<project_id>/explore.log
```

The `git-agent` and project directories are owner-only (`0700`), and the log
and its coordination lock are owner-only files (`0600`). Concurrent batches,
including batches under different parents, serialize appends so records do not
interleave or overwrite one another. A logging failure never changes the
explore result, matching `search_code` debug-log behavior.

Each single-line record contains an RFC 3339 timestamp followed by
`mode=batched|unbatched`, `branch=true|false`, `project_id`, quoted absolute
`workspace`, `batch`, `size`, `item`, `parent`, `depth`, and
`query=[redacted]`. `mode` and `branch` are independent because a follow-up
branch may itself be batched or unbatched. Fresh searches and the depth-three
reset record `branch=false`, an empty parent, and depth zero. Disposition is
recorded before provider execution, so a sealed item remains auditable even if
semantic preparation has succeeded but provider execution or later result
handling fails. Query text is never written to this log.

#### `git-agent search [flags] <query...>`

Run local embedding-backed context search and print machine-readable JSON by
default.
Filesystem mode is the default. Inside a Git repository, it indexes from the
repository root, shares that index across working directories, and limits
results to the current working directory and any narrower `--scope`. Outside a
Git repository, it searches current files under the current working directory.
Files are read exactly as they exist on disk; Git is not otherwise required.
Staged, unstaged, and untracked files are included when physically under the
search root unless skipped by dot-path rules, built-in low-signal
ignore patterns, `.gitignore`, `.gitagentignore`, non-text MIME type, or binary,
oversized-file, and symlink safety checks. Built-in search ignores exclude paths
matching `*.lock`, `*.lockfile`, `bun.lock`, `bun.lockb`,
`Cartfile.resolved`, `cabal.project.freeze`, `Cargo.lock`, `composer.lock`,
`conda-lock.yaml`, `conda-lock.yml`, `cpanfile.snapshot`, `deno.lock`,
`flake.lock`, `Gemfile.lock`, `go.sum`, `mix.lock`, `npm-shrinkwrap.json`,
`package-lock.json`, `Package.resolved`, `packages.lock.json`, `pdm.lock`,
`Pipfile.lock`, `pixi.lock`, `Podfile.lock`, `poetry.lock`, `pnpm-lock.yaml`,
`pubspec.lock`, `renv.lock`, `shard.lock`, `stack.yaml.lock`, `uv.lock`,
`yarn.lock`, `*.bazel`, `*.sha256`, `LICENSE`, `COPYING`, or `NOTICE`.
Accepted chunk bodies are snapshotted in an owner-only operating-system
temporary file for the duration of the search. Embedding batches, hybrid body
scoring, and final excerpts read from that immutable snapshot rather than
rereading possibly changed source files or retaining the complete body corpus
in the Go heap. The snapshot is removed when the search ends and can require
temporary disk space proportional to the accepted source text.
`.gitagentignore` uses the same pattern syntax and per-directory base behavior
as `.gitignore`, but only affects `git-agent search` discovery.
`--scope` accepts comma-separated file or directory paths relative to the
current working directory and limits filesystem or revision discovery to those
paths. Inside a Git repository, scopes are converted to repository-relative
paths before discovery. Ignore files are still resolved from the search root or
committed tree, so root `.gitagentignore`
patterns apply normally to scoped paths such as `--scope foo/`. Visible scopes
share the same physical cache as unscoped search for the same source. Scopes
that include paths normally skipped by default discovery, such as dot/hidden
paths, use a separate `scope-*` cache because they opt into a different physical
candidate universe. Remote scopes are relative to the remote repository root.

Go files with a pre-package heading comment containing `DO NOT EDIT` are indexed
as path-only chunks. Search embeds the filename/language metadata for those
files but excludes generated body content.

`--rev <rev>` switches to revision mode. The command must be inside a Git
repository, resolves the revision to a commit, searches only that committed tree,
and ignores current filesystem contents. Revision mode reads `.gitignore` and
`.gitagentignore` from the resolved commit tree, not from the working tree.

`--remote <url>` switches to remote mode. The command caches the sanitized remote
URL under `~/.git-agent/remotes/<remote-sha>/`, keeps a bare repository at
`repo.git`, resolves `--rev` against that cached repository, and searches the
resolved committed tree. When `--rev` is omitted, remote mode resolves `HEAD`
from the remote default branch and reports `rev` as `HEAD`. Remote mode never
checks out a worktree and never includes untracked, staged, unstaged, or
submodule working-tree content. Cached remote URLs are sanitized before they are
written to manifests, output, debug logs, or completion metadata.

Remote repositories are fetched on first use, when the last successful fetch is
at least 15 minutes old, or whenever `--reindex` is set. Fresh cache hits do not
touch the network. If a requested revision cannot be resolved from the cached
repository, the command fetches and retries before failing. Fetch failures fail
the command clearly rather than silently using stale data.

When a remote fetch is required, search resolves direct revisions from the
remote's advertised refs before transferring the main pack. Revision
expressions that need unavailable commit metadata, such as `HEAD~1`, use a
temporary `blob:none` preflight. The temporary repository is removed before the
command returns. A server without object-filter support is handled by the main
unfiltered fetch; search does not download an unfiltered preflight and then
repeat that transfer.

The main fetch and index build form one cancelable producer/consumer operation.
The received pack is written to the cached bare repository while a temporary
object overlay parses the same bytes. Once the selected commit and tree are
known, search waits for the selected tree's ignore files, builds one ignore
matcher, chunks selected-tree files as they arrive, and dispatches complete
embedding windows while file production remains open. It never embeds blobs
merely because they occur in the pack. The final partial embedding window is
dispatched only after file production closes. Pack ordering, delta bases,
ignore-file availability, reuse lookup, and embedding batch size can delay
overlap, so this concurrency is lossless best-effort rather than a promise that
every fetch overlaps provider work. Blob size is checked before content is read,
and reads remain bounded. A fresh filtered cache classifies intentionally absent
over-limit blobs as oversized during later cache-hit traversal. The final remote
cache contains pack storage, not the temporary parsed objects.

All search index production under one metadata root is a global cross-process
single flight. Before project metadata migration, remote repository
initialization or fetch, source discovery, chunking, embedding, and index
persistence, a search acquires one user-level operating-system lock. This
serializes those phases across local, revision, and remote searches even when
they target unrelated projects or remotes; the owning process may retain the
remote producer/consumer overlap described above. Waiting is context-cancelable
and does not release, delete, or otherwise disturb the active producer's lock.
After acquiring the lock, a waiter re-evaluates the selected source and index;
concurrent requests for the same missing index reuse the completed index, and
concurrent `--reindex` requests for the same selected index perform one fetch
and one rebuild in total. An unrelated request runs afterward and builds its
own index normally. Ordinary search releases the global lock after index
persistence and remote completion, before query embedding and scoring. The warm
`explore` path releases it after local retrieval, then the batch leader
reacquires it for the deferred confirmation described below.

SSH transport tries identities from an available SSH agent first, including
Pageant or the native agent on Windows, then unencrypted default private keys at
`~/.ssh/id_ed25519`, `~/.ssh/id_ecdsa`, `~/.ssh/id_rsa`, and
`~/.ssh/id_dsa`. If agent discovery or signing fails, usable default keys remain
fallbacks. Encrypted private keys require an agent because the command never
prompts for a passphrase. Server host keys are verified against OpenSSH
`known_hosts`; verification is never disabled.

Search does not run the Responses API, call model tools, generate explanations,
or use lexical fallback. It frames and embeds the query
as an implementation-location search when the configured embedding input cap can
include the framing; otherwise it embeds the raw query so user query text is not
truncated away. Search embeds local chunks and performs an exact cosine scan over
the shared vector payload, with a legacy per-index payload fallback. For every
chunk with an available vector, it computes vector relatedness plus normalized
BM25-style body text, path token, and indexed symbol token components, combines
them into the final hybrid score, and then applies `--min-score`. Surviving
candidates are ordered by that same final score. Output and replay history keep
the original query string, not the framed embedding input.

When global `index.remote` is configured, every search confirms the selected
revision store against the remote before returning results. A search either
lists the remote refs itself or, when blocked behind an overlapping producer,
reuses a successful confirmation whose remote observation completed after that
search began. Sequential searches do not reuse an earlier confirmation.

A fresh `explore` first performs a warm-only local search. When every selected
chunk already has a compatible vector, it publishes those semantic leads to the
batch immediately. After batch collection, the leader starts one ordinary
index-only synchronization beside the Responses API request; it waits for both
operations before persisting sessions or publishing answers. Because that
remote observation starts after the batch is collected, it confirms freshness
for every included search. A missing vector makes the warm-only probe stop
before embedding and rerun the original blocking synchronization path. Remote
failure remains terminal even when provider work already completed.

A matching remote-tracking ref and locally present commit skip object fetch; a
clean synchronized worktree skips commit and push. Failed synchronization never
publishes a reusable confirmation. Filesystem mode selects the local
repository's committed `HEAD`; local revision mode selects resolved `--rev`;
remote mode selects resolved `--remote` revision. Remote must be reachable;
list, fetch, or push transport failures fail command explicitly instead of
falling back to independent local rebuild. Non-Git directories and local
repositories without `origin` remain local-only.

Sync implements `pull --rebase` behavior without invoking Git executable. It
commits pending local index-store changes, fetches remote default branch, and
places local changes on fetched head. Diverged index histories merge records
whose embedding model, dimensions, and exact final-input identity are
compatible, then commit resolved state. When local commits were replayed or
merged, search pushes them before inspecting or building current source. Push
rejection fetches, merges compatible records, and retries. Empty remote is
initialized on `main`; otherwise default branch is preserved. Remote repository
is wholly owned by `git-agent` and must not contain unrelated files.

Ordinary search imports selected revision records before ensuring the selected
local index is complete, then publishes compatible records after persistence.
The warm `explore` path may query an already complete working index first; its
deferred index-only synchronization still imports and publishes compatible
records before any answer is persisted. Filesystem mode ensures and publishes
the committed HEAD revision index without exporting working-tree-only vectors,
then builds or queries the actual working tree.
unstaged, and untracked files, but dirty-worktree-only vectors, query history,
absolute roots, locks, temporary files, auth data, and cached bare
repositories are never exported.

`--format json` is the default stdout contract. `--format brief` first writes a
header line as `# mode=<filesystem|revision|remote> index=<fresh|refreshed|built|empty>`,
then writes one result per line as `<score> <path>:<start-line> <summary>`, with
final hybrid score rounded to two decimals. Search applies `--min-score` to that
score after vector, text, path, and symbol components are computed. JSON
`relatedness` is the same final hybrid score; JSON results expose cosine, vector
relatedness, text, path, symbol, lexical, and final hybrid `rank` components in
`scores`, where `scores.rank` equals `relatedness`. The summary is the indexed
symbol name when available, otherwise the first excerpt line without its excerpt line-number
prefix. Brief output suppresses low-information Go `package <name>` results when
another result for the same file has an indexed symbol. `--index --format brief`
writes only the header line because indexing skips scoring.

When stderr is an interactive terminal and `--debug` is not enabled, search
shows transient progress while waiting for the global index worker and while
missing embeddings are built or updated.
The progress line is rewritten and cleared with ANSI control sequences before
stdout is written. Non-interactive stderr receives no progress output.
`--agent` starts a local progress probe server instead of terminal progress when
a remote needs fetching or embeddings need to be built or rebuilt. The server
listens on a private Unix-domain socket and prints one endpoint JSON object to
stderr containing `network`, absolute socket `address`, and the fixed
`http://localhost/progress` request URL. A client dials that socket and receives
JSON for `GET /progress` with status, including `waiting` while another process
owns the global index flight and `fetching` before a
remote network operation and sanitized server-side fetch detail when available,
completed chunk count, total chunk count, reused chunk count, percent, elapsed
milliseconds, and last update time. Interactive terminal mode rewrites the same
remote-fetch detail in place and clears it before stdout. When `--format` is
omitted, `--agent` changes the output format default
from JSON to brief. The server shuts down when the search command exits. Cache-hit
searches that neither wait nor need a remote fetch or embeddings do not start
the server and do not print progress endpoint metadata.

Waiting, remote fetch, and embedding progress callbacks are serialized. While
the fetch is active, `fetching` updates may also carry discovered, completed,
and reused embedding counts; the total can increase until selected-file
production closes.
Terminal completion means both object transfer and all required embedding work
have completed.

Persistent metadata defaults to `~/.git-agent/<path-sha>/`, where `<path-sha>`
is the SHA-256 of the cleaned absolute project root. When a legacy
`<project>/.git-agent/` directory exists, the next project run migrates its
contents into the home metadata directory before writing new data.
Search indexes and background task records use the same project identity
resolver. A local Git repository with `origin` uses SHA-256 of normalized origin
identity; common SSH and HTTPS spellings for the same host and repository path,
including separate clones, share one identity. A repository without `origin`
or a non-Git project falls back to cleaned absolute-path SHA. On first search
use, completed legacy search data under the absolute-path key is merged into the
origin-keyed search store and the obsolete legacy search tree is removed;
non-search metadata remains under its existing key. Search identity migration
applies even when index sync is not configured.
Remote metadata is stored under `~/.git-agent/remotes/<remote-sha>/`, where
`<remote-sha>` is the SHA-256 of the sanitized remote URL. Remote search indexes
are stored under that remote metadata root and are keyed by resolved commit SHA,
so moving branches create new revision indexes while old commit indexes remain
reusable.

Normal indexing may seed a missing or changed physical index from another
completed index under the same project or remote metadata root. Reuse crosses
filesystem and revision indexes and crosses revision commit SHAs. A chunk vector
is reusable only when its embedding model, dimensions, and exact final capped
embedding input match. Reused vectors are written with the target chunk's source,
blob, path, and line metadata. Search prefers the compatible index with the most
matching chunk inputs and embeds every unmatched target chunk normally. Invalid,
incomplete, or incompatible candidate indexes are ignored. `--reindex` does not
seed from other physical indexes; the existing same-target parallel-writer reuse
still applies. Query replay history remains scoped to its physical index.

Compatible chunk vectors are stored once per project or remote metadata root in
an append-only shared payload under `search/vector-store/`. Each physical
filesystem or revision vector index keeps its own chunk metadata and immutable
shared payload references, so snapshots retain their source, blob, path, and line
identity without copying unchanged float payloads. Shared identity combines the
embedding model, dimensions, and SHA-256 of the exact final capped provider
input. Query embeddings and query history are not stored in the shared vector
store.

Shared-store writes use one metadata-root lock. A writer appends new float
payloads, syncs them, publishes an immutable catalog generation, and only then
publishes the snapshot index manifest. Concurrent snapshot writers can perform
provider work independently, but catalog publication keeps one physical payload
for each compatible identity. A checksum and identity key on every shared
snapshot reference prevent corrupt or mismatched payloads from being used.
Missing or corrupt shared records are treated as cache misses and rebuilt; an
interrupted append can leave unreachable bytes but cannot publish a partial
snapshot reference.

Existing per-index binary payloads remain readable and migrate to shared
references on the next successful cache write without another embedding call.
Shared-reference indexes use format version 3 so older version 2 readers reject
them instead of interpreting shared offsets as local payload offsets. Version 2
indexes remain readable by the current binary for migration.
Records from older formats that lack a provable final-input hash remain in the
physical index's local payload until that chunk is re-embedded. The shared
payload is append-only: automatic garbage collection and compaction are not
performed. `--reindex` embeds the selected candidate set and appends a new shared
record generation for those rebuilt identities. Other snapshots continue to
reference their prior immutable records; a reindex never replaces vectors under
them. Parallel `--reindex` waiters for the same physical index still reuse the
first completed writer instead of appending another generation.

Chunk embedding text clamps each physical source line to `4000` characters
before applying the per-input embedding character cap. This bounds minified or
single-line generated files without changing file discovery, chunk ranges, or
result excerpts.

`--code` narrows the candidate set for the current search or indexing run to
source-code files before chunking and embedding missing chunks. It is intended
for implementation-location searches where docs would otherwise rank above code.
The filter is extension-based and currently includes:
`.go`, `.js`, `.jsx`, `.ts`, `.tsx`, `.mjs`, `.cjs`, `.py`, `.rb`, `.rs`,
`.java`, `.kt`, `.kts`, `.c`, `.h`, `.cc`, `.hh`, `.cpp`, `.hpp`, `.cs`,
`.php`, `.swift`, `.scala`, `.sh`, `.bash`, `.zsh`, `.fish`, `.ps1`, `.sql`,
`.html`, `.css`, `.scss`, `.sass`, `.vue`, and `.svelte`.
`--code` runs after normal filesystem or revision discovery, ignore matching,
and safety checks. It does not exclude test files or test directories by name,
so files such as `foo_test.go` and `*.spec.ts` are included when their extension
matches. In filesystem mode, staged, unstaged, and untracked matching files are
included when physically under the search root and not skipped or ignored. In
revision mode, only matching files from the resolved committed tree are
included. Generated Go files with a pre-package heading comment containing
`DO NOT EDIT` are still included by `--code`, but they are indexed as path-only
chunks; their generated body content is not embedded. `--code` shares the same
physical vector cache as default search for the same physical source cache:
default searches can reuse code vectors written by `--code`, and `--code` can
reuse code vectors written by default search. Replay history remains
filter-aware, so default result history is not replayed as a `--code` result
history entry.
`--code` does not introduce a lexical fallback.

`--no-tests` filters common test files from search results and `--ls-files`
output without changing the physical vector cache. It filters path segments named
`test`, `tests`, `__tests__`, `spec`, `specs`, `__specs__`, `integration_test`,
`integration_tests`, `integration-test`, or `integration-tests`. It also filters
basenames whose extensionless name contains a `.`, `-`, or `_` delimited `test`,
`tests`, `spec`, `specs`, `unittest`, or `unittests` segment. This includes common
forms such as `test_*.py`, `*_test.rs`, `*_tests.rs`, `*_spec.rb`,
`*.test.ts`, and `*-unittest.cc`. For common class-based source languages, it
also recognizes names such as `TestWidget.java`, `WidgetTest.java`,
`WidgetTests.cs`, and `WidgetTestCase.kt`. Similar non-test words such as
`contest`, `latest`, and `testimonial` do not match. `testdata` remains available
because fixtures can be useful implementation context.

`--index` builds missing embeddings for the selected filesystem or revision
source, including any `--scope` and `--code` candidate filters, writes the same
JSON envelope with an empty result list, and skips query embedding, scoring,
replay history, and semantic search. `--no-tests` does not change the indexed
candidate set. `--index --reindex` rebuilds embeddings for the selected
candidate set even when cache entries already exist. Successful indexing writes
the local cache after all missing embeddings complete. Cache writes replace the
stored vector index with the current candidate set, dropping entries for deleted
or newly ignored files. Before replacing snapshot files, a writer removes and
syncs the prior manifest; it durably writes the vector files, then publishes and
syncs a new manifest. Interrupted or failed writes
therefore remain incomplete and are rebuilt instead of being queried as a
completed mixed snapshot. `--code` writes still preserve current
non-code entries in the shared physical cache so default searches can reuse
them. Visible `--scope` writes similarly preserve current out-of-scope entries
in the shared physical cache. `--no-tests` does not alter the indexed candidate
set, so cache writes retain test-file vectors even when `--no-tests` filters
results or `--ls-files` output. Empty candidate sets can be persisted so
`--reindex` can clear a stale index. Parallel searches for the same physical
index source use
one index writer. Other processes wait for the writer, reload the completed
cache, and skip embedding chunks that the writer just stored; parallel
`--reindex` waiters also reuse a cache completed after their command started.

For remote indexing, successful pack transfer alone does not publish
`remote.json`, shared-vector updates, a snapshot manifest, history, or index-sync
export. Publication starts only after the selected file producer and all index
embedding requests succeed. A fetch, pack-parse, progress, cancellation, or
embedding failure cancels its peers, removes temporary overlay storage, and
leaves no completed snapshot for that attempt. Provider results completed before
such a failure remain process memory only.

#### `git-agent search --ls [--remote <url>] [--format text|json]`

List completed local search indexes for the current project. With `--remote
<url>`, list completed indexes for that cached remote instead. The command
resolves the metadata root the same way search does and walks its `search/`
directory for valid `manifest.json` files. Incomplete or incompatible index
directories are skipped.

Default `--format text` writes one human-readable entry per index with mode,
optional short revision, root, path-derived filters (`scope-*` only for scopes
that opt into normally skipped paths, plus legacy `code`), file count, chunk
count, embedding model, dimensions, created time, and the absolute index
directory path. With `--remote`, text output first writes the absolute cached
bare-repository path as `remote repo=<path>`, including when no completed indexes
exist. `--format json` preserves the index-array contract; cached repository
inventory remains available through `--ls-remotes --format json`. The command
does not call embedding providers and does not require API keys.

#### `git-agent search --ls-remotes [--format text|json|completion]`

List cached remote repositories from `~/.git-agent/remotes/`. The command reads
remote metadata only; it does not clone, fetch, embed, query, or require API
keys. Default `--format text` writes one entry per remote with sanitized URL,
optional last resolved revision, last successful fetch time, and cache
directory. `--format json` writes a JSON array of the same fields.
`--format completion` writes one sanitized URL per line for shell completion
helpers.

#### `git-agent search --ls-files [--format tree|json] [--remote <url>] [--rev <rev>] [--scope <paths>] [--no-tests]`

List unique file paths stored in one selected search index. Filesystem indexes
inside Git repositories and all revision and remote indexes use
repository-relative paths. Non-Git filesystem indexes use search-root-relative
paths.
Index selection uses the same physical cache keying as search for filesystem or
`--rev`/`--remote` sources. Visible `--scope` values use the shared source cache
and filter listed output to the scoped paths. Scopes that include normally
skipped paths use their separate `scope-*` cache. With `--no-tests`, the command
uses the same index and filters test paths from the listed output. The command
does not clone or fetch remote repositories. When no usable index is present, the
command fails with an error that points at the expected index directory and
suggests `git-agent search --index`.

Default `--format tree` writes a rooted tree of indexed files using box-drawing
characters. `--format json` writes an object with the selected index summary and
a sorted `files` array. The command reads index metadata and path lists only; it
does not load embedding vectors or call providers.

#### `git-agent config index.remote [<git-url>]`

With `<git-url>`, set global dedicated index Git remote. Without value, print
sanitized URL. Configuration is stored at
`${XDG_CONFIG_HOME:-~/.config}/git-agent/config.json` with private file
permissions. `git-agent config --unset index.remote` removes setting. Unknown
keys, empty values, extra arguments, and reading unset key fail. Sync uses same
pure-Go transport and authentication behavior as search `--remote`; it never
invokes Git executable or interactive prompt.

#### `git-agent index sync`

Perform explicit full-machine index sync. Command requires configured
`index.remote`, pulls/rebases dedicated index repository once, inventories local
metadata, and additively exports every completed revision or cached remote
revision index that has identifiable source origin. Filesystem indexes are not
exported. Command does not discover source files, create embeddings, query a
provider, or require embedding credentials.

Compatible local records merge with remote snapshots. Remote snapshots absent
locally remain unchanged; command never prunes another machine's revisions.
Corrupt, incomplete, incompatible, or no-longer-identifiable local revision
indexes are skipped. After inventory, command creates at most one merged index
commit and performs one final push; pull/rebase may first push replayed pending
local sync commits as required by sync ordering. Progress is written to stderr
while fetching remote state, scanning local indexes, syncing each eligible
index, and pushing merged state. Fetch and push object-transfer updates reuse
sanitized go-git transport progress and append it as a bracketed suffix, such as
`index sync: fetching remote [Receiving objects: 42%]`; phase-only progress
remains visible while transport totals are unavailable. Interactive stderr
rewrites one transient line with ANSI control sequences and clears it before
stdout. Non-interactive stderr writes each update as a newline-delimited log
line without ANSI control sequences. Index sync does not start a progress probe
server. Stdout is exactly
`synced indexes=<n> records=<n> skipped=<n>` followed by newline. Transport,
configuration, locking, and unsafe-tree failures fail command explicitly.
Generated index-store commits are unsigned: each dedicated local sync
repository persists `commit.gpgSign=false` before committing, overriding wider
Git configuration without changing source repositories or remote-search caches.

The dedicated repository contains a mandatory `schema.json`. Clients parse the
complete schema document before reading, committing, or pushing index data and
reject malformed, unknown, or future versions. Schema v1 stores complete vector
records in per-revision JSON snapshots. While the repository remains v1,
clients continue writing v1.

Schema v2 stores vectors as canonical little-endian float32 payloads in
immutable, content-addressed packs under
`packs/<model-key>/<pack-sha256>.pack`. Both keys are full lowercase SHA-256
values. Pack entries contain the embedding identity, exact payload SHA-256, and
CRC32 checksum; revision manifests under
`indexes/<origin-sha256>/<revision-sha1>/<model-key>.json` contain metadata and
pack/slot references. Import validates the pack path, complete pack digest,
header version, model, dimensions, slot, embedding identity, payload digest,
and checksum before publishing a local index. A derived pack catalog may be
cached only below the sync repository's `.git` directory and is never
committed. The cache directory retains at most the current local `HEAD`
catalog. Opening a schema-v2 sync repository or publishing a new catalog
removes owned historical catalogs and abandoned catalog temporary files.
Missing, malformed, stale, or invalid catalog data is rebuilt from the
immutable vector packs.

Concurrent schema-v2 writers merge pack files by content-addressed path and
merge manifests by cache-record identity. Identical payloads choose the
lexicographically smallest pack/slot reference. Different payload digests for
the same record fail reconciliation rather than silently selecting one. Normal
sync is additive and does not remove unreferenced packs or revision manifests.

#### `git-agent index migrate --to v2 [--dry-run]`

Perform the explicit schema-v1 to schema-v2 transition while holding the sync
repository lock. `--dry-run` clones and validates authoritative remote state in
temporary storage, constructs the prospective packs and manifests, and writes
exactly one stdout summary without committing or pushing:
`migration from=1 to=2 indexes=<n> records=<n> unique_vectors=<n> packs=<n>
current_bytes=<n> projected_bytes=<n> saved_bytes=<n> dry_run=true`. The normal
form validates every v1 snapshot, builds and revalidates a complete v2 tree,
publishes schema v2, commits once, pushes through normal reconciliation, and
writes `migrated from=1 to=2 indexes=<n> records=<n> unique_vectors=<n>
packs=<n> bytes=<n>` to stdout. Repeating migration on a v2 repository is a
successful no-op. If a schema-v2 repository contains canonical 16-character
schema-v1 manifests, migration treats it as a recoverable interrupted or mixed
migration: it strictly validates each v1 manifest and its metadata/path match,
merges its records into canonical v2 packs and 64-character manifests, removes
all v1 manifest paths from the current tree, commits the additions and removals,
and pushes the repaired tree. This recovery is exclusive to `index migrate`;
normal index sync continues rejecting mixed-schema trees. Malformed manifests,
metadata/path mismatches, symlinks, unrelated paths, and unknown schemas fail
without publishing a repair. Dry-run constructs and validates the same repaired
tree without changing local or remote Git state. A repair writes the existing
summary forms with `from=2 to=2` and counts the recovered v1 indexes and records.
Migration changes only the current tree; history rewriting, retention, pruning,
and pack compaction are not performed.

Migration progress uses the same stderr rules as index sync. It reports
`index migrate: fetching remote`, `index migrate: scanning v1 snapshots`, and
`index migrate: building indexes <done>/<total>` for both forms. A non-dry-run
migration additionally reports `index migrate: installing schema v2` and
`index migrate: pushing remote`. Mixed-schema recovery uses the same phases and
may scan both the pending local tree and fetched authoritative tree before it
publishes one strictly valid v2 result. Building updates include percentage and
elapsed time once at least one index is complete. Fetch and push transport
details use the same bracketed suffix as index sync. Interactive stderr
rewrites and clears one transient line; non-interactive stderr emits each
update as a newline. Progress callback failures abort the operation. Progress
does not change either command's exact stdout summary.

#### `git-agent index gc [--dry-run]`

##### GC-001 — Command and scope

`git-agent index gc` is the single explicit garbage-collection entry point.
It accepts only one optional `--dry-run`; duplicate `--dry-run`, `--remote`,
positional arguments, and unknown options fail with
`usage: git-agent index gc [--dry-run]`. Eligible local metadata roots are
exactly direct directories named by 64 lowercase hexadecimal characters at
`~/.git-agent/<id>` and `~/.git-agent/remotes/<id>`. Within each root, GC owns
only the `search` subtree and recognizes index candidates through the exact
manifest and payload filenames defined below. It does not enter
`~/.git-agent/index-sync`, cached bare `repo.git` trees, query-lock directories,
or vector-store internals as index candidates. Every traversed owned directory
and file must be a non-symlink beneath its lexical metadata root; a symlink,
non-regular owned file, containment escape, or malformed recognized generation
filename fails preflight.

An unset `index.remote` does not fail garbage collection: the command completes
the local phase and skips shared-repository cleanup. When `index.remote` is
configured, shared cleanup runs after every selected local store has published.
Normal `git-agent index sync` remains additive and never performs garbage
collection implicitly.

After every selected phase succeeds, stdout is exactly one newline-terminated
summary:

`gc local_stores=<n> local_compacted=<n> local_vectors=<n>
local_removed_vectors=<n> local_current_bytes=<n> local_projected_bytes=<n>
local_saved_bytes=<n> remote_configured=<true|false>
remote_removed_packs=<n> remote_current_bytes=<n>
remote_projected_bytes=<n> remote_saved_bytes=<n>
dry_run=<true|false>`

`local_stores` is the number of regular `search/vector-store` directories that
complete preflight, including empty stores. `local_compacted` counts stores
whose projected recognized file set or bytes differ and therefore would change
in normal mode. `local_vectors` sums distinct live vector keys per store; the
same key in different stores counts once in each. `local_removed_vectors` sums
current-catalog keys absent from that store's complete live-root set.

Local `current_bytes` is the sum of logical `FileInfo.Size` values for recognized
vector-store catalogs and payload generations plus recognized incomplete or
superseded legacy payload candidates before GC. `projected_bytes` is the same
sum for the exact retained or published file set after GC, including both
retained recovery catalogs and every distinct payload they reference.
`local_saved_bytes` is signed `current_bytes - projected_bytes`. Unknown files
are preserved and excluded from both byte totals. Remote byte counts use
logical sizes for the tracked current schema-v2 tree before and after the final
successful cleanup; they exclude `.git` data and historical objects.
`remote_removed_packs` is derived from the authoritative base of the tree that
was actually pushed, not an earlier conflicted attempt.

A skipped remote phase reports `remote_configured=false` and zero for every
remote count. Dry-run and normal mode calculate identical candidates and
summary values from identical starting state. A store is an idempotent no-op and
does not publish another generation when its live key mapping, retained
generation pair, recognized payload bytes, and cleanup candidates already equal
the deterministic projected state. A failure writes no success summary. Local
stores published before a later failure remain valid; repeating the command
recalculates all counts from current state.

##### GC-002 — Dry run and progress

`--dry-run` performs the same discovery, locking, strict validation,
reachability calculation, and byte accounting but does not publish a local
catalog or payload, delete a file, mutate the persistent local index-sync
repository, create a commit, or push. A configured shared remote is inspected
through disposable state.

Garbage-collection progress is stderr-only. Local phases report
`index gc: scanning local indexes` and
`index gc: compacting local stores <done>/<total>`. A configured shared phase
additionally reports `index gc: fetching remote`,
`index gc: scanning shared indexes`, `index gc: pruning shared packs`, and, for
a changed non-dry-run tree, `index gc: pushing remote`. Fetch and push transport
details use the existing sanitized bracketed suffix. Interactive stderr rewrites
and clears one transient line; redirected stderr emits newline-delimited
updates. Progress callback failure aborts the operation without a success
summary.

##### GC-003 — Local roots and exact reachability

Every strictly valid completed local manifest is a live root regardless of age,
revision, branch, tag, or observed use. Version-1 manifests retain their local
payloads. For each version-2 `shared-v1` manifest, garbage collection strictly
validates `vectors.index.json`; every shared vector key must match its embedding
input, model, and dimensions and must resolve through the current vector-store
catalog to payload bytes with matching dimensions and checksum. Index-local
records without a shared vector key remain owned by that index and their local
`vectors.f32` data is not compacted.

For each metadata root, the existing vector-store lock becomes the lifecycle
lock for shared-vector publication. Writers acquire their index lock first,
then acquire this lifecycle lock before invalidating the old manifest, and hold
it through vector-store updates, index payload publication, and the final
atomic manifest publication. GC acquires only the lifecycle lock while it
discovers and validates the complete stable set of valid shared-vector roots,
builds a candidate, and publishes that store; it never waits for an index lock
while holding the lifecycle lock. This preserves the existing writer lock order
and prevents a new manifest from appearing with a vector key omitted by GC.

Every code path that selects a shared-vector catalog or opens a shared payload
also participates in the lifecycle lock. When it already owns an index lock, it
acquires the lifecycle lock second and holds it through catalog selection,
payload open, every referenced read, and payload close. A reader without an
index lock acquires only the lifecycle lock and must not acquire an index lock
before releasing it. GC's exclusive lifecycle ownership therefore waits for
pre-existing readers before publishing and removing generations, while new
readers cannot select an old catalog during replacement. The universal order is
`index lock → lifecycle lock`; no path may acquire these locks in reverse.

After releasing the lifecycle lock, GC processes recognized incomplete or
superseded per-index payloads one index directory at a time in sorted path
order. It acquires that index's existing lock, rescans the directory, and
deletes only candidates that remain incomplete or superseded. A concurrent
writer therefore completes before classification, and a newly valid manifest
is preserved. No vector-store lock is held during this per-index cleanup.

Recognized index payload filenames are exactly `manifest.json`,
`vectors.index.json`, `vectors.f32`, and `embeddings.json`. A directory that
still lacks a valid completed manifest after its index lock is acquired is
incomplete. GC may remove only its recognized payload files and empty owned
directories; any unknown entry preserves the directory. A valid version-2
manifest may own superseded `embeddings.json` or obsolete index-local payload
bytes only when every record is a validated shared reference; GC removes only
those recognized superseded files. Version-1 manifests and version-2 indexes
with local-only vector records retain their index-local payloads. Malformed
completed metadata, unknown versions, invalid shared references, checksum
failures, symlinks, containment failures, and unreadable owned data fail
preflight before any store publishes.

##### GC-004 — Generation-safe local compaction

Shared-vector reads resolve a record's authoritative offset, dimensions, and
checksum from the current vector-store catalog by vector key. The offset copied
in an existing `vectors.index.json` is not authoritative after compaction.
Catalog lookup must still validate the record's expected key, dimensions, and
checksum, so unchanged completed indexes remain readable when live vectors move.

A compacted store writes one payload containing exactly the distinct live
vectors in deterministic vector-key order. Recognized generation filenames are
only `catalog-<20-decimal-digits>.json`, `vectors-<20-decimal-digits>.f32`, and
the legacy payload `vectors.f32`. A catalog payload field must be a basename
matching one recognized payload filename; separators, traversal, absolute
paths, symlinks, and other names fail validation. Existing catalogs without a
payload filename refer only to legacy `vectors.f32`.

Compaction reserves two consecutive catalog generations. It fully writes and
syncs `vectors-<first-generation>.f32`, publishes and syncs a recovery catalog
for the first generation, then publishes and syncs an identical current mapping
at the second generation. Both catalogs reference the new compact payload.
Only after the second catalog and directory are durable may GC remove older
recognized catalogs and payloads. It retains exactly the two new catalogs and
every distinct payload they reference. At every interruption boundary either
the old retained generation or one of the new catalogs identifies a complete
payload. Normal append writes continue retaining a current and immediately
previous valid catalog and every payload they reference.

Local stores publish independently. The command preflights all selected stores
before publishing the first, but no cross-directory transaction is promised.
If publication of a later store or the optional shared phase fails, earlier
valid compactions remain and the command returns an error.

##### GC-005 — Optional shared-repository cleanup

When `index.remote` is configured, GC inspects authoritative schema-v2 state
under the existing index-sync ownership and repository lock. It strictly
validates every current manifest and vector pack, marks every pack referenced by
every valid manifest, and selects only current-tree pack files with zero
references. It preserves every valid manifest, referenced pack, `schema.json`,
and the existing unsafe-tree rejection rules. A schema-v1, mixed, malformed,
unknown, or unsafe tree fails without cleanup.

Dry-run constructs and validates the prospective tree in disposable storage.
Normal execution removes the selected files, validates the complete resulting
tree again, creates an unsigned cleanup commit, and attempts the push. On every
non-fast-forward response, GC fetches the newly authoritative tree, discards the
stale removal plan, reruns strict validation and reachability, reapplies only
the newly unreferenced removals, validates again, and replaces the attempted
cleanup commit before retrying. A pack newly referenced by the authoritative
tree is never removed. The success summary is computed from the authoritative
base and exact tree accepted by the final push. The successfully pushed result
contains at most one cleanup commit above that base. A no-op creates no commit
or push. Cleanup changes only the current tree: it does not rewrite Git history,
remove historical Git objects, or run general Git garbage collection.

##### GC-006 — Compact derived pack catalog

The uncommitted current-HEAD vector-pack catalog uses this exact binary layout:

| Offset | Width | Field |
| ---: | ---: | --- |
| 0 | 8 | magic `GITAGCT\0` |
| 8 | 4 | little-endian uint32 version `2` |
| 12 | 20 | raw Git HEAD SHA-1 |
| 32 | 4 | little-endian uint32 pack count |
| 36 | 8 | little-endian uint64 entry count |
| 44 | `pack_count * 36` | pack rows |
| next | `entry_count * 72` | slot rows |
| final | 32 | SHA-256 of every preceding byte |

Each pack row is a raw 32-byte pack digest followed by its little-endian uint32
slot count. Pack rows are strictly sorted by digest and include every regular
`.pack` in the current validated pack tree, including packs whose identities
duplicate slots in another pack. Each slot row is a raw 32-byte embedding key,
raw 32-byte vector digest, little-endian uint32 pack-table index, and
little-endian uint32 slot. Slot rows are strictly ordered by pack-table index
then slot, cover every slot from zero through each pack's declared count exactly
once, and match that pack's validated entry table. Loading those rows through
the existing canonical selection rule reconstructs the complete catalog,
including deterministic selection when duplicate identities occur across
packs.

Decoding rejects a wrong magic or HEAD, unknown version, count or size overflow,
truncation, trailing bytes, checksum mismatch, duplicate or unsorted packs or
slots, missing or extra current-tree packs, an out-of-range pack-table index or
slot, incomplete slot coverage, and any embedding/vector mismatch with the
validated pack entry. Rejected or absent data is reconstructed from immutable
packs. A legacy Gob cache is recognized only as an upgrade candidate: after its
existing validation succeeds, GC performs a complete authoritative pack scan
and writes the resulting binary cache rather than deriving completeness from
the legacy entries. New writes use only version 2. Atomic publication and
current-HEAD historical-cache pruning remain unchanged.

With `--debug`, search writes live human console diagnostic events to stderr
using the same renderer as streamed traces. It writes one `search_skip` event per
file or directory skipped by git-agent's own safety rules, including dot paths,
symlinks, oversized files, binary files, non-text MIME types, unreadable paths,
and non-regular files. Paths skipped only by built-in low-signal ignores,
`.gitignore`, or `.gitagentignore` patterns are not reported. While embedding
missing index chunks, `--debug`
writes live `search_timing`, `search_embed_plan`, and `search_embed_progress`
events. `search_embed_plan` includes the number of embedding batches and the
concurrent request limit chosen for the run. `search_embed_progress` includes
compact completed/total percent progress, elapsed embedding time, average
elapsed time per embedded chunk, and client-side embedding call duration for
that batch.

### Flags

Every command accepts the global form
`git-agent --cwd <directory> <command> [args...]`. `--cwd` must precede the
subcommand, requires one nonempty value, and may occur at most once. Relative
values resolve from the caller's original working directory; absolute values
are used directly. Git Agent changes to the selected directory before command
dispatch, so repository discovery, search roots and scopes, guidance, relative
path arguments, working-directory-sensitive configuration, and detached child
processes observe that directory. Failure to enter the directory returns
nonzero before the subcommand runs and emits none of its normal stdout output.
Without `--cwd`, existing dispatch behavior is unchanged.

For `explore`, the selected directory is also the complete semantic, guidance,
agent-environment, and read-tool boundary. An ancestor Git repository may still
provide project identity, session metadata, repository summary fields, tracked
internal-path handling, and index/HEAD sources, but it does not widen that
boundary.

Message-generation subcommands reserve this shared flag surface:

- `--model`
- `--fast`
- `--low`
- `--medium`
- `--high`
- `--xhigh`
- `--base-url`
- `--timeout`
- `--max-steps`
- `--guidance-family`
- `--append-prompt <text>`
- `--debug`
- `--pprof <addr>`

`explore` supports `--debug` for console trace and phase timing events, `--fast`
with the shared service-tier behavior, and its command-specific
`--follow-up <search-id>` form.

`review` and `simplify` additionally support
`--depth fast|balanced|thorough`, `--max-web-searches <positive-n>`, and the
isolated `[--fast] --follow-up <turn-id> <prompt...>` form. They also support
`--help-agent`, which prints only the launch syntax, scope modes, `--depth`,
reasoning-effort flags, and follow-up syntax intended for automated coding
agents. The agent help reserves `thorough` for security-related issues or very
complex logic and directs agents to use `fast` or `balanced` otherwise.
`--wait <id>` is valid only as the isolated retrieval form documented above.
`--depth` and `--max-steps` are mutually exclusive.

`release-note` additionally supports:

- `--out <file>`: write rendered Markdown to file and stream human console trace
  to stdout

`search` additionally supports:

- `--scope <paths>`: comma-separated paths to search or index; local paths are
  relative to the current directory, while remote paths are relative to the
  remote repository root
- `--limit <n>`: default `20`, valid `1..100`
- `--format`: search accepts `json|brief` and defaults to `json`; `--ls`
  accepts `text|json` and defaults to `text`; `--ls-remotes` accepts
  `text|json|completion` and defaults to `text`; `--ls-files` accepts
  `tree|json` and defaults to `tree`
- `--code`: search source-code files only
- `--no-tests`: exclude common test files and test directories from results and
  `--ls-files` output
- `--agent`: serve current indexing progress over a private local socket
  when embeddings need to be built or rebuilt; defaults output to brief unless
  `--format` is set
- `--index`: build embeddings for the selected source without searching
- `--reindex`: rebuild embeddings for the selected source and drop stale cache
  entries
- `--ls`: list search indexes for the current project or `--remote` cache
  without embedding or querying
- `--ls-remotes`: list cached remote repositories without embedding, fetching,
  or querying
- `--ls-files`: list files in the selected search index without embedding or
  querying
- `--remote <url>`: search or inspect a cached remote Git repository URL
- `--rev <rev>`: search a committed Git tree instead of current filesystem files
- `--min-score <score>`: minimum final hybrid score threshold;
  default `0.70`, valid finite `0 < score <= 1`
- `--embedding-model <model>`: default `text-embedding-3-small`
- `--embedding-dimensions <n>`: default `1024`, valid positive integer
- `--base-url <url>`: override provider base URL
- `--timeout <duration>`: override default request timeout
- `--debug`: enable diagnostics on stderr
- `--pprof <addr>`: serve Go pprof endpoints on the requested address

Flag behavior:

- `--fast`: send `service_tier=priority`
- `--low`: send `reasoning.effort=low`
- `--medium`: send `reasoning.effort=medium`
- `--high`: send `reasoning.effort=high`
- `--xhigh`: send `reasoning.effort=xhigh`
- `--append-prompt <text>`: append a bounded `## Operator hint` section to the
  task user prompt. The hint is escaped inside `<operator_hint>` tags and is
  explicitly lower priority than task instructions, tool policy, project
  guidance, and authoritative repository evidence.
- `--pprof <addr>`: bind the requested address and serve `/debug/pprof/`
  endpoints until the command exits
- default: omit `service_tier`; omit `reasoning` for commands other than
  `review` and `simplify`; for those inspection commands, use the
  depth-derived reasoning defaults documented above

`commit-msg` and `commit` additionally support:

- `--amend`

### Authentication and environment variables

Default auth uses ChatGPT/Codex credentials from `~/.codex/auth.json`.
The file must set `"auth_mode": "chatgpt"` and include
`tokens.access_token` plus `tokens.account_id`. ChatGPT auth defaults the
provider base URL to `https://chatgpt.com/backend-api/codex` and sends
`Authorization: Bearer <access_token>` plus
`ChatGPT-Account-ID: <account_id>`. ChatGPT requests also send
`originator: codex_cli_rs` and `User-Agent: codex_cli_rs`; both client identity
headers are required for current model routing.

`OPENAI_API_KEY` is a legacy fallback for OpenAI-compatible providers when
`~/.codex/auth.json` is absent.
`OPENAI_BASE_URL` applies only to that legacy API-key path; ChatGPT auth uses
`https://chatgpt.com/backend-api/codex` unless `--base-url` is passed
explicitly.
`search` requires an embeddings API key. It reads `OPENAI_EMBEDDING_API_KEY`
first so embeddings credentials can stay separate from message-generation auth,
then falls back to `OPENAI_API_KEY`. Codex/ChatGPT auth is not used for
embeddings. It reads `OPENAI_EMBEDDING_BASE_URL` before `OPENAI_BASE_URL` for
the same isolation. `OPENAI_EMBEDDING_MODEL` changes the default search
embedding model without changing `OPENAI_MODEL`; `OPENAI_EMBEDDING_DIMENSIONS`
changes search embedding dimensions without changing non-search model usage.
`OPENAI_EMBEDDING_MAX_INPUT_CHARS` changes the per-input embedding cap from the
default `32000` characters. `OPENAI_EMBEDDING_BATCH_INPUTS` changes the maximum
inputs per embedding request from the default `32`;
`OPENAI_EMBEDDING_BATCH_MAX_CHARS` changes the maximum total characters per
embedding request from the default `700000`; `OPENAI_EMBEDDING_CONCURRENCY`
changes the concurrent embedding request limit from the default
`min(GOMAXPROCS, 8)`.
The selected account/backend must have embeddings access and quota; otherwise
search fails clearly and does not fall back to lexical retrieval.
Supported environment variables:

- `OPENAI_API_KEY`
- `OPENAI_BASE_URL`
- `OPENAI_MODEL` (overrides the default message-generation model,
  `gpt-5.6-luna`)
- `OPENAI_EMBEDDING_API_KEY`
- `OPENAI_EMBEDDING_BASE_URL`
- `OPENAI_EMBEDDING_MODEL`
- `OPENAI_EMBEDDING_DIMENSIONS`
- `OPENAI_EMBEDDING_MAX_INPUT_CHARS`
- `OPENAI_EMBEDDING_BATCH_INPUTS`
- `OPENAI_EMBEDDING_BATCH_MAX_CHARS`
- `OPENAI_EMBEDDING_CONCURRENCY`

Resolution order:

1. explicit CLI flag
2. `~/.codex/auth.json` ChatGPT auth
3. environment variable fallback, including `OPENAI_API_KEY` auth
4. internal default when defined by that subsystem

For ChatGPT auth, message generation canonicalizes the public `gpt-5.6` alias
to `gpt-5.6-sol` because the ChatGPT Codex endpoint accepts the canonical model
identifier. `gpt-5.6-sol`, `gpt-5.6-terra`, and `gpt-5.6-luna` pass through
unchanged. API-key providers retain the requested model identifier.

### stdout / stderr contract

- stdout for generation-only commands: final generated artifact only
- stdout for `review` and `simplify` launchers: one strict JSON object containing
  `command`, `id`, `pid`, and `url`
- stdout for `review --follow-up ...` and `simplify --follow-up ...`: the same
  strict launch object for the newly allocated turn
- stdout for `review --wait <id>` and `simplify --wait <id>`: the stored strict
  final report JSON only
- stdout for `search`: JSON result by default; brief header and result lines
  with `--format brief`
- stdout for `release-note --out <file>`: streaming human console trace lines
  while generating the release note; the rendered Markdown is written to the
  requested file after a preflight writable check
- stdout for `commit` / `commit --amend`: streaming human console trace lines
  while generating the message, followed by Git's raw commit summary after
  success
- stderr: per-request `llm.usage` metrics for every Responses-backed command,
  diagnostics, console-formatted debug output, search and index-sync progress,
  `--agent` progress probe endpoints, validation failures, provider/tool loop
  summaries when `--debug` is enabled, and stderr emitted by a successful
  delegated `git commit`
- each `llm.usage` line reports the one-based model `step`, `input_tokens`,
  `cached_input_tokens`, `cache_write_input_tokens`, and `output_tokens`; fields
  unavailable from a provider are zero
- `search` writes errors and optional `--debug` diagnostics to stderr only
- `explore` always writes progress and provider usage to stderr; `--debug`
  additionally writes its human console trace and phase timings while preserving
  one strict result object on stdout
- `review` and `simplify` keep trace events in memory for SSE replay; detached
  runs persist only their durable task record
- `release-note --out <file>` and `commit` / `commit --amend` stream human
  console trace lines to stdout
- `commit` / `commit --amend` delegate commit creation to `git commit`, so Git
  config, hooks, `commit.gpgSign`, system `gpg`, and `gpg-agent` behavior apply
- if commit creation fails after message generation, the command returns nonzero
  after streaming trace events to stdout; the final error includes the
  generated commit message plus the commit failure so the user can commit
  manually

### Exit behavior

Nonzero exit codes are returned for:

- invalid CLI arguments
- missing repository context
- missing required environment configuration
- provider/API failures
- embeddings auth/config/backend failures for `search`
- trace-recording failures and context cancellation or deadlines during tool
  execution
- validation failures that cannot be repaired
- failed, unknown, malformed, corrupt, dead-producer, canceled, or wrong-command
  background waits

### Build and install

The repository provides Shadowtree recipes with:

- `shadowtree build`: run the Go profile's package-aware build
- `shadowtree test`: run `go test ./...`
- `shadowtree install`: build and install the binary to
  `<destdir><prefix>/bin/git-agent`
  and, if the fish config dir exists, install fish completions

Defaults:

- `prefix`: `$HOME/.local`
- `destdir`: empty
- `fish_config_dir`: `$XDG_CONFIG_HOME/fish`, falling back to
  `$HOME/.config/fish`
- fish completions directory: `<fish_config_dir>/completions`

## 3. Architecture

### Package map

- `cmd/git-agent`: process entrypoint
- `internal/cli`: argument parsing and command dispatch
- `internal/config`: environment and flag materialization
- `internal/agent`: bounded agent loop contract
- `internal/background`: atomic durable background task records and waiting
- `internal/openai`: official OpenAI Go SDK adapter for the Responses API and
  minimal embeddings adapter for `search`
- `internal/provider`: provider-neutral hosted-capability values and failures
- `internal/doccmd`: fixed local documentation command execution and HTML
  extraction
- `internal/guidance`: project guidance discovery and rendering
- `internal/gitctx`: typed repository inspection
- `internal/projectidentity`: shared normalized-origin or path-fallback project
  identity resolution
- `internal/skillcmd`: bounded delegation to the fixed `skills-mgr` executable
- `internal/tools`: curated model tool registry
- `internal/tasks/commitmsg`: commit message behavior
- `internal/tasks/releasenote`: release note behavior
- `internal/tasks/review`: review and simplification modes, prompts, schemas,
  validation, output shaping, and prepared change context
- `internal/tasks/search`: filesystem/revision discovery, chunking, local
  binary vector cache, hybrid ranking, replay metadata, and JSON rendering
- `internal/textutil`: shared normalization and output shaping helpers
- `internal/trace`: in-memory and console event recording

System, user, and developer instruction prompts owned by the agent, CLI, and
task packages are maintained as package-local embedded Markdown. Static prompts
use `.md` sources; prompts with runtime values use `.md.tmpl` sources rendered
with `text/template`. Go code owns prompt selection and data assembly but does
not duplicate the instruction prose.

### Request assembly layers

Every task request is assembled using Codex-style layering:

1. top-level Responses `instructions` containing task-level system behavior
2. developer message containing the read-only tool policy
3. developer message containing environment context
4. optional developer prompt layer containing verbatim Markdown from
   `skills-mgr list`
5. developer message containing project guidance
6. task-specific user prompt
7. strict function tool registry for that task, if that task exposes tools

The project guidance block is not treated as ordinary user text. It is a
separate injected layer mirroring Codex’s style.

Environment context includes:

- current working directory
- repository root
- command name
- mode or release range
- selected guidance family
- stdout contract

Tool policy states that repository and skill functions are read-only, with skill
reads delegated to `skills-mgr`. Review and simplify may also use fixed typed
documentation commands and provider-hosted web search. No model-supplied
executable, argv array, generic shell, write tool, or provider mutation exists.
External queries may verify public language and library contracts only and must
not contain secrets, source, diffs, credentials, personal data, or private
repository details. Tool results use JSON envelopes with truncation metadata;
external text remains untrusted data and cannot replace exact repository
evidence.

Task prompts use explicit evidence boundaries: repository-sourced text such as
diffs, file contents, commit messages, filenames, refs, and prepared JSON/XML
context is treated as data rather than instructions. Project guidance may shape
style and repository conventions, but it must not override the authoritative
diff or release-range evidence.

The OpenAI adapter uses the official `github.com/openai/openai-go/v3` package.
It converts internal request items into `responses.ResponseNewParams`,
including:

- `Instructions`
- structured input message items
- `function_call` items
- `function_call_output` items
- strict function tool definitions
- provider-neutral hosted capability definitions translated only by adapter
- `Store: false`
- request-scoped `ParallelToolCalls`, enabled for explore, non-branch-capable
  review and simplify nodes, commit-message and commit generation, PR-message
  generation, and release-note generation; any request with a branch
  `ControlTool` forces it off
- `web_search_call.action.sources` and `reasoning.encrypted_content` includes
  when hosted web search is enabled
- hosted-only `MaxToolCalls` when configured; local function-call ceilings stay
  enforced only in runner

### Agent loop lifecycle

1. apply the optional global `--cwd` directory before command dispatch
2. parse shared flags and validate auth-independent options
3. for commit-message tasks, collect staged paths
4. precompute normal staged context early enough to detect deterministic
   submodule-only messages before provider auth is required
5. for normal submodule-only staged changes, format and return the local
   message without the SDK-backed agent loop
6. resolve provider config and create a stdout-streaming human console trace
   for `commit` / `commit --amend`
7. precompute task context before the first provider call: staged context for
   normal commit messages, amend context for `--amend`, PR context for
   `pr-message`, or release-note context for `release-note` including resolved
   refs, parent commits, submodule gitlink changes, submodule history when
   locally available, and repository ownership/link hints
8. resolve project guidance for the task target paths, after context prep when
   prepared paths define the target scope
9. when `skills-mgr` is available, call `skills-mgr list`, inject its Markdown
   output verbatim as a developer prompt layer, then build the remaining task-specific
   instructions, developer context, and initial user prompt, appending any
   `--append-prompt` hint as lower-priority escaped prompt data
10. send a streaming request to the Responses API through the official OpenAI
   Go SDK
11. stream each request and response when console or SSE tracing is active;
    publish retry progress and repeat a retryable interrupted stream once
    without changing request semantics
12. if the model requests one or more tools, validate the complete response
    batch before execution: require every call ID and allowed name, reject
    repeated calls and repeated batch IDs, permit at most one branch control
    call, and admit the batch only when every call fits the remaining local
    budget
13. execute all admitted ordinary registered read-only calls concurrently and
    collect their results by provider position; after the batch joins, recheck
    any authoritative diff-review snapshot before emitting outputs or branching;
    return recoverable non-context execution errors as structured failed tool
    outputs so the model can correct arguments or choose other evidence, but fail
    the node on cancellation, deadline, or authoritative review snapshot drift
14. stream admitted ordinary tool calls in provider order before execution and
    stream their successful or failed outputs in provider order after the batch
    completes when tracing is active
15. append complete provider continuation output followed by one matching
    function-call-output per executed ordinary call in provider order
16. when the batch contains a branch call, append its selected result after all
    ordinary outputs and fork from that completed conversation; otherwise
    evaluate the next-request context budget and continue until final text is
    returned
17. if the local budget is exhausted, force a no-tool finalization request while
    preserving any structured text format required by the task
18. validate output against task rules
19. if invalid and repair budget remains, run exactly one repair pass
20. print final text to stdout for generation-only commands, write it to
    `--out` for `release-note --out <file>`, or stream human console trace
    lines while generating the message and then print Git's raw commit summary
    after creating or amending through `git commit`

### Subcommand execution flow graphs

#### `git-agent commit-msg`

```mermaid
flowchart TD
    Start([git-agent commit-msg]) --> Parse[Parse shared flags]
    Parse --> LocalConfig[Validate auth-independent flags]
    LocalConfig --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> StagedPaths[Collect staged paths]
    StagedPaths --> Prepare[Precompute staged commit context]
    Prepare --> SubmoduleOnly{Only submodule gitlinks?}
    SubmoduleOnly -- yes --> LocalMessage[Format local submodule message]
    LocalMessage --> Stdout
    SubmoduleOnly -- no --> Resolve[Resolve provider config from flags, env, defaults]
    Resolve --> Guidance[Resolve project guidance for staged paths]
    Guidance --> Skills[Inject skills-mgr list Markdown]
    Skills --> Registry[Register read-only commit-message tools and optional skills_read]
    Registry --> Runner[Build OpenAI runner with validator and tool specs]
    Runner --> Request[Assemble request layers]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Tool calls?}
    ToolDecision -- yes --> ToolBudget{Within tool budget?}
    ToolBudget -- yes --> ExecuteTools[Execute allowed read-only tools]
    ExecuteTools --> Continue[Append function call and output items]
    Continue --> Model
    ToolBudget -- no --> Budget[Extend interactively or force final artifact]
    Budget --> Model
    ToolDecision -- no --> Shape[Shape body wrapping]
    Shape --> Validate[Validate shaped commit message]
    Validate --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass]
    Repair --> Reshape[Shape repaired output]
    Reshape --> Revalidate[Revalidate shaped repaired output]
    Revalidate --> Preserve
    Valid -- yes --> Preserve[Preserve supported task ID suffix]
    Preserve --> FinalValidate[Validate shaped output]
    FinalValidate --> Stdout([Print artifact only to stdout])
```

#### `git-agent commit-msg --amend`

```mermaid
flowchart TD
    Start([git-agent commit-msg --amend]) --> Parse[Parse --amend and shared flags]
    Parse --> Resolve[Resolve config from flags, env, defaults]
    Resolve --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> StagedPaths[Collect staged paths]
    StagedPaths --> Prepare[Precompute amend context]
    Prepare --> Evidence[Collect original HEAD message, HEAD diff, final amended diff, staged diagnostics]
    Evidence --> Guidance[Resolve project guidance for final amended paths]
    Guidance --> Skills[Inject skills-mgr list Markdown]
    Skills --> Registry[Register read-only commit-message tools and optional skills_read]
    Registry --> Runner[Build OpenAI runner with amend validator and tool specs]
    Runner --> Request[Assemble amend request layers with prepared amend context]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Tool calls?}
    ToolDecision -- yes --> ToolBudget{Within tool budget?}
    ToolBudget -- yes --> ExecuteTools[Execute allowed read-only tools]
    ExecuteTools --> FinalDiff[Model uses prepared final diff or narrower git_final_amended_diff as authoritative evidence]
    FinalDiff --> Continue[Append function call and output items]
    Continue --> Model
    ToolBudget -- no --> Budget[Extend interactively or force final artifact]
    Budget --> Model
    ToolDecision -- no --> Shape[Shape body wrapping]
    Shape --> Validate[Validate shaped amended commit message]
    Validate --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass]
    Repair --> Reshape[Shape repaired output]
    Reshape --> Revalidate[Revalidate shaped repaired output]
    Revalidate --> Preserve
    Valid -- yes --> Preserve[Preserve supported task ID suffix]
    Preserve --> FinalValidate[Reject delta or process phrasing]
    FinalValidate --> Stdout([Print artifact only to stdout])
```

#### `git-agent commit` / `git-agent commit --amend`

```mermaid
flowchart TD
    Start([git-agent commit --optional-amend]) --> Parse[Parse --amend and shared flags]
    Parse --> LocalConfig[Validate auth-independent flags]
    LocalConfig --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> StagedPaths[Collect staged paths]
    StagedPaths --> Mode{Amend?}
    Mode -- no --> Prepare[Precompute staged commit context]
    Mode -- yes --> PrepareAmend[Precompute amend context]
    Prepare --> SubmoduleOnly{Only submodule gitlinks?}
    SubmoduleOnly -- yes --> LocalMessage[Format local submodule message]
    LocalMessage --> LocalGitCommit[Run git commit --file]
    LocalGitCommit --> Summary
    LocalGitCommit -- failure --> Manual
    SubmoduleOnly -- no --> Resolve[Resolve provider config from flags, env, defaults]
    PrepareAmend --> Resolve
    Resolve --> Guidance[Resolve project guidance for task paths]
    Guidance --> Skills[Inject skills-mgr list Markdown]
    Skills --> Trace[Create stdout-streaming console trace]
    Trace --> Registry[Register read-only commit-message tools and optional skills_read]
    Registry --> Runner[Build OpenAI runner with validator and tool specs]
    Runner --> Request[Assemble request layers]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Tool calls?}
    ToolDecision -- yes --> ExecuteTools[Execute allowed read-only tools]
    ExecuteTools --> RecordTools[Stream trace event]
    RecordTools --> Continue[Append function call and output items]
    Continue --> Model
    ToolDecision -- no --> Validate[Validate and shape commit message]
    Validate --> FinalTrace[Record final artifact]
    FinalTrace --> Commit{Amend?}
    Commit -- no --> GitCommit[Run git commit --file]
    Commit -- yes --> GitAmend[Run git commit --amend --file]
    GitCommit --> Summary[Print raw Git commit summary]
    GitAmend --> Summary
    Summary --> Done([commit complete])
    GitCommit -- failure --> ErrorTrace[Trace commit error event]
    GitAmend -- failure --> ErrorTrace
    ErrorTrace --> Manual([Return error with generated message])
```

#### `git-agent pr-message`

```mermaid
flowchart TD
    Start([git-agent pr-message]) --> Parse[Parse shared flags]
    Parse --> Resolve[Resolve config from flags, env, defaults]
    Resolve --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> Prepare[Precompute PR context for origin/HEAD..HEAD]
    Prepare --> Evidence[Collect base, changed paths, stats, branch commits, recent commits, bounded diff]
    Evidence --> Guidance[Resolve project guidance for changed paths]
    Guidance --> Skills[Inject skills-mgr list Markdown]
    Skills --> Registry[Register optional skills_read]
    Registry --> Runner[Build OpenAI runner with prepared context]
    Runner --> Request[Assemble request layers with prepared PR context]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Skill read?}
    ToolDecision -- yes --> ExecuteSkill[Execute skills-mgr get]
    ExecuteSkill --> Continue[Append function call and output items]
    Continue --> Model
    ToolDecision -- no --> Shape[Shape body wrapping]
    Shape --> Validate[Validate shaped squash commit message]
    Validate --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass without tools]
    Repair --> Reshape[Shape repaired output]
    Reshape --> Revalidate[Revalidate shaped repaired output]
    Revalidate --> FinalValidate
    Valid -- yes --> FinalValidate[Validate shaped output]
    FinalValidate --> Stdout([Print artifact only to stdout])
```

#### `git-agent release-note <base> <release>` or `git-agent release-note patch|minor|major`

```mermaid
flowchart TD
    Start([git-agent release-note args]) --> Parse[Parse shared flags, optional --out, and release range or version bump]
    Parse --> OutCheck{--out set?}
    OutCheck -- yes --> Preflight[Preflight output file writable]
    OutCheck -- no --> Resolve
    Preflight --> Resolve
    Resolve --> Floors[Raise max steps and timeout to release-note minimums]
    Floors --> Timeout[Create task timeout context]
    Timeout --> OpenRepo[Open repository]
    OpenRepo --> Guidance[Resolve project guidance for repository root]
    Guidance --> Skills[Inject skills-mgr list Markdown]
    Skills --> Trace{--out set?}
    Trace -- no --> Registry[Register repo_summary and optional skills_read]
    Trace -- yes --> StreamTrace[Create stdout-streaming console trace]
    StreamTrace --> Registry
    Registry --> Infer{Version bump shortcut?}
    Infer -- yes --> Bump[Find latest reachable semver tag and bump patch/minor/major; use HEAD for evidence]
    Infer -- no --> Prepare
    Bump --> Prepare[Precompute release-note context]
    Prepare --> Refs[Resolve base and release revision]
    Refs --> ParentLog[Collect parent repository commits]
    ParentLog --> Submodules[Inspect submodule gitlink changes]
    Submodules --> SubHistory[Collect local submodule history when available]
    SubHistory --> Runner[Build OpenAI runner with release-note validator and JSON format]
    Runner --> Request[Assemble request layers with prepared release context]
    Request --> Model[Call Responses API]
    Model --> ToolDecision{Fallback or skill read?}
    ToolDecision -- yes --> ToolBudget{Within tool budget?}
    ToolBudget -- yes --> ExecuteTool[Execute repo_summary or skills-mgr get]
    ExecuteTool --> Continue[Append function call and output items]
    Continue --> Model
    ToolBudget -- no --> Budget[Extend interactively or force final artifact]
    Budget --> Model
    ToolDecision -- no --> ValidateJSON[Validate structured release-note JSON]
    ValidateJSON --> Valid{Valid?}
    Valid -- no --> Repair[Run one repair pass]
    Repair --> Revalidate[Revalidate repaired JSON]
    Revalidate --> BuildDoc[Build Markdown document locally]
    Valid -- yes --> BuildDoc
    BuildDoc --> ValidateDoc[Validate rendered document requirements]
    ValidateDoc --> Render[Render final Markdown]
    Render --> Output{--out set?}
    Output -- no --> Stdout([Print artifact only to stdout])
    Output -- yes --> File([Write artifact to --out file])
```

### Bounded execution

The runtime must enforce:

- maximum model steps
- maximum tool calls
- maximum bytes/lines per tool result
- per-request timeout where a command default or explicit `--timeout` applies
- overall task timeout where a command default or explicit `--timeout` applies;
  `review` and `simplify` are unlimited unless the flag is set

## 4. Guidance resolution

### Goal

Follow Codex-style scoped project guidance formatting while preserving a
single-family rule:

- same-family scoped files may concatenate
- different-family files never concatenate

### Family precedence

Default family selection:

1. AGENTS-family
2. CLAUDE-family fallback if and only if no AGENTS-family guidance was found
3. no guidance if neither family is present

### Family membership

AGENTS-family candidates:

- `AGENTS.override.md`
- `AGENTS.md`

CLAUDE-family candidates:

- `CLAUDE.md`

### Scope discovery

Guidance resolution walks from repository root to the target directory in order.
For each directory in that chain:

1. choose at most one file from the active family using that family’s filename
   precedence
2. append it to the resolved source list

Example:

- `/repo/AGENTS.md`
- `/repo/frontend/AGENTS.md`
- `/repo/frontend/admin/AGENTS.md`

For a target inside `frontend/admin`, all three files are concatenated in that
order.

Example of disallowed cross-family merge:

- `/repo/AGENTS.md`
- `/repo/frontend/CLAUDE.md`

Result: choose AGENTS-family only, ignore CLAUDE-family entirely.

### Rendered format

The injected guidance block uses a Codex-style outer wrapper:

```text
# AGENTS.md instructions for /absolute/target/path

<INSTRUCTIONS>
<PROJECT_DOC path="AGENTS.md">
...
</PROJECT_DOC>

<PROJECT_DOC path="frontend/AGENTS.md">
...
</PROJECT_DOC>
</INSTRUCTIONS>
```

Notes:

- the heading remains `AGENTS.md instructions for ...` for parity with Codex’s
  visible wrapper shape
- the chosen family may still be CLAUDE-family under the hood
- inner path tags preserve provenance and scoped boundaries using
  repository-relative paths to avoid leaking absolute machine paths

### Guidance target path

Guidance must resolve against the task target path, not blindly against process
cwd.

Task defaults:

- `commit-msg`: staged paths when present in normal mode; final amended paths
  for `--amend`; if no task paths are available, current repository root
- `pr-message`: changed paths between `origin/HEAD` and `HEAD`; if no changed
  paths are available, current repository root
- `release-note`: current repository root

For `commit-msg`, guidance is resolved across all task paths. Normal mode uses
staged paths; amend mode uses the final amended paths so guidance can cover the
latest HEAD commit being amended as well as staged refinements. Family selection
remains global for the task: if any task path has AGENTS-family guidance,
AGENTS-family is selected and CLAUDE-family files are ignored for the whole
request. Sources are de-duplicated while preserving root-to-leaf order.
`pr-message` uses the same family-selection behavior, but its target paths come
from the current-branch diff against `origin/HEAD`.

## 5. Tool system

### Principles

- repository inspection tools are read-only
- skill tools are read-only
- typed tool contracts
- no arbitrary shell access
- no generic “run any git command” escape hatch
- repository paths are root-confined; walk tools skip symlinks and repository
  readers cannot follow symlinks outside the repository
- bounded output with explicit truncation markers

### Shared repository tools

Shared tools:

- `repo_summary`
- `list_files`
- `read_file`
- `inspect_file`
- `grep`
- `find`

`read_file` accepts repository-relative path, optional inclusive line range,
optional `with_line_number` output formatted like `nl -ba`, and source
`worktree`, `index`, or `head`. Source selection lets staged review
inspect index content without leaking later worktree edits. Agent policy permits
`read_file` only when its path is copied verbatim from prepared context or prior
repository-tool output; package import paths, package names, types, and symbols
do not imply filenames. Models must use available inventory or search tools to
discover unknown paths first. `grep` implements bounded RE2 matching over
repository text files with optional safe glob. `find` implements bounded
file/directory discovery by safe glob. Both are implemented in Go, do not invoke
shell commands, skip internal state directories and symlinks, and return
explicit truncation state.

`inspect_file` applies the same path, source, staged-mode, and symlink policy as
`read_file`, but returns metadata instead of content: byte and line counts,
`outline_kind`, and a bounded outline. Unsupported readable files return
`outline_kind: none` with an empty outline. Supported outlines contain Go types
and functions, Markdown headings, or JSON pointers with value kinds. Exact byte
and line counts stream across the complete file; outline parsing retains at most
the first 4 MiB, returns at most 200 entries and 64 KiB of entry data, and marks
larger results truncated. Worktree file reads reject non-regular files.

### Skill manager tools

Request assembly and registry construction resolve `skills-mgr` from `PATH`.
When it is available, message-generation commands:

- invoke `skills-mgr list` and inject its Markdown stdout verbatim as a
  developer prompt layer
- expose `skills_read`, which invokes `skills-mgr get <locator> [start:end]`

Git-agent does not discover skill roots, parse skill configuration, preload
skill metadata itself, or inject skill-use rules. Read operands pass directly
to `skills-mgr`, which owns their validation and behavior. Git-agent only
resolves and invokes the fixed executable without a shell, uses the command
working directory, captures bounded stdout and stderr, applies cancellation and
a 20-second timeout, and returns tool results in the standard envelope. When
`skills-mgr` is unavailable, the skills prompt layer and delegated tool
definition are omitted.

### Commit message tools

Commit message tools:

- `git_staged_paths`
- `git_staged_status`
- `git_staged_stat`
- `git_staged_diff`
- `git_staged_diff_for_paths`
- `git_recent_commits`
- `git_head_show`
- `git_diff_against_parent`
- `git_final_amended_diff`
- `git_amend_delta`
- `git_show_file_at_rev`

`commit-msg` and `commit` expose these tools plus available skill manager
tools. `pr-message` exposes only available skill manager tools. It precomputes
`origin/HEAD` base
metadata, changed paths, diff stats, branch commits, recent style commits, and
a bounded full diff in Go before the first provider call.

### Review and simplification tools

Both inspection commands expose shared repository tools, `jq`, and available
skill manager tools. `jq` accepts `path`, `source`,
`pointer`, `max_bytes`, and `max_lines`; it parses at most 16 MiB from the
selected repository JSON source and retrieves one value through a plain RFC
6901 JSON Pointer. An empty pointer selects the document root. It implements
object-key unescaping and canonical array indices, but not jq filter syntax or
an external executable. It preserves the selected JSON type. Values within the
requested caps are returned as `value`; larger values return a bounded
`value_preview`, exact standalone formatted size metadata, and
`truncated:true`, so the model can request a narrower pointer. Source selection,
repository confinement, symlink rejection, staged-mode isolation, and
diff-snapshot drift checks match `read_file`.

Diff modes additionally expose:

- `review_changes`
- `review_diff`
- `review_diff_for_paths`

With `--orchestration-artifact`, they additionally expose
`read_orchestration_artifact`. Tool accepts only manifest-declared artifact ID
and bounded line/byte window, revalidates file size and SHA-256 on every read,
and never accepts filesystem path. Initial prompt receives compact ID/size/digest
inventory, not artifact bodies.

These names are stable across staged and uncommitted modes; registry binds them
to selected authoritative scope. `review_changes` pages through the complete
prepared path, status, and line-stat inventory using zero-based `offset` and a
bounded `limit`, so prompt compaction never makes changed paths undiscoverable.
Before any diff-mode repository tool executes, registry verifies current
authoritative repository fingerprint still matches prepared scope. Codebase
mode does not register diff tools or apply drift checks. All repository tools
remain read-only.

They also discover executable paths once during registry construction and expose
only commands present on `PATH`:

- `go_doc {target,symbol,flags[]}` permits `all|short|src|u|c|cmd`, rejects
  option-shaped or invalid targets, runs `go doc` from repository root with
  `GOENV=off`, empty `GOFLAGS`, `GOTOOLCHAIN=local`, and `GOPROXY=off`
- `rust_doc {topic}` runs only `rustup doc --path <validated-topic>`, requires a
  regular local HTML file under installed rustup toolchain documentation, and
  returns bounded text from `#main-content`
- `context7_library {name,query}` runs only
  `ctx7 library <name> <query> --json`
- `context7_docs {library_id,query}` runs only
  `ctx7 docs <library-id> <query> --json`

Context7 JSON is parsed before envelope creation. Commands never invoke shell,
accept custom base URLs or unrelated subcommands, auto-install dependencies,
open browser, download Rust toolchains, or run `cargo doc`. Per-tool timeout is
a recoverable failed envelope; parent task cancellation is terminal. Stdout and
stderr are fully drained into bounded buffers. Final summary ends with
deduplicated material external URLs or local documentation locators and
discloses failed hosted lookup capability.

### Release note tools

Release-note generation precomputes ref resolution, parent logs, submodule
gitlink changes, submodule history, and repository ownership in Go before the
first provider call. The model receives only the `repo_summary` fallback tool
for rare metadata gaps plus available skill manager tools; legacy
range/submodule tools are intentionally not exposed to the model. `resolve_ref`,
`git_log_range`, `gitmodules_table`, `submodule_gitlink_range`,
`submodule_log_range`, and `repo_kind` remain in the registry only as deprecated
legacy tools.

### Tool I/O expectations

Each tool definition must provide:

- stable tool name
- description
- strict JSON schema for arguments using `additionalProperties: false`
- required fields for mandatory arguments
- bounds for numeric cap arguments
- JSON result envelope with stable fields
- explicit truncation metadata when output is capped

Tool result envelope:

```json
{
  "ok": true,
  "tool": "git_staged_diff",
  "data": {},
  "truncated": false
}
```

Recoverable tool execution errors use the same channel:

```json
{
  "ok": false,
  "tool": "read_file",
  "error": "openat missing.go: no such file or directory",
  "truncated": false
}
```

Failed invocations consume one tool call and are appended as
`function_call_output`, allowing the model to correct a path or arguments on a
later step. Context cancellation and deadline errors remain terminal.

The tool loop records both the model's function-call arguments and the exact
tool-output envelope sent back to the model.

### Limits

Each tool result must honor caps for:

- bytes
- lines
- entries
- nested commit/submodule log counts

The model must be told when output was truncated so it can request narrower
follow-up reads.

## 6. Task behavior

### Commit message: normal mode

Behavior:

- inspect the staged diff only
- treat staged paths as authoritative scope
- precompute staged context before generation, with changed paths, status,
  stats, recent style commits, previous HEAD paths/stats/diff for contrast,
  and a bounded staged diff
- when the bounded staged diff is truncated, precompute an additional focus
  diff for high-churn paths that were omitted or cut off, unless the change is
  handled by generated-heavy compaction/outlier rules
- compact generated-heavy staged changes with a context pack only when raw
  outlier diffs for small handwritten change clusters remain visible in the
  initial request
- use recent commit history as style reference only
- use previous HEAD paths/stats/diff only as contrast to understand what was
  already done, not as current staged scope; for large previous diffs, paths
  and stats preserve contrast shape even when the previous diff text is capped
- allow the model to request extra related file reads when the diff is
  ambiguous
- allow the model to request path-filtered staged diffs for omitted or
  high-churn clusters when the bounded full staged diff is large or truncated
- avoid tool calls that merely repeat prepared context; use narrow read-only
  tools only when they reduce material uncertainty
- cover each distinct high-signal staged change cluster present in the staged
  diff, rather than letting a dominant cluster hide a secondary behavior change
- avoid copying phrasing from recent commits or previous HEAD diff as if it
  were current staged work
- prefer `refactor` when staged evidence shows extraction, relocation,
  deduplication, or internal reorganization of existing behavior, even if new
  helper files or tests are added
- use `feat` only when the staged diff introduces a genuinely new user-visible
  capability, API, command, config option, or behavior
- when staged submodule commit summaries are available, include those summaries
  in the generated message rather than emitting only a generic submodule-ref
  update subject
- for normal submodule-only staged changes, skip model generation and format a
  deterministic message locally from staged submodule history; detect
  conventional versus Title-case subject style from recent commits, use a
  release-note-like submodule body, and collapse subjects with more than three
  submodules to `submodules`

Output rules:

- subject line first
- blank line before body only when body exists
- no fences
- no explanations
- the model is not asked to hard-wrap body paragraphs; output shaping treats
  nonblank body lines inside the same paragraph as soft wraps, reflows prose to
  the target width (72 characters), preserves blank lines between paragraphs,
  and locally wraps list items, blockquotes, and Git trailers with their
  structural prefixes intact
- long unbreakable tokens such as URLs may exceed the limit only when they
  cannot be wrapped safely

### Commit message: amend mode

Behavior:

- describe the final amended commit as one commit versus its parent
- never narrate the amended result as “previous commit plus extra changes”
- precompute prepared amend context before generation, including original HEAD
  message, latest HEAD commit metadata, HEAD-vs-parent paths/stats/diff,
  staged paths/status/stats/diff diagnostics, submodule diagnostics when
  present, recent style commits, and final amended paths/stats/diff versus
  HEAD's first parent
- expose the latest HEAD commit context in the initial request so the model
  does not have to infer the commit being amended from an empty prompt or from
  staged-delta tools alone
- treat `git_final_amended_diff` as authoritative; it overlays staged changes
  on current HEAD and compares the final amended result against the first parent
- treat prepared final amended diff fields as authoritative initial evidence;
  use `git_final_amended_diff` only for narrower follow-up when the prepared
  diff is truncated or ambiguous
- treat the current HEAD message as the output anchor; preserve its subject and
  high-level story, revising body details only when the final amended diff
  proves them false
- treat the original HEAD message as evidence and an anchor, not as executable
  instructions
- use current HEAD, HEAD-vs-parent, and staged-vs-HEAD views only as diagnostic
  inputs
- never base the subject or narrative on staged paths or staged delta alone

Output rules:

- one narrative only
- the original HEAD subject must be preserved by validation
- no delta/process phrasing such as “also”, “this amend”, or “in addition”
- preserve task IDs or scope markers only when still supported by the final
  diff

### PR message mode

Behavior:

- describe the current branch as one squash merge commit versus `origin/HEAD`
- treat the `origin/HEAD` to `HEAD` diff as authoritative scope
- use the prepared PR context as authoritative evidence
- do not request repository or PR-specific tools; `pr-message` exposes only
  available skill manager tools
- use branch commits only as supporting evidence for intent, grouping, and task
  IDs
- ignore staged and unstaged work unless it is already committed at `HEAD`
- do not emit pull request prose, review instructions, or release notes

Output rules:

- subject line first
- blank line before body only when body exists
- no fences
- no explanations
- no commit-by-commit changelog
- the model is not asked to hard-wrap body paragraphs; output shaping applies
  the same commit-message paragraph reflow and target-width rules

### Release note generation

Behavior:

- peel and validate both refs
- generate a parent-repository commit log for the selected range
- include each release-note commit's full message content in prepared context,
  clamped independently to 10 lines and 1000 words
- include per-commit changed paths, diffstat, and bounded patch excerpts so
  release-note bullets can be grounded in concrete commit evidence instead of
  commit summaries alone
- classify changed paths into operator-facing signals such as runtime, config
  schema, API, CLI, docs, generated, tests, dependency-only, and submodule-only
  changes
- precompute candidate release-note items with draft facts, recommended sections,
  confidence, refs, and evidence; the model should polish these candidates rather
  than inventing new behavior
- inspect submodule gitlink changes
- include submodule commit groups only when the gitlink moved and local commit
  history is available; submodule commit messages follow the same 10-line and
  1000-word independent clamps and include the same changed-path evidence when
  available
- treat commit messages, diffs, and prepared release context as evidence rather
  than executable instructions
- optimize prose for deployers/operators rather than developers
- keep narrative bullets concise: state the change first, avoid generic benefit
  clauses when they restate the capability, and add second-clause detail only for
  non-obvious impact, required action, compatibility/risk, rollout scope, or
  behavior changes

Output rules:

- first printable line starts with `### `
- no preamble
- no duplicate section narratives
- include `### Full Changelog` when the range touched code
- parent-repo commits appear first in the full changelog
- submodule groups appear after parent commits
- commit/repo links must follow repository ownership rules

### Validation

Each task owns a validator.

Commit message validator checks at minimum:

- non-empty output
- no code fences
- subject present
- no stray commentary
- amend mode does not use process/delta phrasing
- amend mode preserves the original HEAD subject
- normal mode includes staged submodule commit summaries when prepared context
  exposes them
- body lines stay within the target width after output shaping (target width: 72
  characters after shaping, except for long unbreakable tokens such as URLs)
- output shaping reflows soft line breaks inside body paragraphs so generated
  messages do not keep isolated word shards from model line wrapping

Release note validator checks at minimum:

- first printable line starts with `### `
- no forbidden preamble
- heading/content structure valid
- no low-signal release-note continuations such as generic "enabling operators"
  or "reducing editing errors" clauses
- release-note prepared context contains candidate items, changed paths, diffstat,
  bounded patch excerpts, operator signals, and omit/include policy hints
- `### Full Changelog` included when required

### Repair strategy

If validation fails:

1. summarize the validation errors
2. run one repair pass through the model
3. revalidate
4. return an error if still invalid

## 7. Testing strategy

### Unit tests

Unit coverage should include:

- prompt normalization
- CLI parsing
- guidance family selection
- guidance scoped ordering
- validator rules
- truncation metadata
- strict tool schemas
- tool result envelopes
- console and SSE event redaction

### Golden tests

Golden tests should cover:

- commit message prompt/context assembly
- amend prompt/context assembly
- release note prompt/context assembly
- guidance rendering blocks

### Fake API server tests

Use a local fake OpenAI-compatible server to test:

- tool-call round trips
- finish states
- validation repair pass behavior
- malformed and missing-terminal provider streams
- equivalent streaming retry wire shape, bounded progress, cancellation, and
  one-retry enforcement
- retry classification for positive, negative, unrelated-collision, and
  unknown/future transport failures
- official SDK request compatibility
- stdout-only artifact behavior

### Integration tests

Use temporary repositories to test:

- staged commit message generation scenarios
- amend scenarios
- staged-path guidance scoping
- detached HEAD
- root commit handling
- release-note tag/range handling, including patch/minor/major shortcut inference
- submodule gitlink movement and missing checkout cases

## 8. Risks and open constraints

### go-git fidelity risk

Index and diff fidelity in the read-only context helpers may not perfectly
mirror `git` CLI behavior. Commit creation itself is delegated to `git commit`,
so this risk is limited to generated context for amend and submodule-heavy
scenarios.

Mitigation:

- write integration tests around real temp repositories
- validate behavior, not raw textual parity

### Provider drift risk

“OpenAI-compatible” providers may diverge in tool-call or Responses API
details.

Mitigation:

- keep the SDK adapter thin
- isolate provider translation and SDK type conversion in `internal/openai`
- test against a fake server and at least one real provider

### Release-note formatting regressions

Release-note output has strict deployer-facing formatting constraints.

Mitigation:

- carry those constraints into validators
- lock output with golden tests

### Token growth risk

Generic file reads can inflate context quickly.

Mitigation:

- typed tools first
- strict tool output caps
- encourage narrow follow-up reads

## 9. Current acceptance criteria

The in-repository implementation is complete when:

- `shadowtree build` succeeds without writing a repository artifact
- `shadowtree test` passes
- `shadowtree install destdir=<tmp> prefix=/usr/local` installs an executable
  binary
- `git-agent commit-msg` and `git-agent commit-msg --amend` route through the
  bounded SDK-backed agent loop except for normal submodule-only staged changes,
  which are formatted locally without provider auth
- `git-agent commit` and `git-agent commit --amend` route through the same
  bounded SDK-backed commit-message loop except for normal submodule-only
  staged changes, stream human console trace lines to stdout for SDK-backed
  generation, create or amend the commit
  through `git commit`, and print Git's raw commit summary after success
- `git-agent pr-message` routes through the bounded SDK-backed agent loop,
  targets `origin/HEAD..HEAD`, and sends prepared branch context without
  exposing model tools
- `git-agent release-note [--out <file>] <base> <release>` resolves explicit refs
  before generation; `git-agent release-note [--out <file>] patch|minor|major`
  resolves the latest reachable semantic version tag and uses `HEAD` as the
  release revision
- guidance rendering uses repository-relative `<PROJECT_DOC path="...">` tags
- normal commit-message guidance resolves against staged paths, while amend
  guidance resolves against the final amended paths
- repository tools are read-only; all tools use strict function schemas
- tool outputs use the stable JSON envelope
- generation-only stdout contains only the final generated artifact, except
  `release-note --out <file>` streams human console trace lines and writes the
  artifact to the requested file
- `review` and `simplify` launchers emit one `command`/`id`/`pid`/`url` JSON
  object on stdout with empty success stderr, including follow-up launchers;
  `--wait <id>` emits only a repeatable strict final report or fails with empty
  stdout
- GC-001 and GC-002: `git-agent index gc --dry-run` with an isolated metadata
  root and no `index.remote` succeeds, reports `remote_configured=false`, and
  leaves every byte and modification time unchanged; duplicate or unknown
  arguments fail with exact usage
- GC-003 and GC-004: malformed completed local metadata fails before any store
  publishes; successful compaction preserves exact search vectors through
  catalog-resolved offsets, removes only unreachable recognized bytes, is an
  idempotent no-op on repetition, and remains readable after interruption at
  every payload/catalog publication boundary; a reader paused after catalog
  selection or payload open completes before the old generation is removed
- GC-005: a configured fixture remote removes only packs unreferenced by every
  valid current manifest, preserves all manifests and referenced vectors, and
  recomputes reachability after a concurrent non-fast-forward update before
  reporting the final pushed result
- GC-005: dry-run does not mutate the persistent sync checkout, create a commit,
  or push; normal cleanup changes only the current tree and does not rewrite
  history or prune historical objects
- GC-006: binary catalog round trips and is materially smaller than the legacy
  Gob fixture; wrong-HEAD, future-version, malformed, incomplete, reordered,
  truncated, trailing, or checksum-invalid data is rejected and reconstructed
  from validated packs
