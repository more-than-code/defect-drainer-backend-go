---
name: failure-analysis
description: >
  Analyze failure modes, unsafe intermediate states, and partial rollout risks. Activate
  for Tier 3 changes involving distributed systems, async operations, multi-step
  migrations, or any change where partial deployment creates inconsistent state.
---

# Failure Mode Analysis

## Related Skills
- **data-migration:** For multi-step data migrations.
- **deployment-strategy:** For partial rollout scenarios.
- **production-readiness:** For cross-service coordination, observability, and infrastructure.

## Checklist

- [ ] All intermediate states identified (system states during rollout)
- [ ] Each intermediate state evaluated: safe or unsafe?
- [ ] Unsafe states documented with mitigation
- [ ] Partial failure scenarios enumerated (what if step 3 of 5 fails?)
- [ ] Rollback from each intermediate state possible and documented
- [ ] Timeout/retry behavior analyzed (does retry make it worse?)
- [ ] Data consistency at each step (no orphans, no partial writes)
- [ ] Monitoring detects each failure mode

## Gotchas

- Retrying a non-idempotent operation after timeout doesn't recover — it duplicates. If you can't make it idempotent, use a deduplication key.
- Rolling back code is easy; rolling back data written by the new code is not. If the new code wrote records in a new format, the old code may not be able to read them.
- "Mixed-version safe" means old code and new code running simultaneously on different instances. If old code fails on a new column or new message format, your canary just became an outage.
- Circuit breakers that never reset cause permanent degradation. Always include a half-open state.

## Patterns

### Intermediate state analysis procedure
For each step of a multi-step change, answer:
1. What is the system state after this step?
2. Is this state valid? Can the system operate here indefinitely?
3. What happens if we stop here (crash, timeout, rollback)?
4. Can all consumers (old and new code) handle this state?

If any answer is "no" or "unsure," that step needs a mitigation before proceeding.

### Pre-mortem questions
Ask before deploying:
- "What if this ships to half the fleet and stays there for a day?"
- "What if migration fails at step 3 of 5?"
- "What if we rollback after some data has already been migrated?"
- "What if a dependency is down during deploy?"

## Anti-Patterns

- **"It won't fail"** — it will. Plan for it.
- **Non-idempotent retries** — retries duplicate data. Use dedup keys.
- **Ignoring mixed-version** — both versions will run simultaneously.
- **Testing only the end state** — test the rollout sequence, not just before and after.
