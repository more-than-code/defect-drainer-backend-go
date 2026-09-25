# defect-drainer-go

**In-workspace path:** `backend-go/`  
**Git repo name:** `defect-drainer-backend-go`  
**Module:** `github.com/joe/defect-drainer-go`  
**Binary:** `defect-drainer`

Go control-plane of the Defect Drainer harness API. Goal: a static binary so operator and EC2 hosts do not need **Node as a runtime**. This tree is **the** API (`serve` :8788) and owns the inventory data dir. Compose `api.build` is `../backend-go`. The TS `backend/` tree it replaced was retired and deleted on 2026-09-22 — there is no `pnpm dev` rollback; its history is in the `defect-drainer-backend` remote.

Design: [`../docs/go-backend.md`](../docs/go-backend.md). Product framing: [`docs/workflow.md`](docs/workflow.md). Operator runbook: [`docs/operating.md`](docs/operating.md). Why the verification gates exist: [`docs/verification-hardening.md`](docs/verification-hardening.md).

`serve` listens after POSIX `flock` + SQLite open and serves the console HTTP contract (inventory, intake, batches, search). `healthcheck` GETs `HOST:PORT/health` and exits 0 only when HTTP 200 **and** `ok: true`. The `worker` **subcommand** is still reserved (exit 1); batch-fix spawn runs **in-process** from `serve`.

## Run

```bash
cd /Users/joe/workspace/defect-drainer/backend-go
go test ./...
go run ./cmd/defect-drainer serve
# → http://127.0.0.1:8788
# DEFECTS_ROOT defaults to this module's .data (same directory as the DB) when unset.
# DEFECT_DRAINER_DATA = this module's .data when that DB exists.
```

Or `make serve` / `make test` / `make build` (`CGO_ENABLED=0`). Cross-compile: `make cross-linux`.

## One-writer rule

The inventory is **one SQLite file** plus `evidence/` inside that data dir (`.data/evidence`). Only one backend process (TS **or** Go) may write a given data dir.

- Mutex: both TS (`koffi` → `libc.flock`) and Go (`syscall.Flock`) take `LOCK_EX|LOCK_NB` on `{DATA}/defect-drainer.lock`. A second locker fails immediately and does not listen.
- Operator backup: never start two servers against one data dir.

## Fail-closed paths (`serve`, not `healthcheck`)

| Env | Role |
|-----|------|
| `DEFECTS_ROOT` | Directory that contains `evidence/` (defaults to the data dir; no `DEFECT_DRAINER_` prefix) |
| `DEFECT_DRAINER_DATA` | Runtime: `defect-drainer.db`, `jobs/`, `batch-jobs/`, `clones/` |
| `DEFECT_DRAINER_HOST` / `PORT` | Listen (default `127.0.0.1:8788`) |

Legacy `DEFECT_CHANNEL_*` is still read. `.env` is only `backend-go/.env`, loaded in `main()`, never overriding process env.

If those roots cannot be resolved (PATH-installed binary, no `go.mod` in cwd, no existing `backend-go/.data/defect-drainer.db`), the process **exits 1**. It will not mkdir a second inventory.

From `backend-go/` in this umbrella, `serve` already picks that SSOT: both roots default to this module's `.data`. Override only when the binary cannot see the tree:

```bash
export DEFECT_DRAINER_DATA=/Users/joe/workspace/defect-drainer/backend-go/.data
export DEFECTS_ROOT=/Users/joe/workspace/defect-drainer/backend-go/.data
```

`healthcheck` uses **HOST/PORT only** (default `127.0.0.1:8788`). It does not walk `go.mod`, flock, or open SQLite.

## Batch-fix (in `serve`, after worktrees)

`POST /api/batches` with `start_fix: true` and `mode: grok` follows the TS path:

1. Flip selected `open` / `triaged` defects to `in_progress`.
2. Create `git worktree`s (`defect-drainer/<BATCH-id>`) from each repo's saved base. A local checkout starts at that local branch. A GitHub source starts at `<base_remote>/<base_branch>` (the local branch only when the remote ref is absent). An empty per-repo branch uses the app-level `base_branch`, then `main`. A missing base fails the batch — it does not fall back to `HEAD`. No local/repo URLs → **400** and `SyncBatchAndDefects(..., failed)` (defects reopen).
3. Persist bindings on `job.worktrees`. Empty worktrees refuse spawn.
4. Write `BRIEF.md` (worker role, decisions, acceptance criteria, defects), the DD-owned `PROCESS.md` worker contract, the generated `SKILLS.md` (per-worktree contract files and `SKILL.md` entries from `.agents/skills` / `.claude/skills` / `.grok/skills` — name, one-line summary and path, never bodies), and the batch manifest. Fail-closed 400s still write all three so the handoff explains why spawn did not start.
5. If `agent_toolchain=flutter`, clone the pinned SDK into `<handoff>/.tooling` (`cp -Rc`; skip + warn on missing/conflicting pins). If `allow_simulator_writes`, write a job-scoped `<handoff>/.grok/sandbox.toml` (`dd-simulator`) — never `~/.grok`. The handoff is the agent's writable cwd and grok re-reads the profile on every start (`-c` / `-r` included), so the profile `deny`s its own `.grok` dir, `.grok` is replaced rather than written through (a planted symlink is never followed), and before **every** iteration DD checks the file still holds exactly the bytes it wrote and that `$GROK_HOME/sandbox.toml` (default `~/.grok`) does not mention `dd-simulator` (a user-level profile of that name replaces the job's, deny included). Either check failing refuses the start and fails the job; nothing resolves. Record each worktree `HEAD`. If `verify_commands` is set, run them as **baseline** into `<handoff>/baseline` and persist `job.baseline`.
6. `exec` the coding-agent bin (`GROK_BUILD_BIN` → `DEFECT_DRAINER_GROK_BIN` → `SKETCH_FORGE_GROK_BIN` → Homebrew/`PATH` `grok`) with `--sandbox` from the app `grok_sandbox` (`workspace` or `strict`), or `dd-simulator` when a job profile was written. `--max-turns` comes from the app's `agent_max_turns`, else `DEFECT_DRAINER_GROK_BATCH_MAX_TURNS` / `GROK_BATCH_MAX_TURNS`, else `80`; the stored value is clamped to 1..1000 so a settings typo cannot buy an unbounded run. Output format is `streaming-json`, so DD reads the `end` event (`stopReason`, `num_turns`, `total_cost_usd`, `sessionId`) instead of inferring from exit status. Text deltas are joined into lines at tool calls and other events. A line that starts with `{` but is not valid JSON is logged as `invalid agent event (N bytes; see agent.log)`. Prompt includes toolchain notes, VERIFICATION commands, and already-failing baseline rows. Iterations 2+ re-exec with `-c` and a code-constant continue prompt. When the app has `allow_simulator_writes`, that spawn takes the one host simulator lease (every such app shares the booted simulator). If the lease is held, the job is persisted as `queued` with `queuedAt` and `queuedMode` (`fresh`, `rerun`, or `resume`) and the call returns success; the oldest still-queued job starts in that mode when the holder's agent loop ends, or sooner if the holder never launches. Stop and Delete dequeue a waiting job. A restart keeps `queued` jobs and starts the oldest.
7. The driver loop repeats step 6 until `artifactsComplete` (`NOTES.md`, plus `fix-notes/<id>.md` and a harvestable `fix-evidence/<id>/` image per defect, or an exact `UNFIXED:` / `NO-SCREENSHOT:` line) or a cap ends it; `job.agentRun` records iterations, `stoppedBy`, turns and cost after **each** iteration. Then on **no genuine failure**: re-run `verify_commands` into the handoff (`job.verification`, `verify.json`), measure diff hygiene vs the recorded heads (`job.diffHygiene`; advisory only), `judgeVerification` (only `regression` and `blocked` stop a resolve), then harvest with resolve permitted. On **failure** (non-zero exit that is not turn exhaustion): harvest with no verification and `AllowResolve` false — evidence and notes are imported, nothing resolves. On **cancel**: existing stop path. The first spawn persists each worktree head as `job.diffBase`.
8. Harvest `fix-evidence/` + `fix-notes/` (regular files under the handoff only). Evidence may import even when the verdict is not ok; resolve additionally requires an image imported by **this run** (historical `fix_evidence` on the defect is not proof this job fixed anything) and a run permitted to resolve. Harvest also appends `job.harvests` (oldest first, the last 20 runs): each defect's outcome and the images this run imported. `GET /api/batch-jobs/{id}` returns that list sanitised, and so do the job bodies from stop, resume, rerun, create-prs, refresh-prs, and batch create. `GET /api/batches` omits it. Then `SyncBatchAndDefects`: `complete` leaves harvested defects; `failed`/`cancelled` reopens `in_progress` rows with no `fix_evidence`.
9. `GET /api/batch-jobs/{id}/ship-preview` lists uncommitted, non-ignored files on each local worktree. The suggested commit message uses the defect title as the subject and the agent's fix summary (`fix-notes` **Fix:**, otherwise the NOTES.md "What was done" bullets) as the body. The file list stays in the dialog, not in the message. `POST /api/batch-jobs/{id}/create-prs` (`{"commit":true,"message":"…"}` only after the operator confirms that list; an empty body does not commit) and `refresh-prs` ship each worktree against the base stored on it. `baseSource=origin` runs host `gh` (`GH_BIN` overrides) and opens a pull request onto `baseBranch`. `baseSource=local` commits those files onto the job branch when `commit` is set, then merges that branch into the local base and does not call `gh` or push. The primary checkout is updated only when it is already on that branch and clean; a dirty checkout is refused; any other checkout is left in place and the merge runs in a temporary worktree. Rows land on `job.prs[]` (`kind` `pr` or `merge`, `status`, `url`, `ghNumber`, `ghState`, `mergedAt`, `checkedAt`, `branch`, `base`, `error`). Skip when there are no commits vs that base. Jobs saved before `baseSource` existed still use `origin/main`.
10. `GET /api/batch-jobs/{id}/diff/{repo}` returns hunks for the console drill-down (`kind=reflow|all`). 404 unknown job / no worktree; 409 no recorded `baseSha`; 410 worktree gone.
11. `DELETE /api/batch-jobs/{id}` kills a live agent, then `git.DiscardWorktrees`: each worktree inside the handoff is removed and unregistered (`git worktree remove --force`, then `prune`), and its `defect-drainer/*` branch is deleted only when it has no commits beyond its base, the primary's HEAD or its upstream (re-checked at delete time). Because `job.json` is agent-writable, the base comes from the app's saved settings (`trustedBases`), not the binding; git runs only in a repo where the worktree is registered; and worktree paths must resolve (symlinks followed) strictly inside the handoff. Then the handoff goes. The per-repo report is the response's `cleanup[]`; `GET …/delete-preview` returns the same plan read-only, including uncommitted files.

**Restart** (`POST /api/batch-jobs/{id}/resume` or `/rerun`) reruns steps 4–8 on the existing worktrees. `Restart` checks and flips the job to `running` under one lock, so an overlapping request gets **409** rather than a second agent on the same handoff — including while a Stopped run's driver goroutine is still in verify or harvest; the batch returns to `in_progress` and reopened defects to `in_progress`. Resume's iteration 1 passes `-r <session>` with the continue prompt — the newest `<grok home>/sessions/<url-escaped handoff>/<uuid>/` (`$GROK_HOME`, else `~/.grok` — grok moves its session store with it) holding a `chat_history.jsonl`; none found blocks Resume. Re-run starts a new session. Both skip step 5's baseline and reuse `job.baseline` (none stored → verify judges without one, fail-closed), measure hygiene from `job.diffBase`, clear the previous `verification` / `diffHygiene`, stamp `job.runStartedAt`, and seed `agentRun` so iterations, turns and cost accumulate (`agentRun.resumes` counts Resumes). Caps are per restart. Two cutoffs apply after a restart: the loop's completion check still counts artifacts from `createdAt` on (what a resumed session already wrote stays done), but harvest imports only images whose inode was born after `runStartedAt`, so earlier screenshots are never re-imported and resolve needs an image this run produced (a file overwritten in place keeps its old inode and does not count). Notes still read from `createdAt`, since they only set resolution text. The rules (`resumeCheck` / `rerunCheck`, `internal/jobs/restart.go`) are exposed on job reads as `resumable` / `rerunnable`: both need grok mode, no live agent, every worktree present, and no merged/closed PR; Resume additionally needs a `failed` or `cancelled` job, or a `completed` one that stopped on `iterations` / `wall`, plus a session on disk.

`GET /api/batches` returns `{ batches, jobs }`. Job JSON writes `jobId` (still reads legacy `id`) and first-class `verification` / `baseline` / `diffHygiene` / `agentRun` (unknown keys still round-trip via Extra).

The spawned child carries `SKILL_FORGE_AGENT_ROLE=worker`; `serve` itself never sets it, so a human session started from the same shell is not marked as a worker.

Exhausting `--max-turns` ends one **iteration**, not the job. PR-H2's driver loop landed
alongside this setting: the agent exits 1 with `max turns reached`, and because that is turn
exhaustion rather than a genuine failure DD resumes the session (`grok -c`) instead of
failing the job — up to `DEFECT_DRAINER_AGENT_MAX_ITERS` (8) and
`DEFECT_DRAINER_AGENT_MAX_WALL_SEC` (2700). Branching on the `max_turns_reached` event
rather than the exit code is what separates the two, since both exit non-zero.

So `agent_max_turns` bounds how much each iteration attempts, and the loop caps bound the
job. Raise it for repos whose tool calls are slow — a Flutter `pub get` / `analyze` / `test`
cycle burns turns far faster than a Node one — to spend fewer iterations on the same work.

What has not changed: exhausting the *loop* caps is not a partial success. A defect with no
new fix evidence from this run stays open, work the agent left uncommitted reaches nothing
(`gh` skips a PR with no commits ahead of base, and harvest will not resolve), and the job
records `agentRun.completedByArtifacts: false` so the shortfall is visible rather than
inferred.

The verification runner is **not sandboxed**. Commands come only from App Settings (`verify_commands`), never from the agent. Their PATH is the host pin (`$FVM_HOME/versions/<pin>/bin`), not the agent-writable handoff clone. Verify commands and the Flutter toolchain probe do not inherit `DD_TEST_ACCOUNT_PASSWORD` or any app `env:` secret name.

`start_fix: true` + `manual` stays planned / job `manual` and does **not** flip defects.

### Test-run jobs

`POST /api/test-runs` with `execute:"agent"` starts a job with `kind:"test_run"` and `testRunId` set. There is no `batches` row; `batchId` stays empty. The handoff is still `batch-jobs/<job>/`. Worktrees are detached (`git worktree add --detach`) at the resolved commit under `worktrees/<repo>`. No branch is created. Each resolved 40-character SHA is written onto the run's `refs` before the agent starts. A repo with no ref uses the same base a fix job would (`ResolveJobBase`, including the origin fetch).

`BRIEF.md` tells the agent it is a test executor: run id, title, app, plan id and version, environment keys (never `secret_ref`), the account label and username only, and either `the password is in $DD_TEST_ACCOUNT_PASSWORD` or `no password is available to you; mark steps that need one blocked`. Steps are `position · key · section · action · expected`. `SKILLS.md` is the same path listing as a fix job. `PROCESS.md` is the test worker contract. The continue prompt is a code constant naming `RUN.md`, `results/<key>.md`, `results/x-<slug>.md` and `evidence/<key>/<file>`.

The loop stops when `RUN.md` is fresh and every snapshot step has a fresh valid result file or a `NOT-RUN: <key>` line. A run with no steps is complete once `RUN.md` exists. There is no baseline, no verification section and no verify commands. On a non-failure stop (including the iteration and wall caps) harvest imports results and fresh screenshots, then the store's finish transition moves the run to `awaiting_review`. A `pass` with no image imported by that harvest becomes `unverified`. Screenshot import uses the start of this run (`runStartedAt`, else the job's create time), so a Re-run does not import an earlier run's screenshots and a `pass` backed only by an old image is `unverified`. A failed job stays `running` while it harvests, then becomes `failed`, and leaves the run `in_progress`. Stop does not harvest; the run stays `in_progress`. `ship-preview`, `create-prs` and `refresh-prs` return **409** `test run jobs do not ship`. Diff hygiene is still measured; a dirty worktree logs `worktree changes are never shipped from a test job`. Resume and Re-run are allowed from `awaiting_review`: the run returns to `in_progress`, `started_at` is now, and `finished_at` is cleared.

When the account `secret_ref` is `env:NAME`, the value is read from that host variable at every spawn and passed only as `DD_TEST_ACCOUNT_PASSWORD`. It is never stored, returned or logged. Job logs, `agent.log`, and harvested `actual`, `action`, `expected`, and `NOT-RUN` text replace the exact value with `[redacted]` before newline normalization and before the 24000-byte log cut (the forwarder holds back `len(secret)-1` bytes). A secret split across stdout and stderr, or secret bytes inside an image, is not redacted. A value that is a substring of `[redacted]`, or that starts with a non-empty suffix of it, is refused: `test account secret env NAME cannot be redacted safely`. Every agent spawn, verify command, and toolchain probe drops an inherited `DD_TEST_ACCOUNT_PASSWORD` and every variable named by any app's `env:` `secret_ref`. If that app list cannot be read, the spawn fails. After the child environment is built, those names are dropped again, except the test job's own `DD_TEST_ACCOUNT_PASSWORD`. Names `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `PWD`, `LANG`, `TERM`, and `CI`, and names starting with `LC_`, `GROK_`, `SKILL_FORGE_`, `DEFECT_DRAINER_`, `DEFECT_CHANNEL_`, `DD_`, `GIT_`, or `GH_`, are reserved.

## Test plans and runs (API)

Per-app **test plans** are ordered steps (`action`, `expected`). A **test run** copies those steps when it is created (or starts with none) and records a result on each step. The copy is independent of the plan: later edits do not change the run. The run stores `plan_version`, the plan's `version` at snapshot time. `version` starts at 1 and increments when title, purpose, env, or steps (keys, order, section, action, expected) change. Changing only `archived`, including un-archiving by re-posting the same content, leaves `version` as it is. Re-posting an archived plan's `(app_id, key)` un-archives it.

Step screenshots live at `{DEFECTS_ROOT}/evidence/<TRUN-id>/s-<stepKey>-NN.ext` and are served by `GET /evidence/{id}/{file}`. A plan `key` and a step `key` match `^[a-z0-9][a-z0-9._-]{0,63}$`; a key containing `..` is rejected so a screenshot name cannot leave that directory.

| Method | Path | Behaviour |
|---|---|---|
| `GET` | `/api/test-plans?app_id=&include_archived=1` | Newest `updated_at` first. Archived plans are omitted unless `include_archived=1`. List rows include `step_count` and omit `steps`. |
| `POST` | `/api/test-plans` | Upsert on `(app_id, key)`. **201** `{plan, created:true, changed:true}` when new. **200** `{plan, created:false, changed}` when the key already exists. |
| `GET` | `/api/test-plans/{id}` | Plan and steps. |
| `GET` | `/api/test-plans/{id}/grid?runs=N` | Current steps against the last N **reviewed** runs of the plan, newest `started_at` first. `N` defaults to 10 and must be 1..50. |
| `PATCH` | `/api/test-plans/{id}` | Any of `title`, `purpose`, `env`, `steps` (full replace), `archived`. |
| `DELETE` | `/api/test-plans/{id}` | **200** `{archived:true}` when a run still references it, otherwise `{deleted:true}`. |
| `GET` | `/api/test-runs?app_id=&plan_id=` | Newest `created_at` first. List rows keep `progress` and omit `steps`. |
| `POST` | `/api/test-runs` | `execute: "manual"` (default path) or `execute: "agent"`. Manual: `executor` is `operator` (default) or `agent-session`; `executor=dd-job` is **400** `executor=dd-job is not available yet`. Status starts `in_progress` and `job_id` is null. Agent: **201** `{run, job}`. The run is `executor:"dd-job"`, `executed_by` defaults to `dd-job`, status `queued` until the spawn is `running` (then `in_progress`), and `job_id` is set. Resolved commit SHAs are written onto `refs` (a repo with no ref records the base branch). Optional `plan_id` must belong to the app and must not be archived. Optional `account_label` and `refs`. An `env:NAME` password that is unset is **400** `test account secret env NAME is not set` and nothing is created; shorter than 6 bytes is **400** `test account secret env NAME is too short to redact safely`; a value that cannot be redacted (a substring of `[redacted]`, or one that starts with a non-empty suffix of it) is **400** `test account secret env NAME cannot be redacted safely`. On an agent run, `started_at` is the create time until the job starts. No local checkout is **400** `no product repos resolved for worktrees …`. A ref that does not resolve is **400** after the run exists: the job is `failed` and the run is `abandoned`. |
| `GET` | `/api/test-runs/{id}` | Run and steps. Each step includes `defect_id` and `defect_status`. |
| `DELETE` | `/api/test-runs/{id}?evidence=1` | Deletes the run. `evidence=1` also removes that run's evidence directory. **409** `run is being executed by job <job-id>` while that `dd-job` is `queued` or `running`. |
| `POST` | `/api/test-runs/{id}/steps` | Appends an exploratory step. **409** unless the run is `in_progress`. **201** `{run}`. While a `dd-job` run's job is `queued` or `running`, **409** `run is being executed by job <job-id>`. |
| `PATCH` | `/api/test-runs/{id}/steps/{key}` | `status` and/or `actual`. `unverified` is **400**. **409** unless `in_progress`. The same live-job **409** applies. |
| `POST` | `/api/test-runs/{id}/steps/{key}/evidence` | Multipart screenshots, same limits as fix evidence. **409** unless `in_progress`. The same live-job **409** applies. |
| `POST` | `/api/test-runs/{id}/steps/{key}/file-defect` | Body `{title?, severity?, repos?, reporter?, note?}`. Files one defect from a `fail` or `blocked` step and copies that step's screenshots onto it. **201** `{defect, run}`. **400** unless the step is `fail` or `blocked`. **409** when the run is `abandoned`, or the step already has a defect (`step already has defect <DEF-id>`). |
| `POST` | `/api/test-runs/{id}/finish` | `in_progress` → `awaiting_review`. A `pass` step with no evidence becomes `unverified`. Pending steps stay pending. Other states are **409**. The live-job **409** applies while the job is `queued` or `running`. |
| `POST` | `/api/test-runs/{id}/abandon` | `queued`, `in_progress`, or `awaiting_review` → `abandoned`. Otherwise **409**. The live-job **409** applies while the job is `queued` or `running`, so a queued run can be abandoned only once that job is not live. |
| `POST` | `/api/test-runs/{id}/review` | Only from `awaiting_review` (**409** otherwise). `reviewer` is required. Each listed step is `accepted` or `disputed`. An unknown key or a `pending` step is **400**. Accepting an `unverified` step leaves its status `unverified`. |
| `POST` | `/api/test-runs/{id}/save-as-plan` | `{key, title}` creates a plan (**409** if that app already has the key) or `{plan_id}` replaces that plan's steps (exploratory flag dropped, `version` increments when the steps differ). **409** when the run is `abandoned`. |

App Settings accepts `test_accounts`: `[{label, username, environment, secret_ref}]`, returned on every app as an array. Any other key is **400** with `test_accounts only accept label, username, environment, secret_ref; passwords are never stored`. `label` is required and unique per app, case-insensitively. `secret_ref` is free text up to 200 characters; a value that starts with `env:` must match `env:` plus an uppercase environment-variable name (`^env:[A-Z_][A-Z0-9_]*$`). `PATH`, `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `PWD`, `LANG`, `TERM`, `CI`, and names starting with `LC_`, `GROK_`, `SKILL_FORGE_`, `DEFECT_DRAINER_`, `DEFECT_CHANNEL_`, `DD_`, `GIT_`, or `GH_` are **400** `secret_ref env:NAME is reserved; use a dedicated variable such as QA_PASSWORD`. A row saved before that check is refused at the next spawn with the same text. On run create, `account_label` copies the matching account into `env.account`. Passwords are never stored.

A manual run's `job_id` is null. An agent run's `job_id` is the test job. Per-step `defect_id` is null until a defect is filed, and `defect_status` is that defect's current status, or null when there is no link or the defect was deleted. Title is at most 200 characters. Purpose, action, expected, actual, and notes are at most 4000. `created_by`, `executed_by`, and `reviewer` are at most 120. `env` is a JSON object of at most 8 KiB. A filed defect has `source` `test-run` and `related` `["<TRUN-id>/steps/<step_key>"]`. `needs_retest` is true for a plan step when a linked defect is `resolved` and no reviewed run of that plan, with `started_at` after the defect's `updated_at`, has accepted that step as `pass` or `unverified`. Resume and Re-run of a test job are allowed from `awaiting_review` and are **409** when its run is `reviewed`, `abandoned` or deleted; `resumable` and `rerunnable` carry that reason. The same **409** is returned when the account secret is unset, shorter than 6 bytes, or cannot be redacted safely.

## What stays on the worker host

`git`, `gh`, and the coding-agent CLI are **not** replaced by this binary. They are required only on machines that run batch-fix. Tests inject fakes via `GROK_BUILD_BIN` and `GH_BIN`. Inventory/console do not need them.

## Related

- Retired TS API: `defect-drainer-backend` remote only — the `../backend/` tree was deleted 2026-09-22
- Operator UI: `../console/` (React/Vite; Node is build-time only)
- Umbrella status: `../tasks/todo.md`

## License

MIT — see [LICENSE](./LICENSE).
