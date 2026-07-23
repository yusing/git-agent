---
pjdoc:
  version: 1
  kind: architecture
  scope: root
  status: draft
  revision: ARCH-2
  files:
    - review-static-checks.md
---
# Git Agent planned-increment architecture

This architecture set records only cross-boundary decisions required by the
current draft increment. Shipped architecture remains documented by
`docs/spec.md` until the increment is implemented.

Current increment owners:

- `internal/checks`: checker-neutral scope, results, coordinator, and private
  helper dispatch.
- `internal/tasks/review`: provider report and final report combination.
- `internal/cli`: detached review sequencing and authoritative scope creation.
- `internal/checks/golangci`: exact bundled golangci-lint adapter.
- `internal/gitctx`: authoritative scope and drift fingerprints.
- `internal/background`: format-agnostic durable final-event storage.

- [Review static-check architecture](review-static-checks.md)
