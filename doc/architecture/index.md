---
pjdoc:
  version: 1
  kind: architecture
  scope: root
  status: draft
  revision: ARCH-2
  files:
    - review-static-checks.md
    - review-follow-up.md
    - branched-review.md
---
# Git Agent planned-increment architecture

This architecture set records only cross-boundary decisions required by the
current draft increment. Shipped architecture remains documented by
`docs/spec.md` until the increment is implemented.

Current increment owners:

- `internal/checks`: checker-neutral scope, results, coordinator, and private
  helper dispatch.
- `internal/tasks/review`: provider and final reports, branch policy, scoped
  instructions, model catalog, and mechanical leaf-report aggregation.
- `internal/cli`: detached review sequencing and authoritative scope creation.
- `internal/checks/golangci`: exact bundled golangci-lint adapter.
- `internal/gitctx`: authoritative scope and drift fingerprints.
- `internal/background`: durable final-event storage and minimal follow-up
  parent/mode metadata.
- `internal/agent`: terminal branch control calls and portable conversation
  forks.

- [Review static-check architecture](review-static-checks.md)

- [Review and simplify follow-up architecture](review-follow-up.md)

- [Branched review and simplify architecture](branched-review.md)
