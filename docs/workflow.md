# Agent harness — defect fixing workflow

**defect-drainer** is an **agent harness** for product defect fixing: collect bugs into a durable inventory, then run **bounded AI coding-agent** sessions to fix them in isolated git worktrees, prove the fix, and open PRs.

It is **not** tied to one vendor product name, a ticket bus, or a live sprint tracker. Any coding agent that can run headless against a handoff directory (today’s default runner is pluggable; see implementation note) sits **inside** the harness.

**Search & analytics** (multi-artifact find, defect-kind and prompt-use analytics, OpenSearch plan): see [search-and-analytics.md](search-and-analytics.md).

---

## One loop

```
  Report          Inventory           Harness (batch job)              Ship
  ──────          ─────────           ───────────────────              ────
  screenshot  →   DEF-* record    →   worktrees + BRIEF + agent   →   Create PR
  + notes         evidence            baseline → agent → verify        Refresh PR
                  status/severity     fix_evidence required            merge on GH
                                      resolve only if verified         resolve defect
```

| Stage | Who / what | Outcome |
|-------|------------|---------|
| **1. Report** | Operator (console), script/API, or coding agent | Structured defect + report evidence. `reporter` = who filed; `source` = how detected |
| **2. Inventory** | SQLite SSOT + files under `evidence/` | Triage, select defects, no code changes yet. Console shows local reported time and a gallery for evidence thumbs |
| **3. Batch fix** | Backend starts a **coding-agent job** with sandbox + worktrees | Each app repo’s Settings row picks GitHub (`origin/<branch>`) or a local checkout path + branch. Code changes only in `defect-drainer/<BATCH-…>` branches |
| **4. Prove** | Operator / agent uploads **fix evidence**, and DD re-runs the app's **verification commands** itself | Both required before resolve — see [Verification](#verification-who-decides-a-fix-is-real) |
| **5. Ship** | Host `git` + `gh` (Create PR / Refresh PRs) | PR lifecycle tracked on the job; merge stays on GitHub |
| **6. Close** | Resolve defect with resolution + fix_evidence | Bucket → resolved |

---

## Verification: who decides a fix is real

> Rationale and the remaining hardening items:
> [`verification-hardening.md`](./verification-hardening.md).

A fix job used to resolve a defect because an image file existed. The agent's own
claims (“6 tests passed”) were prose, and nothing re-ran them. On 2026-08-17 a P1
was resolved off widget-test renders with the project's real gate never executed.

**Verification commands** (App Settings, per repo) close that. They are
**operator-authored, never agent-authored** — DD's runner is not sandboxed, so
executing strings written by the coding agent would hand it unsandboxed
execution on the host. The agent is told what they are and is expected to run
them, but DD runs them again itself and only DD's result counts.

### Baseline → fix → verify

DD runs the same commands **twice**:

1. **Baseline** — after worktrees exist, before the agent starts.
2. **Verify** — after the agent exits.

Comparing the two makes each failure attributable:

| Baseline | After | Verdict | Blocks resolve? |
|---|---|---|---|
| pass | pass | `pass` | no |
| fail | fail | `pre-existing` | **no** — already broken, not this job |
| pass | fail | `regression` | **yes** — this job broke it |
| fail | pass | counted as *newly fixed* | no |
| — | could not run | `blocked` | **yes** — misconfiguration is never excused |

Without a baseline, long-standing red blocks every resolve and reads as damage
the agent did: `fvm flutter analyze` exits 1 on `ttd-mobileapp` purely from
pre-existing infos. A command that cannot run (bad repo name, missing worktree)
always blocks — including when the baseline failed the same way — so a typo
cannot silently disable a check.

The agent's brief lists what was already failing, so it does not chase red it
did not cause.

### Choosing commands

Two properties matter: **non-zero exit on failure**, and **runnable
unattended** (no dev server, no browser). For Tutored that means
`fvm flutter test`, `fvm flutter analyze --no-fatal-infos --no-fatal-warnings`,
`pnpm test`, `pnpm check`. Bare `flutter analyze` exits 1 on infos, which is why
the flags matter. The parity harness needs a browser, so it is not a fit.

An empty command list keeps the old behaviour (resolve on fix evidence alone),
and the console labels that per repo as “nothing enforced”.

### Acceptance criteria

Intake writes an `## Acceptance` checklist into each defect. The BRIEF now
renders it per defect as the bar, and `fix-notes/<DEF>.md` must answer each item
— explicitly saying so when one cannot be met.

### Agent toolchain and Simulator writes

Two related App Settings exist because a sandboxed agent otherwise cannot
produce real evidence:

- **Agent toolchain** — clones the repo's pinned Flutter SDK into the job
  handoff (APFS copy-on-write, ~10s) with `flutter`/`dart` shims redirecting
  `FLUTTER_ROOT`, `PUB_CACHE`, `HOME` there. The `workspace` sandbox confines
  writes and Flutter writes `$FLUTTER_ROOT/bin/cache` as it runs, so an agent
  pointed at `~/fvm` cannot run the suite at all.
- **Allow Simulator writes** — writes a job-scoped `.grok/sandbox.toml`
  granting write access to the CoreSimulator device tree, so `simctl install`
  and real screenshots work. One directory, one job; the global profile is
  untouched.

Both fail soft: a warning and the previous behaviour, never a failed job.

---

## What “agent harness” means here

| Harness concern | How DD handles it |
|-----------------|-------------------|
| **Workspace isolation** | `git worktree` per product repo from that repo’s configured base (GitHub remote-tracking ref or local committed branch); never edit primary checkouts |
| **Permissions / sandbox** | OS-level sandbox profiles (`strict` / `workspace`) per app |
| **Brief / handoff** | `BRIEF.md` (role, facts, decisions, deliverables, defects) + `PROCESS.md` (worker contract) + `SKILLS.md` (per-worktree skill and contract paths) under the job handoff dir |
| **Observability** | Job log stream (harness lines vs agent lines) |
| **Stop / re-run** | Console job actions |
| **Deterministic ops** | Create PR, refresh merge state, resolve — host tools, not “hope the model did it” |

The **coding agent is a worker inside the harness**, not the product. The harness owns inventory, gates (fix-backed-by-evidence), and git/PR actions.

That role is now explicit rather than implied: the child process carries
`SKILL_FORGE_AGENT_ROLE=worker`, `BRIEF.md` opens with a Role section, and
`PROCESS.md` records the mappings a general process file cannot know about a
headless job — the operator's Start Batch click *is* the spec approval, scope is
the listed defect ids, commit in the worktree and never push, and the worker's
ledger is `NOTES.md`, not `tasks/todo.md`. Completion is measured in files:
`NOTES.md`, `fix-notes/<id>.md` and `fix-evidence/<id>/fix-01.png`, with exact
lines `UNFIXED: <id>` to decline a defect and `NO-SCREENSHOT: <id>` where the
sandbox blocks capture. A `DONE` reply without those files is ignored.

---

## Surfaces

Ways to **enter** the workflow — same inventory, same schema:

| Surface | Role |
|---------|------|
| **Console** | Day-to-day report, triage, jobs, evidence |
| **HTTP API** | Scripts and future SDKs (`/api/intake`, defects, batches) |
| **Coding agent** | May write defects/evidence in-repo schema when asked; still uses the same inventory |

---

## Relationship to other tools

| Tool | Use for |
|------|---------|
| **defect-drainer** | Collect → batch agent fix → prove → PR → resolve |
| Product `tasks/` / issues | Implementation tracking outside the inventory if needed |
| **chat-canvas** (planned) | Shared AI chat service; may later call DD via MCP tools — does not replace this harness |

---

## Implementation note (runners)

Product language is **runner-agnostic** (“AI coding agent”, “batch fix job”).

The **current** default headless runner is configured via env/binary resolution on the host (CLI under `DEFECT_DRAINER_*` / path discovery). Swapping or multi-runner support is a harness concern, not a rebrand of the workflow.

---

## Env naming

Config uses the **`DEFECT_DRAINER_*`** prefix (e.g. `DEFECT_DRAINER_PORT`).  
Legacy `DEFECT_CHANNEL_*` is still **read** for compatibility; do not use it in new docs or scripts.
