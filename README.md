# defect-drainer-go

**In-workspace path:** `backend-go/`  
**Git repo name:** `defect-drainer-backend-go`  
**Module:** `github.com/joe/defect-drainer-go`  
**Binary:** `defect-drainer`

Go control-plane of the Defect Drainer harness API. Goal: a static binary so operator and EC2 hosts do not need **Node as a runtime**. This tree is **the** API (`serve` :8788) and owns the inventory data dir. Compose `api.build` is `../backend-go`. The TS `backend/` tree it replaced was retired and deleted on 2026-09-22 — there is no `pnpm dev` rollback; its history is in the `defect-drainer-backend` remote.

Design: [`../docs/go-backend.md`](../docs/go-backend.md). Product framing: [`docs/workflow.md`](docs/workflow.md). Why the verification gates exist: [`docs/verification-hardening.md`](docs/verification-hardening.md).

`serve` listens after POSIX `flock` + SQLite open and serves the console HTTP contract (inventory, intake, batches, search). `healthcheck` GETs `HOST:PORT/health` and exits 0 only when HTTP 200 **and** `ok: true`. The `worker` **subcommand** is still reserved (exit 1); batch-fix spawn runs **in-process** from `serve`.

## Run

```bash
cd /Users/joe/workspace/defect-drainer/backend-go
go test ./...
go run ./cmd/defect-drainer serve
# → http://127.0.0.1:8788
# DEFECTS_ROOT = umbrella; DEFECT_DRAINER_DATA = this module's .data when that DB exists.
```

Or `make serve` / `make test` / `make build` (`CGO_ENABLED=0`). Cross-compile: `make cross-linux`.

## One-writer rule

The inventory is **one SQLite file** plus umbrella `evidence/`. Only one backend process (TS **or** Go) may write a given data dir.

- Mutex: both TS (`koffi` → `libc.flock`) and Go (`syscall.Flock`) take `LOCK_EX|LOCK_NB` on `{DATA}/defect-drainer.lock`. A second locker fails immediately and does not listen.
- Operator backup: never start two servers against one data dir.

## Fail-closed paths (`serve`, not `healthcheck`)

| Env | Role |
|-----|------|
| `DEFECTS_ROOT` | Inventory / `evidence/` (no `DEFECT_DRAINER_` prefix) |
| `DEFECT_DRAINER_DATA` | Runtime: `defect-drainer.db`, `jobs/`, `batch-jobs/`, `clones/` |
| `DEFECT_DRAINER_HOST` / `PORT` | Listen (default `127.0.0.1:8788`) |

Legacy `DEFECT_CHANNEL_*` is still read. `.env` is only `backend-go/.env`, loaded in `main()`, never overriding process env.

If those roots cannot be resolved (PATH-installed binary, no `go.mod` in cwd, no existing `backend-go/.data/defect-drainer.db`), the process **exits 1**. It will not mkdir a second inventory.

From `backend-go/` in this umbrella, `serve` already picks that SSOT. Override only when the binary cannot see the tree:

```bash
export DEFECTS_ROOT=/Users/joe/workspace/defect-drainer
export DEFECT_DRAINER_DATA=/Users/joe/workspace/defect-drainer/backend-go/.data
```

`healthcheck` uses **HOST/PORT only** (default `127.0.0.1:8788`). It does not walk `go.mod`, flock, or open SQLite.

## Batch-fix (in `serve`, after worktrees)

`POST /api/batches` with `start_fix: true` and `mode: grok` follows the TS path:

1. Flip selected `open` / `triaged` defects to `in_progress`.
2. Create `git worktree`s (`defect-drainer/<BATCH-id>`). No local/repo URLs → **400** and `SyncBatchAndDefects(..., failed)` (defects reopen).
3. Persist bindings on `job.worktrees`. Empty worktrees refuse spawn.
4. Write `BRIEF.md` (worker role, decisions, acceptance criteria, defects), the DD-owned `PROCESS.md` worker contract, the generated `SKILLS.md` (per-worktree contract files and `SKILL.md` entries from `.agents/skills` / `.claude/skills` / `.grok/skills` — name, one-line summary and path, never bodies), and the batch manifest. Fail-closed 400s still write all three so the handoff explains why spawn did not start.
5. If `agent_toolchain=flutter`, clone the pinned SDK into `<handoff>/.tooling` (`cp -Rc`; skip + warn on missing/conflicting pins). If `allow_simulator_writes`, write a job-scoped `<handoff>/.grok/sandbox.toml` (`dd-simulator`) — never `~/.grok`. Record each worktree `HEAD`. If `verify_commands` is set, run them as **baseline** into `<handoff>/baseline` and persist `job.baseline`.
6. `exec` the coding-agent bin (`GROK_BUILD_BIN` → `DEFECT_DRAINER_GROK_BIN` → `SKETCH_FORGE_GROK_BIN` → Homebrew/`PATH` `grok`) with `--sandbox` from the app `grok_sandbox` (`workspace` or `strict`), or `dd-simulator` when a job profile was written. `--max-turns` comes from the app's `agent_max_turns`, else `DEFECT_DRAINER_GROK_BATCH_MAX_TURNS` / `GROK_BATCH_MAX_TURNS`, else `80`; the stored value is clamped to 1..1000 so a settings typo cannot buy an unbounded run. Output format is `streaming-json`, so DD reads the `end` event (`stopReason`, `num_turns`, `total_cost_usd`, `sessionId`) instead of inferring from exit status. Prompt includes toolchain notes, VERIFICATION commands, and already-failing baseline rows. Iterations 2+ re-exec with `-c` and a code-constant continue prompt.
7. The driver loop repeats step 6 until `artifactsComplete` (`NOTES.md`, plus `fix-notes/<id>.md` and a harvestable `fix-evidence/<id>/` image per defect, or an exact `UNFIXED:` / `NO-SCREENSHOT:` line) or a cap ends it; `job.agentRun` records iterations, `stoppedBy`, turns and cost after **each** iteration. Then on **no genuine failure**: re-run `verify_commands` into the handoff (`job.verification`, `verify.json`), measure diff hygiene vs the recorded heads (`job.diffHygiene`; advisory only), `judgeVerification` (only `regression` and `blocked` stop a resolve), then harvest with resolve permitted. On **failure** (non-zero exit that is not turn exhaustion): harvest with no verification and `AllowResolve` false — evidence and notes are imported, nothing resolves. On **cancel**: existing stop path.
8. Harvest `fix-evidence/` + `fix-notes/` (regular files under the handoff only). Evidence may import even when the verdict is not ok; resolve additionally requires an image imported by **this run** (historical `fix_evidence` on the defect is not proof this job fixed anything) and a run permitted to resolve. Then `SyncBatchAndDefects`: `complete` leaves harvested defects; `failed`/`cancelled` reopens `in_progress` rows with no `fix_evidence`.
9. `POST /api/batch-jobs/{id}/create-prs` and `refresh-prs` run host `gh` (`GH_BIN` overrides) and write `job.prs[]` (`status`, `url`, `ghNumber`, `ghState`, `mergedAt`, `checkedAt`, `branch`, `error`). Skip when there are no commits vs the resolved base (never `origin/origin/main`).
10. `GET /api/batch-jobs/{id}/diff/{repo}` returns hunks for the console drill-down (`kind=reflow|all`). 404 unknown job / no worktree; 409 no recorded `baseSha`; 410 worktree gone.

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

The verification runner is **not sandboxed**. Commands come only from App Settings (`verify_commands`), never from the agent. Their PATH is the host pin (`$FVM_HOME/versions/<pin>/bin`), not the agent-writable handoff clone.

`start_fix: true` + `manual` stays planned / job `manual` and does **not** flip defects.

## What stays on the worker host

`git`, `gh`, and the coding-agent CLI are **not** replaced by this binary. They are required only on machines that run batch-fix. Tests inject fakes via `GROK_BUILD_BIN` and `GH_BIN`. Inventory/console do not need them.

## Related

- Retired TS API: `defect-drainer-backend` remote only — the `../backend/` tree was deleted 2026-09-22
- Operator UI: `../console/` (React/Vite; Node is build-time only)
- Umbrella status: `../tasks/todo.md`

## License

MIT — see [LICENSE](./LICENSE).
