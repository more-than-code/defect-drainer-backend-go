---
name: llm-integration
description: >
  Enforce validation, transparency, and safe defaults for LLM calls inside
  automated workflows. Activate when building or reviewing any system that
  invokes an LLM as a processing step — covers output validation, prompt
  design, cost tracking, merge discipline, and fallback behavior.
---

# LLM Integration

## Related Skills
- **audit-trail:** For provenance tracking and integrity verification of LLM-produced artifacts.
- **testing-strategy:** For testing LLM integration points and validation logic.

## Checklist

### Separation of Concerns
- [ ] Structure and existence decisions are deterministic — LLM never decides *what exists*, only *what it means*
- [ ] LLM is used for interpretation, classification, or enrichment — not for authoritative data
- [ ] Every LLM call site has a defined fallback if the model is unavailable or returns garbage

### Post-Validation
- [ ] Every LLM output passes deterministic validation before entering the system
- [ ] Validation checks structure (schema, types, required fields) AND domain constraints (value must exist in known set, reference must resolve)
- [ ] Invalid LLM output degrades to safe default or "unresolved" — never silently accepted

### Evidence and Provenance
- [ ] LLM output is never promoted to authoritative data without traceable source evidence
- [ ] Each resolved item carries: source input, model output, validation result, evidence reference
- [ ] Unresolved items are explicitly logged with reason — not silently dropped

### Transparency
- [ ] Every LLM invocation logs: model, tokens in, tokens out, latency, cost (or cost estimate)
- [ ] Aggregate token/cost telemetry available per run, per stage, per session
- [ ] Quality and accuracy are non-negotiable — transparency exists so teams make informed decisions, not to cut corners

### Prompt Design
- [ ] Prompts are precise about what role the LLM plays (classify, resolve, extract — not "figure it out")
- [ ] Prompts distinguish input from output when both appear in the source material (e.g., "which column is the lookup key" vs "which column is the result")
- [ ] Prompts include the domain vocabulary the model needs — don't assume the model shares your terminology
- [ ] Batch multiple items per call when items are independent — validate each item individually

### Merge Discipline
- [ ] LLM output is merged as union with deterministic baseline — LLM refines, never reduces
- [ ] If deterministic extraction and LLM disagree, the union is kept and the conflict is flagged — not silently resolved
- [ ] Re-running with different model versions or parameters produces auditable diff, not silent drift

## Gotchas

- LLMs return confidently wrong answers on ambiguous prompts. "Figure out the right column" → wrong. "Given this evidence, which column is the INPUT key?" → right. Precision in the prompt is the #1 quality lever.
- Batching N items in one call is efficient, but you must validate each item independently. One bad item shouldn't reject the whole batch.
- Re-running with a different model version silently changes outputs. Always log model ID and produce auditable diffs between runs.
- Token costs compound invisibly. A prompt that costs $0.02 per call costs $200 at 10K calls. Log costs per-call and aggregate per-run.

## Patterns

### Default: Validate-then-accept
```
llm_result = call_llm(prompt)
validated = validate_against_known_constraints(llm_result)
if validated:
    accept(validated, evidence=prompt_input)
else:
    mark_unresolved(reason="failed validation", raw=llm_result)
```

Never skip the validation step. LLM output merged with deterministic baseline as a union — LLM refines, never reduces.

### Transparency log entry
Each LLM call logs: `{timestamp, model, tokens_in, tokens_out, latency_ms, cost_estimate, call_site, batch_size}`

## Anti-Patterns

- **LLM as source of truth** — the model suggests; the system decides.
- **Silent acceptance** — skipping validation because "the model is usually right."
- **Suppression by LLM** — letting LLM results replace deterministic baseline. Baseline coverage must never regress.
- **Cost opacity** — running LLM calls without logging token usage.
