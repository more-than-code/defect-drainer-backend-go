# defect-drainer-go

**Folder:** `backend-go/` · **Git name:** `defect-drainer-backend-go`  
**Module:** `github.com/joe/defect-drainer-go`

Go control-plane binary (`defect-drainer`) — **the** harness API runtime. Design: `../docs/go-backend.md`. Framing: `docs/workflow.md`. Rationale for the verification gates: `docs/verification-hardening.md`. The TS `backend/` tree was retired and deleted on 2026-09-22; its history lives in the `defect-drainer-backend` remote.

- Inventory SSOT: `.data/defect-drainer.db` (this module) + umbrella `../evidence/` — do not invent a second store
- One writer per data dir: `syscall.Flock` takes `LOCK_EX|LOCK_NB` on `{DATA}/defect-drainer.lock` before listen (the retired TS tree used `koffi`/`libc.flock` on the same file)
- Restarting `serve` is not free: spawned coding-agent children live in-process (`procs[jobID]`) and the signal path is a bare `srv.Close()` with no drain (`cmd/defect-drainer/main.go`). Check `GET /api/batches` for a job still `running` / `waiting_external` before killing — a restart mid-job orphans its agent child (holding a worktree) and loses the in-memory job state. The replacement cannot bind until the old process releases the lock, so stop, wait for the port, then start; never start a second one alongside
- `start_fix`+`grok` is the worker path (flip `in_progress` → worktrees → BRIEF.md + PROCESS.md + SKILLS.md → toolchain/sandbox provision → baseline → spawn (child env `SKILL_FORGE_AGENT_ROLE=worker`) → verify → verdict-gated harvest/resolve). Diff hygiene is advisory (`GET /api/batch-jobs/{id}/diff/{repo}`). Job-scoped `<handoff>/.grok/sandbox.toml` (`dd-simulator`) never edits `~/.grok`. `create-prs`/`refresh-prs` exec `GH_BIN`/`gh` and persist `job.prs[]` (`ghNumber`). Job JSON key is `jobId`.
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
