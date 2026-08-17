# defect-drainer-go

**In-workspace path:** `backend-go/`  
**Git repo name:** `defect-drainer-go`  
**Module:** `github.com/joe/defect-drainer-go`  
**Binary:** `defect-drainer`

Go control-plane rewrite of the Defect Drainer harness API. Goal: a static binary so operator and EC2 hosts do not need **Node as a runtime**. The live operator API is still TypeScript `backend/` until cutover.

Design: [`../docs/go-backend.md`](../docs/go-backend.md). Product framing: [`../docs/workflow.md`](../docs/workflow.md).

This tree is **PR 0 (stub CLI)** only: `version` works; `serve`, `healthcheck`, and `worker` exit 1 (`not implemented`). No listen, no SQLite.

## Run

```bash
cd /Users/joe/workspace/defect-drainer/backend-go
go test ./...
go run ./cmd/defect-drainer version
# later:
# go run ./cmd/defect-drainer serve
```

Or `make test` / `make build` (`CGO_ENABLED=0`). Cross-compile: `make cross-linux`.

## One-writer rule

The inventory is **one SQLite file** plus umbrella `evidence/`. Only one backend process (TS **or** Go) may write a given data dir.

- Mutex: both TS (coexistence lock PR) and Go take `LOCK_EX|LOCK_NB` on `{DATA}/defect-drainer.lock`.
- Flock only in Go does **not** stop the TS backend. Do not point both at the same DATA until that TS PR is in.
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

## What stays on the worker host

`git`, `gh`, and the coding-agent CLI are **not** replaced by this binary. They are required only on machines that run batch-fix (later PRs). Inventory/console do not need them.

## Related

- Live API: `../backend/` (`defect-drainer-backend`)
- Operator UI: `../console/` (React/Vite; Node is build-time only)
- Umbrella status: `../tasks/todo.md`

## License

MIT — see [LICENSE](./LICENSE).
