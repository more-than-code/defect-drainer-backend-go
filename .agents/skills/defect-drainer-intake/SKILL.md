---
name: defect-drainer-intake
description: File machine-detected findings into the Defect Drainer inventory as defects with evidence, and run the human adjudication loop over them. Covers the /api/intake contract (multipart fields, normalize modes, the async 202), bridge discipline for an automated detector (idempotency, unmodified evidence, filing without pre-filtering), and what an operator does with a queue of findings. Use when wiring a parity harness, vision review, a11y scan, lint sweep, or any other detector into Defect Drainer; when findings need a human decision before anyone writes code; or when a script posted to intake and you need to know whether anything was actually created.
---

# Defect Drainer intake

Getting machine-detected findings into a durable inventory where a **human decides what
they are**, and getting that decision back out as something a machine can check next time.

## Scope — read this before adding anything here

Defect Drainer's own repo documents the workflow it owns: report → inventory → batch fix →
prove → PR → resolve (`defect-drainer/docs/workflow.md`, `AGENTS.md`, `_template.md`). That
is discoverable from the repo and changes with the product. **Do not restate it here.**

This skill covers only the seam neither repo documents: how an automated detector's output
becomes a human queue, and what the human does with it. If something you want to add is
true of DD generally, it belongs in DD's docs.

## 1. The intake contract

`POST /api/intake` — multipart, accepts multiple files per report.

| Field | Notes |
|---|---|
| `comment` | free text. **Also the source of the title** — see §2 |
| `severity` | `P1` / `P2` / `P3` |
| `area`, `surface`, `source` | triage facets; `source` is how you find your own batch later |
| `app_id` | defects are per app; an app may own several repos |
| `repos` | comma-separated; the coding agent picks where to fix |
| `mode` | `local` / `grok` / `manual` — see below |
| files | posted unmodified, in order (`01.png`, `02.png`, …) |

`client` is **derived from the app** and should normally be left unset — operators select an
app, not a platform. Defect statuses are `open`, `triaged`, `in_progress`, `resolved`,
`wontfix`, `duplicate`.

### `mode` — the trap

| Mode | What it does |
|---|---|
| `local` | deterministic normalisation on the host. The right default for detector output. |
| `grok` | a vision agent reads the screenshots and writes the defect body. |
| `manual` | **parks the job at `waiting_external`** and waits for something outside DD to `POST /api/jobs/:id/complete`. |

`manual` does not mean "a human will look at it later." It means *"an external agent will
finish this job."* Observed 2026-08-16: 25 findings posted with `mode: manual` produced 25
jobs at `waiting_external` and **zero defects**. Every request returned `202`.

Do not send `grok` for text that is already LLM output. A vision pass over a vision
reviewer's own summary adds a second lossy hop and launders the first pass's errors into
confident prose.

### A 202 is not a filing

Intake is asynchronous: the route enqueues a job and returns `202 {job}`. The defect does
not exist yet, and may never. **Read back before claiming anything was filed:**

```bash
curl -s localhost:8788/api/defects | jq '[.defects[] | select(.source=="<your-detector>")] | length'
curl -s localhost:8788/api/jobs | jq -r '.jobs[] | "\(.status) \(.mode) \(.defectId)"' | sort | uniq -c
```

A `waiting_external` or `failed` count above zero is the failure this whole section exists
to catch. The health endpoint's `open`/`resolved` counters are the fastest sanity check.

## 2. Writing the report

**Set the title deliberately.** In `local` mode the title is the first 72 characters of
`comment` (`normalizeJob.ts`), and multipart transport rewrites `\n` as `\r\n`. A comment
that opens with a prefix line therefore yields titles like:

```
[parity-review] discover-list — copy-mismatch\r\n\r\nEnded challenge and …
```

Twenty-five of those make a list view nobody can triage from. Lead the comment with one
self-contained sentence that reads as a title on its own, and put the machinery below it.

**Say what the finding is NOT.** A detector with recall has poor precision — that is the
trade that makes it useful. The report must carry the caveat, because the operator reading
it three days later has no other way to know:

- which detector produced it, which backend/model, and when;
- how to verify it against source (a semantics dump, a route file — something specific);
- that it is **not adjudicated**;
- its baseline state, stated as text and *not acted on*.

**Evidence goes in unmodified.** Post both halves of a comparison as separate files, never
composited into one side-by-side image. A finding is unreadable without both at full
resolution, and a compositing step's bugs are indistinguishable from product bugs in the
result.

## 3. Bridge discipline

A bridge script moves findings; it does not judge them.

1. **File everything.** No quality filter, no confidence threshold, no dropping findings
   that look wrong. If the bridge decides what is worth a human's time, the human is no
   longer in the loop — they are reviewing the bridge's opinion. Map severity mechanically
   and carry the detector's own rating through verbatim so a bad mapping stays visible.
2. **Baseline state is reported, never enforced.** Already-accepted debt may still be worth
   inventorying; that is the operator's call.
3. **Idempotency on the detector's own key.** Use the same key the detector's ratchet or
   baseline uses — re-running after a re-detection must not duplicate. Keep a ledger file of
   what was sent.
4. **Claim the key in-loop, not just against the ledger.** A detector can emit two findings
   sharing one key at different severities. Consulting a ledger you only write at the end
   files both and records one, so the next run duplicates. Add the key to an in-memory set
   the moment you decide to send it.
5. **Ledger records what was SENT.** Intake is async, so there is no defect id to record
   synchronously. Do not write a ledger entry that implies more certainty than you have.
6. **Dry-run before the first real batch.** `--dry-run` printing the keys catches
   duplicate-key and missing-evidence bugs while they are free.

## 4. The adjudication loop

This is the point of the inventory. Everything above exists to make this step possible.

**Batch size is a correctness property.** Twenty-five findings handed over at once is a
rubber stamp with extra steps; the operator scrolls, the marginal call gets the same
attention as the obvious one, and a wrong accept becomes permanent baseline. Adjudicate in
the same unit the detector works in — one screen, one lens, one category at a time.

Every finding has exactly three outcomes, and each one has an artifact:

| Outcome | Artifact |
|---|---|
| **Real defect** | keep it open in DD, triaged with the repo that owns it |
| **False positive** | `wontfix` with the reason, *and* the detector's baseline updated so it stops re-reporting |
| **Accepted debt** | baseline entry with reason + date + owner, and a real task if it will be fixed |

Closing a false positive without updating the detector's baseline guarantees you adjudicate
it again next run. That is how an inventory becomes noise and gets abandoned.

### The arrow back — how autonomy is earned

**An adjudication you have made twice is a deterministic check you have not written yet.**

This is the only mechanism that shrinks the human queue without lowering the bar. A vision
reviewer reporting "the composer is misaligned" is a judgement call every single run; a
geometry check asserting two declared regions share a vertical centre is a fact, and it
never needs a human again. The first was adjudicated by eye repeatedly before it became the
second.

So when you adjudicate, ask what would have caught this without you. If the answer is a
comparison of two numbers, a presence assertion, or a schema check — that is the next thing
to build, and it retires a recurring line item from the queue permanently.

A detector graduates to running unattended **one check at a time**, when that specific check
has been right repeatedly with no false positive. Never by deciding the detector as a whole
is now trustworthy.

## 5. Related

- **`product-parity`** — the detector this was built for: capture-driven web ↔ mobile parity
  across look, geometry, tokens, copy, interaction and API. Its visual review is the only
  check in it with recall, and therefore the one whose output needs this queue.
- `defect-drainer/docs/workflow.md` — everything downstream of adjudication.
