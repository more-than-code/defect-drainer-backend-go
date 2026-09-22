# Verification hardening — what shipped, what is left

Companion to [`workflow.md`](./workflow.md) → *Verification: who decides a fix is real*.
That doc describes how the system behaves today; this one records **why** it was
built and **what is deliberately not done**, so the next person does not have to
rediscover either.

Live status is tracked at the umbrella `tasks/todo.md`. Task files are
deliberately unversioned (backend `fe536d8` stopped tracking `tasks/`), so the
reasoning that must survive lives here instead.

## The failure that started it

`DEF-20260817-mobile-chat-composer-still-shows-the-sti-yav9` (P1) was marked
**resolved** because a PNG existed in the handoff. The agent's fix notes claimed
"6 passed" and "flutter analyze — no issues"; nothing re-ran either. Harvest
resolved on file existence alone.

Cross-checking that fix against an independent fix of the same defect found
**four** issues in one direction and **three** in the other — neither agent was
dishonest, and neither was sufficient on its own.

## Shipped

The **Where** column names the Go control plane, which is the only implementation. Rows 1–7 shipped first in the TypeScript tree (`src/jobs/*.ts`, `src/batches.ts`); that tree was archived and deleted on 2026-09-22, so read it in the `defect-drainer-backend` remote if you need the original.

| # | Change | Where |
|---|---|---|
| 1 | Verification commands re-run by DD; only DD's result counts | `internal/jobs/verify.go` (`runVerification`), Settings, Jobs panel |
| 2 | Baseline run before the agent, so pre-existing red is attributable | `internal/jobs/verify.go` (`judgeVerification`), `internal/jobs/jobs.go` |
| 3 | Acceptance criteria from the defect body rendered in BRIEF.md | `internal/jobs/brief.go` (`AcceptanceCriteria`) |
| 4 | Agent toolchain: pinned SDK cloned into the handoff | `internal/jobs/toolchain.go` |
| 5 | Job-scoped Simulator write grant | `internal/jobs/sandbox.go` |
| 6 | BRIEF names each worktree's own contract files (AGENTS.md / CLAUDE.md) | `internal/jobs/brief.go` (`contractFilesIn`) |
| 7 | Diff-hygiene measurement: how much of the diff is reformatting | `internal/jobs/diff_hygiene.go` |
| 8 | Worker role: BRIEF declares it, `PROCESS.md` is the contract, `SKILLS.md` lists per-worktree skill paths, `SKILL_FORGE_AGENT_ROLE=worker` on the child (PR-H1, Go only) | `jobs/worker_files.go`, `jobs/brief.go`, `git/spawn.go` |

Design decisions worth not re-litigating:

- **Operator-authored commands.** DD's runner is not sandboxed. Executing
  command strings written by the coding agent would hand it unsandboxed
  execution on the host. The agent is told the commands and expected to run
  them; it cannot author or edit them.
- **Unrunnable is never excused.** A command that could not run (bad repo name,
  missing worktree) blocks even when the baseline failed identically, so a typo
  cannot silently disable a check.
- **Contract files and skills are listed, not assumed.** The generated
  `SKILLS.md` prints, per worktree, the absolute path of each contract file
  that exists there plus every `SKILL.md` found under `.agents/skills`,
  `.claude/skills` and `.grok/skills` — name, one-line summary and path, never
  the body (a `|` / `>` frontmatter block folds into that one line).
  Where the repo's rules are stricter than DD's, the repo wins. Paths rather
  than copies because the agent's cwd is the job handoff, so directory-based
  discovery never fires; and per worktree rather than merged because two repos
  may vendor the same skill name at different paths. Repos without either
  (e.g. `ttd-deploy`) say so explicitly and fall back to the default set.
  Unreadable frontmatter is listed as such and never fails the job.
- **Reflow is detected per hunk, not by `git diff -w`.** `-w` compares line by
  line, so a formatter joining three lines into one still reads as three
  deletions and one addition. On the 2026-08-17 fix `-w` saw 14% of the churn;
  comparing each hunk's added and removed text with all whitespace stripped saw
  **34 of 82 hunks and 146 of 523 lines** — the actual reflow. Advisory only:
  it never blocks a resolve.
- **Repo-name aliasing.** Worktree bindings are named by app entry name on the
  entries path (`webapp`) and by git URL leaf on the `repo_urls` path
  (`ttd-webapp`). `bindingAliases()` resolves both through the app's own
  entry→url mapping — never by substring.

## Go control plane

Ported on `backend-go` `dev` (`c530772`, 2026-08-19): verification runner,
baseline/verdict, harvest gate, diff hygiene, toolchain, job-scoped sandbox,
and BRIEF.md. Live process is `defect-drainer serve`. TS `backend/` is rollback.

## Not done, and why

### No second-pass reviewer
Diffing a fix against the defect's acceptance criteria and the repo's
conventions would have caught most of the seven cross-check findings without
either agent getting smarter.

### Detector re-run gate (parked)
Tag a verify command with the defect `source` that filed it, so the check that
caught the bug must pass again before resolve. Needs a third verdict —
*not applicable* — for a source-gated check in a batch with no such defect,
and an unrecognised source must count as a failure so a typo cannot disable it.

**Parked because no detector here runs unattended.** The tutored parity harness
needs a browser and a dev server. Revive when a detector gains a headless mode,
or when one is added that already has it (lint sweep, a11y scan, source-diff
checks).

### Reporter kind: human vs agent (parked)
`reporter` is empty on all 31 defects; `source` only hints (`parity-*` vs
`console`, and `console` does not prove a human wrote it). Recording
human-vs-agent at intake would let real user-observed pain sort apart from
machine findings, which are sometimes noise — a re-review produced two
"mobile-only Pin/Rename" findings that did not reproduce on a second pass.

## Detector blind spots are defects too

While fixing the P1 above, a user-facing copy bug shipped inside a gap in the
*detector*: the tutored parity gate's i18n drift check excludes multi-line
source entries (54 of them), so a string telling users to tap a deleted button
passed every automated check. Harness gaps deserve defect records of their own,
or they keep shipping bugs no agent is equipped to see.
