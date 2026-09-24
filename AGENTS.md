# defect-drainer-go

**Folder:** `backend-go/` · **Git name:** `defect-drainer-backend-go`  
**Module:** `github.com/joe/defect-drainer-go`

Go control-plane binary (`defect-drainer`) — **the** harness API runtime. Design: `../docs/go-backend.md`. Framing: `docs/workflow.md`. Operator runbook (job actions, safe restart, orphaned agents): `docs/operating.md`. Rationale for the verification gates: `docs/verification-hardening.md`. The TS `backend/` tree was retired and deleted on 2026-09-22; its history lives in the `defect-drainer-backend` remote.

- Inventory SSOT: `.data/defect-drainer.db` and `.data/evidence` (this module) — do not invent a second store
- One writer per data dir: `syscall.Flock` takes `LOCK_EX|LOCK_NB` on `{DATA}/defect-drainer.lock` before listen (the retired TS tree used `koffi`/`libc.flock` on the same file)
- Restarting `serve` is not free: spawned coding-agent children live in-process (`procs[jobID]`) and the signal path is a bare `srv.Close()` with no drain (`cmd/defect-drainer/main.go`). Check `GET /api/batches` for a job still `running` / `waiting_external` before killing — a restart mid-job orphans its agent child (holding a worktree) and loses the in-memory job state. The replacement cannot bind until the old process releases the lock, so stop, wait for the port, then start; never start a second one alongside. Procedure, including finding and killing orphaned agents: `docs/operating.md` → Restarting the backend
- `start_fix`+`grok` is the worker path (flip `in_progress` → worktrees → BRIEF.md + PROCESS.md + SKILLS.md → toolchain/sandbox provision → baseline → spawn driver loop (child env `SKILL_FORGE_AGENT_ROLE=worker`) → verify → verdict-gated harvest/resolve). `POST …/resume` and `…/rerun` restart the same job through `Restart` (`internal/jobs/restart.go`, the only home of the restart rules): 409 while any agent or driver goroutine for it is live, the first run's baseline and `diffBase` reused, `agentRun` cumulative, and harvest imports only images newer than `runStartedAt`. Diff hygiene is advisory (`GET /api/batch-jobs/{id}/diff/{repo}`). Job-scoped `<handoff>/.grok/sandbox.toml` (`dd-simulator`) never edits `~/.grok`; it denies its own `.grok` dir and is byte-checked before every iteration (`checkSimulatorSandboxProfile`) — a changed profile, or a `dd-simulator` in `$GROK_HOME/sandbox.toml`, fails the job. Worktrees are cut from the repo's saved base (`baseSource=local` uses that local branch; `origin` uses `<remote>/<branch>`), and a missing base fails the batch. `create-prs`/`refresh-prs` open a GitHub PR for `origin` and, for `local`, commit the worktree files the operator confirmed and then merge into the local branch (primary tree only when it is already on that branch and clean); both persist `job.prs[]` (`kind`, `ghNumber`). `ship-preview` lists those files first. Job JSON key is `jobId`.
- Agent turn cap: app `agent_max_turns` → `DEFECT_DRAINER_GROK_BATCH_MAX_TURNS` → `GROK_BATCH_MAX_TURNS` → `80`; stored value clamped 1..1000. Hitting it ends one **iteration**, not the job: the agent exits 1 with a `max_turns_reached` event and the driver loop resumes the session (`-c`) until the required artifacts exist or `DEFECT_DRAINER_AGENT_MAX_ITERS` (8) / `DEFECT_DRAINER_AGENT_MAX_WALL_SEC` (2700) stops it. A capped run still verifies and harvests, and ends `completed` with `agentRun.stoppedBy` = `iterations`/`wall` and `missing` listed, so the operator can Resume it with a fresh budget. Only a non-zero exit **without** that event fails the job
- Env: `DEFECT_DRAINER_*` (legacy `DEFECT_CHANNEL_*` still read). `DEFECTS_ROOT` has no prefix pair
- Product language: agent harness / coding agent / workflow / inventory
- Cross-repo plans: `/Users/joe/workspace/defect-drainer/tasks/todo.md`
- Local tasks: `tasks/todo.md` (this package only)
- Run: `go test ./...`; `go run ./cmd/defect-drainer serve` (or `make serve`) → `127.0.0.1:8788` after lock + DB open. DATA defaults to this module's `.data` when that DB exists. `.env` is read from the module root (`backend-go/.env`), not the umbrella.
- Only extra module: pinned `modernc.org/sqlite`. `CGO_ENABLED=0`. Flock is `syscall.Flock`.

## Session ownership (worktree)

**If `SESSION.md` exists at this repo root, read it at session start**
(or before the first edit). It defines session ownership and off-limits paths.
If absent, ignore this section — normal primary-tree work.
