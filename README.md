# defect-drainer-go

**In-workspace path:** `backend-go/`  
**Git repo name:** `defect-drainer-go`  
**Module:** `github.com/joe/defect-drainer-go`  
**Binary:** `defect-drainer`

Go control-plane rewrite of the Defect Drainer harness API. Goal: a static binary so operator and EC2 hosts do not need **Node as a runtime**. Compose `api.build` is `../backend-go`; rollback is revert that line to `../backend`. Local TS `pnpm dev` still works when this binary is not running.

Design: [`../docs/go-backend.md`](../docs/go-backend.md). Product framing: [`../backend/docs/workflow.md`](../backend/docs/workflow.md).

`serve` listens after POSIX `flock` + SQLite open and serves the console HTTP contract (inventory, intake, batches, search). `healthcheck` GETs `HOST:PORT/health` and exits 0 only when HTTP 200 **and** `ok: true`. The `worker` **subcommand** is still reserved (exit 1); batch-fix spawn runs **in-process** from `serve` (TS model).

## Run

```bash
cd /Users/joe/workspace/defect-drainer-go-rewrite/backend-go
go test ./...
go run ./cmd/defect-drainer version
export DEFECTS_ROOT=/Users/joe/workspace/defect-drainer
export DEFECT_DRAINER_DATA=/Users/joe/workspace/defect-drainer-go-rewrite/runtime
go run ./cmd/defect-drainer serve
```

Or `make test` / `make build` (`CGO_ENABLED=0`). Cross-compile: `make cross-linux`.

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

If those roots cannot be resolved (PATH-installed binary, no `go.mod` in cwd, no existing sibling `backend/.data/defect-drainer.db`), the process **exits 1**. It will not mkdir a second inventory.

During coexistence, point at the live SSOT:

```bash
export DEFECTS_ROOT=/Users/joe/workspace/defect-drainer
export DEFECT_DRAINER_DATA=/Users/joe/workspace/defect-drainer/backend/.data
```

`healthcheck` uses **HOST/PORT only** (default `127.0.0.1:8788`). It does not walk `go.mod`, flock, or open SQLite.

## Batch-fix (in `serve`, after worktrees)

`POST /api/batches` with `start_fix: true` and `mode: grok` follows the TS path:

1. Flip selected `open` / `triaged` defects to `in_progress`.
2. Create `git worktree`s (`defect-drainer/<BATCH-id>`). No local/repo URLs → **400** and `syncBatchAndDefects(..., failed)` (defects reopen).
3. Persist bindings on `job.worktrees`. Empty worktrees refuse spawn.
4. `exec` the coding-agent bin (`GROK_BUILD_BIN` → `DEFECT_DRAINER_GROK_BIN` → `SKETCH_FORGE_GROK_BIN` → Homebrew/`PATH` `grok`).
5. `POST /api/batch-jobs/{id}/create-prs` and `refresh-prs` run host `gh` (`GH_BIN` overrides) and write `job.prs[]` (`status`, `url`, `ghState`, `mergedAt`, `checkedAt`). Skip when there are no commits vs the resolved base (never `origin/origin/main`).

`start_fix: true` + `manual` stays planned / job `manual` and does **not** flip defects.

## What stays on the worker host

`git`, `gh`, and the coding-agent CLI are **not** replaced by this binary. They are required only on machines that run batch-fix. Tests inject fakes via `GROK_BUILD_BIN` and `GH_BIN`. Inventory/console do not need them.

## Related

- Live API: `../backend/` (`defect-drainer-backend`)
- Operator UI: `../console/` (React/Vite; Node is build-time only)
- Umbrella status: `../tasks/todo.md`

## License

MIT — see [LICENSE](./LICENSE).
