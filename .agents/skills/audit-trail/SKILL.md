---
name: audit-trail
description: >
  Enforce integrity verification, manual correction logging, and change
  provenance tracking. Activate when agents and humans both modify the same
  artifacts, when outputs must be auditable, or when derived artifacts must
  stay in sync with their sources.
---

# Audit Trail

## Related Skills
- **llm-integration:** For validation and transparency when LLM outputs feed into auditable artifacts.
- **data-migration:** For tracking integrity during schema changes and data transformations.

## Checklist

### Integrity Verification
- [ ] Every write operation records hash before and hash after
- [ ] Integrity checks are automated — not left to manual spot-checking
- [ ] Corrupted or unexpected state is detected before downstream consumption

### Scope Control
- [ ] Before any mutation, compute the actual set of changes and compare to intended scope
- [ ] Out-of-scope changes are a hard failure — blocked, not just reported
- [ ] Scope verification is a gate in the workflow, not post-hoc telemetry

### Manual Corrections
- [ ] Manual corrections are logged with the same rigor as automated changes
- [ ] Each correction records: what changed, before/after values, reason, who, when, evidence
- [ ] Manual corrections are replayable — if the source is re-processed, the same corrections can be reapplied

### Derived Artifact Sync
- [ ] When one artifact is modified, all derived representations are updated in the same operation
- [ ] Never ship a primary artifact without verifying its derivatives are consistent
- [ ] Sync verification is automated — not dependent on the author remembering

### Provenance
- [ ] Consumers can distinguish: auto-generated vs human-corrected vs hybrid
- [ ] Each data point carries its origin: source reference, extraction method, validation status
- [ ] Provenance metadata survives through all transformations — not stripped in intermediate steps

## Gotchas

- Scope checks that only log out-of-scope changes (instead of blocking them) will ship unintended mutations. Make scope verification a hard gate, not telemetry.
- Manual corrections applied as one-off edits vanish when the source is re-processed. Store corrections as replayable patches, not direct edits.
- Intermediate processing steps that strip provenance metadata make it impossible to trace where a value came from by the time it reaches consumers.

## Patterns

### Hash-gated write
```
hash_before = hash(artifact)
apply_changes(artifact)
hash_after = hash(artifact)
log(patch_id, hash_before, hash_after, reason, changes)
```

### Manual correction log template
```json
{
  "id": "patch_001",
  "date": "YYYY-MM-DD",
  "target": "path/to/artifact",
  "changes": [
    {"field": "...", "before": "...", "after": "...", "reason": "..."}
  ],
  "evidence": "reference to source that confirms correction",
  "hash_before": "...",
  "hash_after": "..."
}
```

### Scope diff gate
Before write: compute `intended_changes` vs `actual_changes`. If `actual - intended` is non-empty, abort with report of unintended mutations.

## Anti-Patterns

- **Patch without log** — fixing data silently. Future debugging becomes impossible.
- **Primary-only updates** — patching the main artifact but forgetting derived outputs. Consumers receive inconsistent views.
- **Unreplayable corrections** — when re-processed, the same errors reappear and nobody remembers the fix.
