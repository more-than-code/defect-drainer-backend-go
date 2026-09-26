# Agent harness — defect fixing workflow

**defect-drainer** is an **agent harness** for product defect fixing: collect bugs into a durable inventory, then run **bounded AI coding-agent** sessions to fix them in isolated git worktrees, prove the fix, and ship them — a GitHub pull request, or a merge into the local base branch.

It is **not** tied to one vendor product name, a ticket bus, or a live sprint tracker. Any coding agent that can run headless against a handoff directory (today’s default runner is pluggable; see implementation note) sits **inside** the harness.

**Operating it** (starting fixes, job states, Stop / Resume / Re-run / a fresh job, restarting the backend safely): see [operating.md](operating.md).

**Search & analytics** (multi-artifact find, defect-kind and prompt-use analytics, OpenSearch plan): see [search-and-analytics.md](../../docs/search-and-analytics.md).

---

## One loop

```
  Report          Inventory           Harness (batch job)              Ship
  ──────          ─────────           ───────────────────              ────
  screenshot  →   DEF-* record    →   worktrees + BRIEF + agent   →   GitHub: Create PR
  + notes         evidence            baseline → agent → verify        local: Merge
                  status/severity     fix_evidence required            into the saved base
                                      resolve only if verified         resolve defect
```

| Stage | Who / what | Outcome |
|-------|------------|---------|
| **1. Report** | Operator (console), script/API, or coding agent | Structured defect + report evidence. `reporter` = who filed; `source` = how detected |
| **2. Inventory** | SQLite SSOT + files under `.data/evidence/` | Triage, select defects, no code changes yet. Console shows local reported time and a gallery for evidence thumbs |
| **3. Batch fix** | Backend starts a **coding-agent job** with sandbox + worktrees | Each app repo’s Settings row picks GitHub (`origin/<branch>`) or a local checkout path + branch. Code changes only in `defect-drainer/<BATCH-…>` branches |
| **4. Prove** | Operator / agent uploads **fix evidence**, and DD re-runs the app's **verification commands** itself | Both required before resolve — see [Verification](#verification-who-decides-a-fix-is-real) |
| **5. Ship** | Host `git`, and `gh` when the repo source is GitHub | A GitHub base opens a PR onto the saved branch. A local checkout shows uncommitted files and a message; confirming commits them onto the job branch, then merges that branch into the local base. The primary tree changes only when it is already on that branch and clean |
| **6. Close** | Resolve defect with resolution + fix_evidence | Bucket → resolved |

A **test run** is the same harness pointed at a plan instead of a defect batch. `execute:"agent"` checks the product out detached at a chosen commit, hands the agent `BRIEF.md` / `PROCESS.md` / `SKILLS.md`, and harvests `RUN.md`, `results/<key>.md` and fresh screenshots. The run ends at `awaiting_review`. Nothing is committed, pushed or merged; a dirty worktree is only a warning. When the plan's account names an `env:NAME` password, that value is injected as `DD_TEST_ACCOUNT_PASSWORD` and redacted from logs and harvested step text. A value split across stdout and stderr, or bytes inside a screenshot, is not covered.

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
  untouched. The handoff is the agent's own writable cwd and grok re-reads a
  profile on every start, so the profile also denies its own `.grok` dir, and
  DD re-checks the file before every iteration (Resume's first included).

Both fail soft: a warning and the previous behaviour, never a failed job. The
one exception is a job profile that no longer holds what DD wrote, or a
`dd-simulator` defined in the user-level `sandbox.toml` (which would replace
it): the next iteration is refused and the job fails, because an agent that
widened its own sandbox must not have its work resolved.

---

## What “agent harness” means here

| Harness concern | How DD handles it |
|-----------------|-------------------|
| **Workspace isolation** | `git worktree` per product repo from that repo’s saved base (GitHub: `<remote>/<branch>`; local: the local branch). The agent does not edit the primary checkout. A later local Merge updates the primary only when it is already on that base and clean |
| **Permissions / sandbox** | OS-level sandbox profiles (`strict` / `workspace`) per app |
| **Brief / handoff** | `BRIEF.md` (role, facts, decisions, deliverables, defects) + `PROCESS.md` (worker contract) + `SKILLS.md` (per-worktree skill and contract paths) under the job handoff dir |
| **Observability** | Job log stream (harness lines vs agent lines) |
| **Stop / resume / re-run** | Console job actions; the server decides which restarts a job allows and says why not |
| **Simulator lease** | One host-wide lease for every app with Simulator writes. A job that needs the booted simulator waits `queued` until the holder's agent loop ends |
| **Deterministic ops** | Create PR or local Merge (from the repo's base source), refresh that state, resolve — host tools, not “hope the model did it” |

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

Since PR-H2 the harness *enforces* that rather than merely asking. The spawn is a
driver loop, not one shot: after each iteration DD checks the handoff for those
files and resumes the session (`grok -c` with a fixed continue prompt, never one
the model wrote) until they exist or a cap ends it — 8 iterations and 45 minutes
of agent wall by default, both overridable per deployment. Output is
`streaming-json`, so DD reads the `end` event's `stopReason`, turn count and cost
instead of guessing from an exit status.

That distinction matters because exit code 0 means two different things: "I
finished" and "I ran out of turns mid-plan". Worse, **turn exhaustion exits
non-zero** (verified on Grok Build 1.0.40: exit 1, `stopReason: cancelled`, plus a
`max_turns_reached` event), so a harness that failed the job on any non-zero exit
would give up exactly when it should resume. DD branches on the event, not the
exit code: `max_turns_reached` resumes, anything else non-zero is a real failure
that stops the loop and resolves nothing.

Stopping the loop is not permission to resolve. `agentRun.completedByArtifacts`
records whether the worker actually delivered, and harvest keeps its own gates on
top: a defect resolves only with an image imported **by this run** — prose plus a
screenshot from a previous job no longer counts — and only when verification
shows no regression. A failed or cancelled run harvests its evidence and notes so
nothing is lost, and resolves nothing.

After verification and diff hygiene, and before harvest, a fix job runs an
**independent reviewer**. It is not the implementer resumed: one fresh process
per lens, in order `spec`, `security`, `code`, `tests`, plus `ui` only when a
worktree has a file under `lib/` or `src/` whose path looks like a widget or
component. Each process gets its own cwd `handoff/review/<lens>/` (never the
implementer handoff — `grok -c` resumes the latest session *per directory*),
`--sandbox read-only`, `--no-subagents`, and no `-c` / `-r` / `--resume`.
`--always-approve` is deliberately absent. The reviewer reads a diff the
implementer wrote, so a prompt injection in that diff could steer it.
Read-only blocks writes but still reads the whole machine, and the
child-network block is a no-op on macOS, so an approved command could read
a file and send it out. The permission gate stays. A command it refuses
ends that lens with `tool-denied: <tool>` (logged at error) and blocks
resolve; that is not a generic missing payload. `REVIEW.md` tells every
lens to review by reading only (`git diff`, `git status`, `git log`,
`git show`, and file reads) and to describe an exploit in a finding
instead of reproducing it. A refused command ends the review.
`REVIEW.md` in that directory is the whole brief. The reviewer cannot write a
findings file under read-only, so DD reads the findings JSON from the
streaming-json text and ignores anything else the model printed, including a
model-supplied `ok`.

`review.ok` is true only when every lens returned a valid `dd.review.v1`
payload, none of the findings are `critical` or `high`, and the stream shows
the model actually diffed a worktree: a `run_terminal_command` tool call whose
command contains `git`, `diff`, and that worktree's absolute path (the
recorded path or its `EvalSymlinks` form), plus a `tool_call_update` for the
same id with `status: completed`. Reading files is not that evidence. Without
it the lens error is `no-read-evidence`. Medium and low findings are advisory.
A required review that is not ok still imports this run's images and notes,
and resolves nothing; the harvest outcome is `review_blocked`.

The reviewer is not spawned when the implementer failed, the job was
cancelled, or `DEFECT_DRAINER_REVIEW=0` (a warn — emergency off switch, not a
setting). A failed implementer still records the skipped review and harvests;
the job stays `running` with `error` already set until those writes finish,
and only then becomes `failed`. Seeing `failed` or `completed` means the
completion goroutine has finished writing `job.json`. It still runs when verification is `blocked` or a `regression`, and
when a cap ended the loop, as long as there is something to review. No diff
against `diffBase` does not resolve, even when fix-notes exist and even when
`DEFECT_DRAINER_REVIEW=0`: nothing resolves, with harvest reason `nothing to
review`. A placeholder note is not a change to the tree. A
missing `.git` or a git error while checking counts as a diff, so the review
is not skipped on a check that failed. An empty worktree path or a `.git`
that cannot be read then fails the manifest: the review is not ok, lenses
are skipped, and nothing resolves. A full line `UNFIXED: <id>` in `NOTES.md`
declines that defect only — `DEF-1` does not decline `DEF-10` — and harvest
will not resolve it. A missing `NOTES.md` is not a decline. A `NOTES.md` that
is present but cannot be read (over the size limit, a symlink, a hard link,
or on another device) blocks resolve for every defect in the job. Test-run jobs do not run the reviewer.
`suggested_fix` is text for a human; only App Settings `verify_commands` are
executed as shell.

Per-lens caps: `DEFECT_DRAINER_REVIEW_MAX_TURNS` (default 8) and
`DEFECT_DRAINER_REVIEW_MAX_WALL_SEC` (default 900). The wall kills the child
and that lens's error is `deadline`; the job still completes. A payload that
is missing, larger than 26 MiB, longer than 100 findings, or has any invalid
row fails that lens closed — the bad row is not dropped. Before the first lens,
and again after the last (including after a kill, Stop, or Delete while the
job still exists), DD records a manifest of each worktree and leaves the tree
as it found it. The path list is `git ls-files -z --cached --others
--exclude-standard` under the pinned `GIT_DIR`. That is the fix, and it leaves
out ignored trees such as the `node_modules` verify's `pnpm install` created.
For each path, Go `Lstat` (never following a symlink) records type, mode, size,
a SHA-256 of a regular file's bytes, the `Readlink` text of a symlink, or
missing. A path that escapes the worktree after `filepath.Clean` fails the
review. HEAD is `git rev-parse HEAD` and `git symbolic-ref -q HEAD`. Any
difference in the path set, an entry, or HEAD makes the review not ok, logs
`worktree changed during review: <repo>: <first differing path or HEAD>` at
error, and blocks resolve. A repo present in only one of the two manifests
is the same kind of change. A listing over 200k paths or 2 GiB of
regular-file bytes fails closed. Each file's size is checked before it is
opened, and a file that grows past the remaining budget during the read
fails closed the same way. Detection replaced restore because restore ran
`checkout-index -f` and `clean -fd` as the server on a path the implementer
controls, and a descendant that called `setsid` could escape the process-group
kill during that write. The implementer is treated as buggy, not malicious:
these checks catch mistakes and accidents. A deliberately hostile implementer
that achieves server RCE is a known limitation, because verify commands already
run implementer code on the server and server-side worktree git is not
sandboxed. That is a separate task.

Every review git command pins `GIT_DIR` and `GIT_WORK_TREE` to the values
recorded before the implementer ran (a linked worktree's git dir must sit under
that repo's `.git/worktrees/`), strips inherited `GIT_*` variables, and sets
`core.hooksPath=/dev/null`. A hooks directory under the handoff is never used.
`GIT_CONFIG_GLOBAL` and `GIT_CONFIG_SYSTEM` are `/dev/null` and
`GIT_CONFIG_NOSYSTEM=1`. Filter drivers are enumerated with `git config
--includes --get-regexp '^filter\.'` (local config, includes, and
`config.worktree`). The driver id is the text between `filter.` and the last
dot, so `filter.a.b.clean` is driver `a.b`. Each driver's `clean`, `smudge`,
and `process` are cleared and `required` is set false, so a worktree
`.gitattributes` cannot run one during the no-diff check or the manifest's
git. The implementer starts in its own process group. After each iteration is
reaped, and before verify and review, DD SIGKILLs that group when it is still
non-empty, so a background child is gone before those commands. If the worktree
path is no longer the directory recorded before spawn, or its `.git` file no
longer names that git dir, DD runs no git there, `review.ok` is false, and
nothing resolves. The reviewer is its own process group; a wall, Stop, or
Delete kills the group. Delete waits until the after-review compare has
finished before it removes worktrees.

**Host dependency.** `--sandbox read-only` refuses to start when
`/var/run/docker.sock` is a symlink ("endpoint is a symlink" / "Refusing to
start"). Podman's Docker-compatibility helper recreates that symlink. The lens
then fails closed with an error naming the socket, and nothing resolves.

When the loop has stopped, the operator can restart the same job two ways, both on
the same worktrees and handoff. **Resume** continues the agent's last session
(`grok -r <session>`), so its reasoning survives a Stop, a backend restart, a
crash, or a run that hit the iteration or wall cap with artifacts still missing.
**Re-run** starts a new session for when the agent went down the wrong path. Both
reuse the first run's baseline — the worktree now holds the agent's edits, so
re-measuring would compare the agent with itself. Files the session already
wrote still count toward "done", but a defect resolves only on a screenshot
created after the restart; earlier images are never imported twice. Both add to
`agentRun` instead of replacing it, each with a fresh iteration and wall budget.
Neither is allowed while an agent is live, once a PR has merged or closed, or when
a worktree is gone; Resume also needs the session on disk and a run that stopped
short of its artifacts.

---

## Surfaces

Ways to **enter** the workflow — same inventory, same schema:

| Surface | Role |
|---------|------|
| **Console** | Day-to-day report, triage, jobs, evidence |
| **HTTP API** | Scripts and future SDKs (`/api/intake`, defects, batches) |
| **Test plans / runs** | HTTP API (`/api/test-plans`, `/api/test-runs`): reusable steps and manual runs that snapshot them. Filing a defect from a failed or blocked step closes into the normal defect loop; resolving that defect marks the step for retest until a later reviewed run accepts it. Agent-executed test jobs are not built yet |
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
