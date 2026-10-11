# Operating Defect Drainer

What to click, what it does, and what to do when a job stops. The concepts and the reasons
behind the gates are in [`workflow.md`](workflow.md). The HTTP contract is in the umbrella
[`docs/go-backend.md`](../../docs/go-backend.md) route table.

## Run it

```bash
# Terminal 1 — API / harness on 127.0.0.1:8788 (SQLite in backend-go/.data)
cd /Users/joe/workspace/defect-drainer/backend-go && go run ./cmd/defect-drainer serve

# Terminal 2 — console on http://127.0.0.1:5174
cd /Users/joe/workspace/defect-drainer/console && pnpm dev
```

Only one `serve` may own a data dir: it takes a lock on `{DATA}/defect-drainer.lock`, and a
second one fails to start. Restarting while a job runs has a procedure, in
[Restarting the backend](#restarting-the-backend) below.

## Before the first job: App Settings

Each app needs, per repo, a **base source** (GitHub or local checkout), a **location** (a URL or
an absolute path) and a **base branch**. Jobs cut their worktrees from that base. Also set per
app: the agent sandbox (`strict` / `workspace`), the Flutter toolchain, Simulator writes, the
turn cap, and the **verify commands** DD runs itself before and after the agent.

Settings are read when a job is **created** (repos, base) and on every **spawn** (sandbox,
toolchain, turn cap, verify commands). Resume and Re-run do not re-measure the baseline, so if
you change the toolchain, verify commands or base, start a **fresh job** rather than restarting
an old one. The reason is under [Re-run](#re-run).

## Start a fix

On **Defects**, check one defect → **Start fix**, or several → **Start batch fix**.

- A resolved defect must be reopened first.
- **manual only (no agent spawn)** creates the batch and job without an agent, for work you do
  yourself.
- Otherwise DD sets the defects to `in_progress`, creates one worktree per repo on a new
  branch `defect-drainer/<BATCH-id>`, writes the brief, runs the verify commands as the
  **baseline**, and starts the coding agent. The job opens on **Jobs**.
- When the app allows Simulator writes, jobs on this machine share one lease for the booted
  simulator. The next job waits as `queued` and starts on its own once the current job's agent
  loop ends.
- **Never have two live jobs on one defect.** Nothing stops it; two agents would fix the same
  defect and the first harvest wins. Stop or delete the old job first.

## What a job's state means

| Status | `agentRun.stoppedBy` / error | Meaning | Next |
|---|---|---|---|
| `running` | — | Agent loop, verify or harvest in progress | Wait, or **Stop** |
| `queued` | — | Waiting for the simulator lease. Another job whose app allows Simulator writes is in its agent loop; this job has not started | Wait, or **Stop** (it never spawns). It starts on its own when the lease frees |
| `completed` | `artifacts` | The agent delivered NOTES.md, the fix notes and a screenshot per defect; verify and harvest ran | Review evidence; **Create PR** / **Merge** |
| `completed` | `iterations` / `wall` (`missing` listed) | The budget ran out before the artifacts existed (8 attempts / 45 min by default) | **Resume** with a fresh budget |
| `failed` | `Grok exited code=…` | The agent crashed (a non-zero exit that was not a turn cap). Evidence was imported, nothing resolved | **Resume**, or **Re-run** if it went wrong |
| `failed` | `interrupted by backend restart` | `serve` stopped while the job ran | Kill any orphan first ([below](#after-an-unclean-restart)), then **Resume** |
| `cancelled` | `stopped by operator` | You pressed Stop | **Resume** or **Re-run** |
| `manual` | — | Created with "manual only" | Work in the worktree yourself |

A turn cap (`--max-turns`) ends one **attempt**, not the job: the loop continues the same session
until the artifacts exist or the attempt/wall cap stops it.

A defect resolves only when **this run** imported a screenshot and verification shows no
regression against the baseline. After a `completed` job that did not resolve it, the defect
stays `in_progress` even though nothing is running: Resume, or start a fresh job.

Each harvest appends a run to `job.harvests` (oldest first, the last 20 kept; Resume and Re-run do not clear them). Each defect is one entry: `resolved` (this run's screenshot was accepted), `regression` (a screenshot was imported but verification failed — read the reason, fix what it reports, then Resume or Re-run), `not_resolved` (a screenshot was imported but this run was not allowed to resolve, because the agent failed, was cancelled, or stopped short of its artifacts — Resume or Re-run), `notes_only` (a note and no screenshot; if verification blocked saving the note, the reason says so), `no_evidence` (nothing was imported — the defect stays open, so Resume or add a screenshot), or `error` (the defect could not be read, or saving a screenshot or resolving failed — the reason is the error text, and any screenshot already saved is still listed). The job drawer's Evidence tab shows each run's screenshots beside the report's. Stop, Resume, Re-run, Create PR, Refresh PR, and creating a batch return that same sanitised list (and `resumable` / `rerunnable` / `simQueue`); the batch list still leaves `harvests` off.

## Job actions

The server decides whether Resume and Re-run are allowed. When they are not, the button is
disabled and its tooltip gives the reason (see [Why is Resume disabled?](#why-is-resume-disabled)).

### Stop
Sends the agent SIGINT, then SIGKILL after 3 s. The job becomes `cancelled`, the batch
`cancelled`, and `in_progress` defects with no fix evidence go back to `open`. Stop signals the
agent process only; if a test or build it launched is still holding the worktree, see
[finding leftover processes](#after-an-unclean-restart).

### Resume
Continues the agent's **last session** (`grok -r <session id>`, named in the log), so it keeps
its reasoning and its worktree edits. Use it when the agent was on the right track and was
stopped, interrupted, crashed, or ran out of budget. It gets a fresh 8-attempt / 45-minute
budget. It needs the grok session on disk (`$GROK_HOME`, else `~/.grok`, under
`sessions/<escaped handoff path>/`).

### Re-run
Starts a **new agent session** on the same worktrees: the edits, notes and screenshots already
in the handoff stay, but the agent does not remember its reasoning. Use it when the agent went
down the wrong path.

Both Resume and Re-run:
- reuse the **first run's baseline** (measured on clean code) and diff base, so DD never
  compares the agent's edits with themselves;
- add to `agentRun` (attempts, turns, cost) instead of replacing it;
- clear the previous run's verification and diff-hygiene results;
- import only screenshots **created after the restart**. A file overwritten in place keeps its
  old inode and does not count; the agent must write a new file;
- set the batch and any reopened defects back to `in_progress`.

Because the baseline is reused, neither picks up a changed toolchain, verify command or base
branch. For that, start a fresh job.

### A fresh job
Everything new (worktree, branch, baseline, session, budget): on **Defects**, select the same
defect(s) and **Start fix** / **Start batch fix** again. Then delete the old job so nobody
restarts it by mistake; Delete also cleans its worktree out of the product repo.

### Delete
The confirm dialog first shows, per repo, what will go: any **uncommitted files** in the job's
worktree (they are discarded) and whether the job branch is **deleted** or **kept**. Delete then
kills a live agent, removes each worktree and unregisters it from its product repo, deletes the
branch `defect-drainer/<BATCH-id>` **only if it is merged** (no commits beyond the base branch
in App Settings, the repo's checked-out HEAD, or its own upstream), and removes the handoff folder. A kept branch is
named in the result message with its unmerged-commit count. Delete it yourself once you no longer
need those commits:

```bash
git -C <product repo> branch -D defect-drainer/<BATCH-id>   # discards that branch's commits
```

Delete only ever touches worktrees inside the job's own handoff folder (symlinks resolved), only
runs git in a repo where that worktree is actually registered, and only deletes branches named
`defect-drainer/…`. The job record is agent-writable, so none of these decisions trust it. A job deleted before this behaviour existed (2026-09-24) may have left a
registration behind; `git -C <product repo> worktree prune` clears it.

### Create PR / Merge / Refresh PRs
GitHub bases show **Create PR** (pushes the job branch, opens a PR with host `gh`) and
**Refresh PRs** (reads merge state). Local-checkout bases show **Merge**. If the worktree has
uncommitted files, the dialog lists them and **Commit and merge** commits them first. Create PR
can be run again after a Resume: it pushes to the same branch and reports the open PR as
`existing`. Once a PR is merged or closed, the job can no longer be restarted; file a new batch.

## Why is Resume disabled?

| Reason shown | What to do |
|---|---|
| an agent is still running for this job — stop it first | Stop, or wait: it clears once verify and harvest finish |
| no grok session found for this job | The agent never started a session, or it was removed. Use **Re-run** |
| agent finished its run — use Re-run for another pass | The run delivered its artifacts. **Re-run**, or a fresh job |
| worktree missing: `<repo>` | The worktree dir is gone or is no longer a checkout. Start a fresh job |
| PR merged/closed — file a new batch | Start a fresh job |
| not a coding-agent job | A `manual` job; there is no agent to restart |
| test run is reviewed | The run is already reviewed. Start a new run |
| test run is abandoned | The run was abandoned. Start a new run |
| test run is deleted | The run row is gone. Start a new run |

## Test-run jobs

A test run started with `execute:"agent"` is a job of `kind:"test_run"`. It has no batch and no branch. Stop, Resume, Re-run and Delete are the same buttons as a fix job, with these differences:

- **Stop** kills the agent and does not import results. The run stays `in_progress` (or `queued` if the agent had not started).
- **Resume** and **Re-run** work from `awaiting_review`: the run goes back to `in_progress`, `started_at` is now, and `finished_at` is cleared. They are refused while the run is reviewed, abandoned or deleted. They are also refused when the account password env var is unset, shorter than 6 bytes, or cannot be redacted safely (`409`, same text as create).
- **Abandon** also accepts a `queued` run whose job is not live, so a run whose job stopped or failed before `running` can be closed.
- **Delete** removes the detached worktrees and the handoff. There is no job branch to keep or delete.
- A **failed** job stays `running` while it imports whatever result files and screenshots exist, then becomes `failed`, and leaves the run `in_progress`. A job that reaches `completed` (including by hitting the iteration or wall cap) imports them and moves the run to `awaiting_review`. Screenshots from before this run's start are not imported again.
- Ship preview, Create PR and Refresh PRs answer `409` `test run jobs do not ship`.

The account password is an env var named by the account's `secret_ref` (`env:NAME`). Set that variable in the environment `serve` itself starts with, before you start it. Changing the variable requires restarting `serve`; a running process keeps the environment it was given. The value is passed to the agent only as `DD_TEST_ACCOUNT_PASSWORD`. Job logs, `agent.log`, and harvested step text replace that exact value with `[redacted]` before a line is cut at 24000 bytes. A secret split across stdout and stderr, or bytes inside a screenshot, is not redacted. Do not use a password that is a piece of the word `[redacted]`. Do not name the variable `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `PWD`, `LANG`, `TERM`, `CI`, or anything starting with `LC_`, `GROK_`, `SKILL_FORGE_`, `DEFECT_DRAINER_`, `DEFECT_CHANNEL_`, `DD_`, `GIT_`, or `GH_` — save answers `400` `secret_ref env:NAME is reserved; use a dedicated variable such as QA_PASSWORD`, and a row already saved that way fails the next spawn with the same text. Verify commands and the toolchain probe never receive the password or any other app `env:` secret. On an agent run, `started_at` is the create time until the job starts.

**Rollback precondition.** Before you run an older `defect-drainer` binary, no test job may be `queued` or `running`. An older binary does not know `kind:"test_run"` and would drive those jobs as fix jobs.

## Restarting the backend

Agent processes are children of `serve`, but they **outlive it**: stopping `serve` does not
reliably stop them, and a restarted `serve` does not know they exist. So the new server's
"is this job live?" check cannot see them, and Resume would start a second agent beside an
orphan. Always restart cleanly, or clean up after.

A job left `queued` — waiting on the simulator lease, with no agent of its own — stays
`queued` across a restart and starts on its own, oldest first. A test job waiting on that
lease is included. A job that was `running` still comes back `failed` /
`interrupted by backend restart`. A test run is left in whatever status it already had;
hydrate does not move it.

### Clean restart
1. Check for live jobs:
   ```bash
   curl -s http://127.0.0.1:8788/api/batches | python3 -c 'import json,sys; print([(j["jobId"], j["status"]) for j in json.load(sys.stdin)["jobs"] if j["status"] in ("running","queued")])'
   ```
2. **Stop** each one on Jobs, or let it finish. Wait until none shows `running`.
3. Stop `serve` (Ctrl-C). Make sure the port is free before starting again, because the new
   process cannot take the lock until the old one exits:
   ```bash
   lsof -nP -iTCP:8788 -sTCP:LISTEN   # prints nothing when free
   ```
4. Start `serve` again.

### After an unclean restart
If `serve` stopped while a job ran (crash, kill, Ctrl-C mid-job), that job comes back as
`failed` / `interrupted by backend restart`. Its batch and defects are left as they were
(`in_progress`) until you Resume, Re-run or Delete it. Before any of those:

1. Find the orphaned agent. Its command line carries the job's handoff path:
   ```bash
   pgrep -fl -- '--cwd .*/batch-jobs/<jobId>'
   ```
2. Stop it the way Stop would, SIGINT, then SIGKILL if it is still there after a few seconds:
   ```bash
   pkill -INT -f -- '--cwd .*/batch-jobs/<jobId>'
   pkill -KILL -f -- '--cwd .*/batch-jobs/<jobId>'
   ```
3. Check nothing else still holds the handoff or its worktrees (a test run or build the agent
   started):
   ```bash
   lsof -t +D /Users/joe/workspace/defect-drainer/backend-go/.data/batch-jobs/<jobId>   # PIDs; empty when clear
   ```
4. Now **Resume** (or Re-run, or Delete).

`go test` runs of this repo spawn fake agents the same way. An interrupted test run can leave
one behind under a temp dir (`pgrep -fl fake-grok`); it is safe to kill.

## Moving evidence into the data dir (one-time, 2026-09-24)

Screenshots used to live at `<umbrella>/evidence`. They now live at
`<umbrella>/backend-go/.data/evidence`, next to the database. `serve` will not move,
create, or delete them to fix the layout. Do this once, by hand:

1. Check for no running jobs (nothing `running` or `queued`):
   ```bash
   curl -s http://127.0.0.1:8788/api/batches | python3 -c 'import json,sys; print([(j["jobId"], j["status"]) for j in json.load(sys.stdin)["jobs"] if j["status"] in ("running","queued")])'
   ```
2. Stop `serve`.
3. Confirm the port is free (`lsof -nP -iTCP:8788 -sTCP:LISTEN` prints nothing).
4. If `<umbrella>/backend-go/.data/evidence` already exists, **stop**. Merge
   `<umbrella>/evidence` into it by hand. `mv` onto an existing directory nests the
   folder, which leaves the screenshots at `evidence/evidence/<DEF>/…`.
5. Otherwise: `mv <umbrella>/evidence <umbrella>/backend-go/.data/evidence`
6. Start **this** build of `serve`. A pre-change build recreates an empty
   `<umbrella>/evidence`. If that happens, remove the empty directory with `rmdir`.
   Never copy that empty directory over the tree in the data dir.

Deleting `backend-go/.data` now also deletes the screenshots.

`serve` refuses to start, and leaves the files where they are, when:

- The error prints `mv` with both paths single-quoted. `DEFECTS_ROOT` was left unset
  and `<umbrella>/evidence` is still there, while `<data>/evidence` is not. Run that
  command after the stop steps above, then start this build again.
- The error says the legacy directory is empty and prints `rmdir`. Both directories
  exist and the legacy one has no entries — an older build recreated it after the
  move. The screenshots are already in `<data>/evidence`. `rmdir` the legacy
  directory. Do not copy it over the data-dir tree.
- The error says to merge them by hand. Both directories exist and the legacy one
  still has files. Combine those files into `<data>/evidence` yourself. Do not `mv`
  onto the directory that is already there.
- The error says the evidence directory does not exist and points at this doc. A
  defect row references screenshots and `{DEFECTS_ROOT}/evidence` is not a directory.
  Put the files there (the `mv` above, when `DEFECTS_ROOT` is the data dir).
- The error says none of the referenced evidence files were found there and points
  at this doc. The directory exists, but none of the stored paths is a regular file
  inside it. Look for a nested `evidence/evidence` left by `mv` onto an existing
  directory, or a stale `DEFECTS_ROOT` that still points at an empty umbrella
  `evidence/` an older build recreated. An inventory with no evidence rows starts
  normally. A refused start does not create the directory. Saving a screenshot does,
  and a successful open of the store creates an empty `{DEFECTS_ROOT}/evidence` as well.
