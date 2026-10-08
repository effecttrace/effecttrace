# Architecture decision records

Each ADR records one decision that shapes EffectTrace, with its context,
consequences and the alternatives that were considered. ADRs are immutable
once accepted; a later ADR supersedes an earlier one instead of rewriting it.

| ADR | Title | Status |
| --- | --- | --- |
| [001](001-evidence-grading.md) | Evidence grading: relationship and evidence are separate, and fixed together | Accepted |
| [002](002-direct-action-correlation.md) | Direct action correlation through the Audit-ID response header and response metadata | Accepted |
| [003](003-structural-relationships.md) | Structural relationships through controller ownerReferences matched by UID | Accepted |
| [004](004-temporal-effect-windows.md) | Temporal effect windows that close on a stable status | Accepted |
| [005](005-storage-strategy.md) | Bounded in-memory storage with recording and replay | Accepted |
| [006](006-privacy-defaults.md) | Privacy defaults: minimize at the source | Accepted |
| [007](007-otlp-export-semantics.md) | OTLP export as a new trace with a span link | Accepted |
| [008](008-no-op-requests-and-change-level-claims.md) | No-op requests and change-level claims | Accepted |

## Template

```markdown
# ADR-NNN: Title

Status: Proposed | Accepted | Superseded by ADR-MMM

## Context
## Decision
## Consequences
## Alternatives considered
```
