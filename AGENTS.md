# defect-drainer-go

**Folder:** `backend-go/` · **Git name:** `defect-drainer-go`  
**Module:** `github.com/joe/defect-drainer-go`

Go control-plane binary (`defect-drainer`) that will replace Node as the harness API runtime. Design: `../docs/go-backend.md`. Framing: `../docs/workflow.md`.

- Inventory SSOT is **shared**: `backend/.data/defect-drainer.db` + umbrella `../evidence/` — do not invent a second store
- One writer per data dir (`{DATA}/defect-drainer.lock`). TS must flock too before a shared-DATA experiment
- Env: `DEFECT_DRAINER_*` (legacy `DEFECT_CHANNEL_*` still read). `DEFECTS_ROOT` has no prefix pair
- Product language: agent harness / coding agent / workflow / inventory
- Cross-repo plans: `/Users/joe/workspace/defect-drainer/tasks/todo.md`
- Local tasks: `tasks/todo.md` (this package only)
- Run: `go test ./...`; later `go run ./cmd/defect-drainer serve` → `127.0.0.1:8788`

## Session ownership (worktree)

**If `SESSION.md` exists at this repo root, read it at session start**
(or before the first edit). It defines session ownership and off-limits paths.
If absent, ignore this section — normal primary-tree work.
