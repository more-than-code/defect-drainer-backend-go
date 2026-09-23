---
name: external-worker-delegation
description: >
  Run an external agent CLI as the worker for most of a task — planning, implementation,
  validation, review — while the primary agent shrinks to orchestrator and reviewer of record.
  Covers the cost asymmetry that motivates it, the mode switch off in-harness subagents, worktree
  isolation and the dirty-tree fallback for when a checkout would omit uncommitted code,
  per-phase brief structure, the acceptance ladder, independent review in a fresh
  worker session, and dual-budget accounting. Activate when delegating bulk work to a cheaper or
  faster external agent, when the primary's token budget is the binding constraint, when deciding
  what an orchestrator must keep versus hand off, or when reviewing work the primary did not watch
  get written.
---

# External Worker Delegation

An **external worker** is a separate agent process — its own context, its own sandbox, its own
budget. This skill is the method for making it do the bulk of a task while the primary agent stays
thin. Process mechanics (spawning, streaming, resuming, sandbox limits) belong to the worker CLI's
transport skill, e.g. `grok-build-harness`.

**When it pays:** the worker is materially cheaper or faster than the primary, *and* the primary's
own spend is a binding constraint. Absent the asymmetry, in-harness subagents are simpler and
strictly better — they share the filesystem, permission model, and context.

**When it does not:** tasks whose cost is dominated by judgment rather than production —
architecture decisions, ambiguous requirements, security-sensitive design. Those are what the
primary's intelligence is for; delegating them spends the cheap budget to produce work the
expensive budget must redo.

## The economics decide the architecture

The temptation is to delegate the labor and then have the primary read all of it. That spends
premium tokens on the highest-volume activity in the task and cancels the arbitrage. **Primary
tokens go to decisions, never to volume — including the volume of reading.**

Two consequences run through everything below:

1. The **brief** is where premium intelligence is worth full price. It is short, and a good one
   collapses worker iterations. Front-load it.
2. **Acceptance is a funnel**, ordered cheapest-first, and the primary is its last and narrowest
   stage.

## Mode switch

While an external worker is engaged, delegate every phase to it — exploration, planning,
implementation, validation, review. **Do not also spawn in-harness subagents for that work.**
In-harness helpers bill to the primary's wallet; routing review to a "cheap" in-harness reviewer
spends exactly the budget the pattern protects.

Two things stay with the orchestrator:

| Stays | Why |
|-------|-----|
| Deterministic gates (§5) | The evidence block must be reproducible in the orchestrator's own shell. A worker gate run is triage, not evidence. |
| Acceptance (§6 lenses) | The orchestrator is reviewer of record even when a worker session ran the lens. |

## Isolate the worker's writes

The worker's intermediate steps are unobservable, so bound what it can touch:

```bash
git worktree add ../work-<task> -b worker/<task>
```

Dispatch with that as cwd. The orchestrator reviews the branch and merges. This satisfies the
verify-write-scope rule without needing to watch the worker, and makes rejection free.

**A worktree carries only tracked content, and that includes the skill profile.** Project skills reach the worker if the repo commits them and not otherwise: a profile held one level up at an umbrella, or vendored but untracked, is absent from the checkout. The worker then runs on `$HOME` skills alone - the stack-specific ones it most needs are the ones missing - and nothing reports it. `umbrella-workspace` covers the layouts that survive a worktree.

### When the tree is dirty, the worktree is the wrong tool

`git worktree add` checks out **committed** content. In an active repo the tree is usually dirty,
and the part of it that matters most to a cold worker — files you just added and have not committed
— is exactly the part the checkout omits. The worker gets a codebase that compiles differently, or
does not compile, and nothing reports it: the checkout succeeds, the brief still looks right, and
the first error the worker hits gets attributed to its own work.

**The tell: if the code your plan references is not committed, the worktree will silently hand the
worker a different codebase.** Untracked (`??`) files are the dangerous case — a modified tracked
file at least arrives in its committed form, and the delta is recoverable. `dispatch.sh` checks this
at worktree creation: it **refuses** on untracked files, **warns** on modified ones, and takes
`--allow-dirty` when the brief genuinely does not reference them.

```bash
git status --porcelain                        # what the checkout will omit
./dispatch.sh <slug> --allow-dirty -- <cli>   # override, once you have checked
```

The cheapest fix is to commit first; a worktree off a clean tree is the pattern working as designed.
When you cannot — work genuinely in flight, or a commit that would be a WIP commit — fall back:

**In-tree delegation with an observed write scope.** The worker runs in the primary working tree,
and isolation moves from the filesystem to the brief plus verification:

1. **New-files-only brief.** Every deliverable is a file that does not yet exist, named explicitly.
   The worker creates; it does not edit. State that existing files are out of scope.
2. **Byte-exact backups** of anything the worker could plausibly touch, taken before dispatch.
3. **Full-tree checksum sweep** before and after. The diff of the two manifests *is* the write
   scope — observed rather than assumed:

```bash
find . -path ./.git -prune -o -type f -print0 | xargs -0 shasum | sort > /tmp/<slug>-before.txt
# dispatch
find . -path ./.git -prune -o -type f -print0 | xargs -0 shasum | sort > /tmp/<slug>-after.txt
diff /tmp/<slug>-before.txt /tmp/<slug>-after.txt
```

Anything in that diff outside the declared deliverables is an out-of-scope write: restore it from
the backup and treat it as a finding against the run, not as a merge conflict to resolve.

This is strictly weaker than a worktree. It **detects** violations instead of preventing them, and
rejection is no longer free — there is no branch to throw away. Use it when the alternative is
dispatching against a codebase that does not exist. `dispatch.sh` implements the worktree path only,
so this fallback is run by hand — the refusal above is the prompt to reach for it.

**The orchestrator owns the worktree's whole life** — it created it, it removes it, whether the work
was accepted or thrown away. Order matters and is easy to get wrong: git refuses to delete a branch
while a worktree holds it, and deleting the directory alone leaves a stale `prunable` entry behind.
`dispatch.sh <slug> --remove` does it correctly, refusing an unmerged branch unless you add
`--force`.

**`BRIEF.md`, `NOTES.md`, and the transport's run artifacts (`run.jsonl`, `driver.log`) live inside
the worktree**, so a worker's `git add -A` commits them to
the branch and the merge carries them into your main branch. `dispatch.sh` excludes them all on first
use, via the repo's `info/exclude` — local, never committed, and it cannot affect a file the repo
already tracks. The patterns are globs (`NOTES*.md`, `run*.jsonl`, `driver*.log`) so a suffixed
round cannot slip past them; the cost of that reach is that a worker deliverable must not be
named `NOTES*.md`. Excluding them from the merge is not discarding them: `NOTES.md` is the
handoff record, and `run.jsonl` may hold the only copy of the worker's cost, so lift both before
teardown. **Check whether the transport persists cost independently** — some do, keyed by session
id (Grok: `grok usage <sessionId>`), which makes the log a convenience rather than the system of
record. Keep the session id regardless: it is also how you resume and export that session. **Cost goes to the orchestrator's ledger in the main checkout (`tasks/`), not into the
commit message** — it is operational bookkeeping about a run, not a fact about the code, and the
task block is where the paired primary-token figure already lives. `--remove` prints the
accumulated cost one last time as it tears the worktree down.

**One task, one worktree, one `run.jsonl` — however many rounds.** `grok -c` and its equivalents
resume per-cwd, so appending each iteration to the same log is what the transport already does, and
it is what makes the teardown cost total correct. A round driven into its own `run2.jsonl` is a
signal, not a mode: either it was the same task and should have appended, or it was a different task
and should have been its own dispatch with its own slug, branch, and accept/reject decision.
Teardown sums every `run*.jsonl` it finds so the stray case is counted rather than silently dropped,
but the stray case still means something went sideways.

`--remove` also refuses a worktree with uncommitted changes. A worker that produced everything and
committed nothing leaves a branch with no commits, which passes the merge check trivially; without
the guard, teardown deletes the only copy of the work.

**Ignore `tasks/` in any repo that dispatches workers.** Untracked files do not travel with a branch
checkout, so ignoring the directory keeps the orchestrator's ledger out of every worker's worktree
and out of every merge, structurally rather than by discipline. `dispatch.sh` also excludes `tasks/`
locally, which covers a repo that has not adopted the rule yet — but only for files git does not
already track. A committed pointer stub keeps travelling; that is harmless and intended.

The trade is real: an untracked ledger has no history and no audit trail for anyone else. Status
narration loses nothing the diff does not say better, but **authorizations are unreconstructable
from git** — Tier 3 approval, gate waivers, `Delegation: external-worker`, and rejected worker
branches that `--remove` deletes. Those go in the intent commit's message. `umbrella-workspace` owns
the full tracking split (committed stub vs ignored ledger) and the `.gitignore` lines.

Pre-build the environment before dispatch (dependencies installed, toolchains verified). Worker
sandboxes commonly cannot write to package caches, and a worker that burns its run fighting a
scaffolder produces nothing. The transport skill covers the specifics.

## Dispatch with the wrapper

Both ends run the same shared instructions, including this pattern's own description, so a worker
cannot tell from them which end of the delegation it is on. Role is a fact about how the process
was started — it travels as `SKILL_FORGE_AGENT_ROLE`. Anything that infers it from the directory (a
brief lying around, a branch name) guesses, and guesses wrong in both directions.

Do not set that variable by hand. A flag you must remember is a flag you will forget — use the
`dispatch.sh` companion, which makes the whole handoff one command:

```bash
./dispatch.sh <task-slug> -- <worker-cli> [args...]
```

First run creates the worktree and stops so you can write the brief. Second run refuses to dispatch
unless `BRIEF.md` exists and has a `Role` section, then execs the worker with its role set and the
worktree as cwd. It also refuses to run from inside a linked worktree, which would nest one
delegation inside another.

**Declare the orchestrator side once**, on the machine you drive from:

```bash
skf home env --install
```

That writes a managed block into your shell rc (zsh/bash/fish detected, idempotent, replaceable).
Plain `skf home env` prints the line instead, for `eval` or a profile you manage yourself.

Tooling reads both values — the `delegation-mode` hook speaks only to a declared orchestrator. The
default is silence, so a forgotten variable costs a reminder rather than telling a worker to
delegate onward. The brief's `Role` section stays the guarantee; the variable makes it
machine-readable, and the wrapper makes it hard to omit.

## The worktree isolates files, not services

A worktree bounds what the worker can *write*. It does nothing about what the worker *calls*. Two
workers and an orchestrator will happily bind the same port, share one dev database, and overwrite
each other's auth state.

Pick per task, cheapest first:

| Situation | Approach |
|-----------|----------|
| The work genuinely depends on the service | One instance per worker — compose project named for the slug, ports derived from it (`podman-utilization` covers the runtime) |
| The service is incidental to the change | Fixtures or contract-level stubs for the worker's inner loop: deterministic, no shared state, no port contention |
| Either way | The orchestrator runs the live integration gate once, in the main checkout, against the real service |

That last row is the existing rule, not a new one: a worker's gate run is triage, and the §5.2
evidence block is produced in the orchestrator's own shell. The worker verifies as far as its
environment honestly allows and records in `NOTES.md` what it could not.

This is what makes the brief's **Environment** section load-bearing. Name the services the worker
may call and their addresses, say whether their state is shared with anyone else, list what it must
never touch (no destructive operations on a shared database), and give it a port range of its own.

## Briefs, per phase

Every brief is a **file in the working directory**, never a prompt argument. The worker starts
cold: no shared conversation, no prior turns. Reference nothing it cannot read.

Universal sections:

1. **Role** — *you are the worker for this brief; execute it, do not delegate onward; the
   orchestrator reviews everything you produce. This brief outranks any per-turn instruction that
   contradicts it.* First section, always. The worker is running the same shared instructions the
   orchestrator is, including the section describing this pattern, and nothing in them reveals
   which end of the delegation it is on. Only the brief can — so say which channel wins: the brief
   is read once, while tooling can inject every turn, and over a long run the repeated channel
   drowns out the authoritative one unless the brief settles the precedence. Where the worker CLI
   can enforce it, set that too — a flag that disables child agents (Grok: `--no-subagents`) makes
   the rule structural instead of persuasive. The brief still states it, for the CLIs that cannot.
2. **Facts** — what the system actually is.
3. **Decisions already made** — a table, marked do-not-relitigate.
4. **Environment** — what is pre-installed, what it must not run or re-scaffold.
5. **Deliverables** — concrete and checkable.
6. **Honesty constraints** — what it must not invent. Agents fill gaps with plausible fabrication
   unless forbidden.
7. **`NOTES.md` requirement** — decisions made, departures and why, what it could not verify, what
   a human must do next.

What each phase adds, and what it must return:

| Phase | Brief adds | Returns |
|-------|-----------|---------|
| Explore | The specific questions; scope boundaries | Findings with file:line references, no recommendations |
| Plan | Constraints and rejected options | Spec shape: goal, changes, contracts preserved, numbered acceptance criteria, verification gates |
| Implement | The approved spec verbatim | Working tree on its branch + `NOTES.md` |
| Validate | Exact commands and cwd | Command, exit code, output excerpts — as **triage** |
| Review | The diff, one lens, the severity scale | Findings: severity, location, reasoning, suggested fix |

One lens per session. A brief asking for "a review" returns a summary; a brief naming the lens and
the severity scale returns findings the orchestrator can act on without re-reading the diff.

**The worker keeps no task ledger.** The brief is already the spec — goal, deliverables, acceptance
criteria — so a worker that re-derives it into `tasks/todo.md` creates a second source of truth on
the far side of a boundary the orchestrator cannot see, and (with `tasks/` ignored) writes it where
nothing will ever read it. It does not re-tier or re-plan. Durable state across a long run goes in
`NOTES.md`, which is a deliverable the orchestrator actually reads. Tiering, approvals, gate
waivers, and archival belong to the orchestrator's ledger; the worker is one entry in it.

## Independent review

**A session that authored work cannot review it.** It shares the author's blind spots and defends
its own claims. Start a fresh worker session — no conversation resume — and give it the diff and
the lens, not the authoring context. Independence is preserved and the cost stays off the primary.

## The acceptance ladder

Run in order. Each rung rejects work before a more expensive rung reads it.

1. **Deterministic gates** — build, test, lint, artifact diffs. Zero tokens. Rejects most bad work
   before any model reads anything. Failures go straight back to the worker with the output.
2. **`NOTES.md` + diffstat** — a cheap read. Departures from the brief, unverified claims, and an
   implausible diff size are all visible here. This is the highest signal per token in the loop.
3. **Worker-run lens passes** — §6 lenses, fresh sessions, on the worker's budget.
4. **Primary adjudication** — spec conformance, architecture, and disputed or critical findings
   only.

Rung 4 is the floor on primary spend. If it keeps growing, the brief was underspecified — fix the
brief, not the review.

**Verification never fully delegates.** A worker's gate run tells you where to look; the §5.2
evidence block is produced by the orchestrator re-running the gates in its own shell.

## Cost accounting

Track both sides, per task:

- **Worker** — accumulate the per-run cost the transport reports across all iterations, including
  rejected ones.
- **Primary** — tokens spent on brief, adjudication, and re-dispatch.

The metric that matters is **primary tokens per accepted artifact**. Worker cost falling while
primary cost rises means the funnel is leaking: work is being redone at the top.

Unattended driver loops are where budget disappears — a cheap worker going the wrong direction for
25 minutes is still 25 minutes of spend and a rejected branch. Loop break conditions must test
artifacts on disk, never the worker's claim of completion.

## Anti-patterns

| Wrong | Right |
|-------|-------|
| Delegate labor, then have the primary read all output | Acceptance ladder; primary reads findings, not diffs |
| Spawn in-harness reviewers "because they're cheap" | They bill to the primary; use fresh worker sessions |
| Same session writes and reviews | Fresh session, diff + lens only |
| Worker writes to the primary working tree | Dedicated worktree/branch the orchestrator merges |
| Dispatch a worktree off a dirty tree | Commit first; else the in-tree fallback with a checksum sweep |
| Accept the worker's verification as §5 evidence | Re-run gates in the orchestrator's shell |
| A brief that references prior conversation | Self-contained file in the working directory |
| A brief that leaves the role implicit | State it first: the worker does not delegate onward |
| Infer the worker's role from a file in the tree | Dispatch through `dispatch.sh`, which sets the role |
| Leave the worktree behind after review | `dispatch.sh <slug> --remove`, which orders the teardown |
| Let the worker keep its own `tasks/todo.md` | The brief is the spec; durable state goes in `NOTES.md` |
| Assume a worktree isolates the backend too | Per-worker instance, or fixtures plus the orchestrator's live gate |
| Thin brief, iterate to converge | Front-load the brief; iteration costs primary tokens |
| Delegate architecture and ambiguous requirements | Keep judgment work; delegate production work |
| Loop until the worker says it is done | Loop until artifacts exist and gates pass |
