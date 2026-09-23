# Parallel sessions and git worktrees

Portable process for **two agent sessions (or humans) editing the same child repo without sharing one working directory**. Feature-specific paths stay in the umbrella plan / local `SESSION.md` — not here.

## What is a worktree here

The **umbrella folder is not a git repo**. Only **child repos** can be git worktrees.

| Layer | What it is | `git worktree remove`? |
|-------|------------|------------------------|
| Primary umbrella | Sibling-repos folder (usually not git) | n/a |
| Feature umbrella | Second sibling folder that *holds* checkouts | n/a — delete the folder after children are gone |
| Child worktree | `git worktree add` of a **git** child, usually same branch name across related children | **Yes** |
| Folder copy | Deploy tree, `node_modules`, cloned extras nested in the feature umbrella | **No** — leftover after worktree remove |
| Other product umbrella | Separate codebase (authoring, ops). Not a worktree of this product | n/a |

A **branch** isolates commits after the fact. It does **not** isolate the working tree on disk. Two concurrent editors of the same child need **two working trees**.

## Prefer worktrees over a second full umbrella clone

```bash
# From the primary child checkout (e.g. umbrella/child-a)
git fetch
git worktree add /path/to/feature-umbrella/child-a -b feature/my-slice
# or attach an existing branch:
# git worktree add /path/to/feature-umbrella/child-a feature/my-slice

git worktree list
# remove when done — see Retire
```

- Create worktrees **only for git children you will edit** (not every sibling under the umbrella).
- Place them **outside** the primary checkout. Prefer a **thin feature umbrella** (below), not a nested path inside the primary tree.
- Use the **same branch name** across related children (e.g. backend + webapp both `feature/…`) for handoff clarity.
- Worktrees do **not** share `node_modules`; run install in each tree before tests.
- Worktrees must **not** share gitignored runtime state (SQLite, WAL, uploads, published assets). Each session that opens a local DB uses **its own** file — see **Runtime state**.

## Thin feature umbrella

When the slice touches **more than one child** (or needs its own `SESSION.md` / compose copy):

- Create a **sibling folder** of the primary umbrella. It is **not** a git repo.
- `git worktree add` each edited child **into** that folder.
- Open the **feature umbrella** as the session root — not a mix of primary child paths and worktree paths.
- Keep product paths, branch names, and DB paths in **that** umbrella’s `SESSION.md` and the feature plan. Do not freeze them in this skill or in long-lived primary `AGENTS.md` as the only SSOT.

```text
primary-umbrella/          # session root for other work
  child-a/                 # primary checkout (e.g. dev)
  child-b/
feature-umbrella/          # session root for this slice
  SESSION.md
  AGENTS.md                # optional; redirect when retiring
  child-a/                 # git worktree
  child-b/                 # git worktree
  child-deploy/            # often a COPY — not a worktree
```

Do **not** `git worktree add` a child that is not a git repo. If you need deploy/compose isolation, copy or generate it and record that it is a **copy** in `SESSION.md`.

## Session roots (hard rule)

| Session role | Open as workspace |
|--------------|-------------------|
| General / other feature | Primary umbrella (and its in-tree children) |
| Parallel feature | **Only** that feature umbrella (and its worktrees) |

Do **not** open both the primary child path and the worktree for the same feature in one multi-root workspace and “hope” the agent picks correctly.

A session “knows” its tree from **workspace CWD / git toplevel**, not from a global registry.

Shared host ports and compose **project names** collide across umbrellas. Stop the feature stack before starting the same services on the primary (and the reverse).

## Markers and AGENTS hooks (soft enforcement)

Nothing locks a folder. Soft signals only:

### 1. `SESSION.md` at the feature-umbrella root (recommended)

Local ownership note: owner session purpose, branch, primary paths that are off-limits, sibling worktrees, plan SSOT path, **absolute DB path for this tree**, what is a worktree vs a copy, cleanup command.

Optional: add `SESSION.md` to each child’s **`.git/info/exclude`** (shared by all worktrees of that repo) so a child-level marker stays **untracked** and never commits.

### 2. Child `AGENTS.md` load hook

In each child repo’s `AGENTS.md` (safe to merge — no-op when the file is absent):

```markdown
## Session ownership (worktree)

**If `SESSION.md` exists at this repo root, read it at session start**
(or before the first edit). It defines session ownership and off-limits paths.
If absent, ignore this section — normal primary-tree work.
```

`SESSION.md` is **not** auto-loaded by git. It is read only when:

- AGENTS instructs the agent to open it and the agent complies, or  
- the user `@`s it / the agent discovers it.

### 3. Umbrella `AGENTS.md` pointer

Thin project note: “parallel sessions use this skill § worktrees; feature-specific paths live in the relevant plan / `SESSION.md`.” Do **not** paste long process here or freeze ephemeral worktree paths in AGENTS forever. While a feature umbrella is active, one line pointing at it is enough; when retiring, point at the primary handoff instead.

### 4. Feature plan (optional)

Concrete absolute paths and branch names for **this** initiative — ephemeral until cleaned up.

## Worktree vs copy

`git worktree list` is the only authority for what is a worktree. Nested extras (deploy, vendor, media) are **copies** unless that list names them.

- `git worktree remove` deletes the worktree directory and unregisters it. It does **not** delete sibling copies or the feature-umbrella folder.
- After the last worktree is removed, delete leftover copies and the feature-umbrella folder explicitly (`rm -rf` only with user confirmation — restricted).
- Tool-created detached worktrees (for example under a child’s `.claude/worktrees/`) are not feature umbrellas. Remove them when leftover; do not treat them as the slice’s checkout.

## Runtime state (one file per session)

Git isolates **source**. It does not isolate **gitignored** SQLite (or WAL/SHM), uploads, or published assets. Two worktrees that both point `DB_PATH` (or equivalent) at the primary file share one live database: schema races, remediations stomping rows, WAL lock fights.

**Rule:** each worktree that opens a local DB gets its **own** file under that worktree. Seed with a checkpointed copy, not a shared path:

```bash
mkdir -p /path/to/feature-umbrella/child/db_dev
sqlite3 /path/to/primary-umbrella/child/db_dev/app.db \
  ".backup '/path/to/feature-umbrella/child/db_dev/app.db'"
```

Put the **absolute** path in that worktree’s `.env` and the feature `SESSION.md`. Tests that already use temp DBs stay isolated — leave them alone.

Published / generated content that is gitignored on purpose (packs, images, audio) is **data**, not a merge input. Recreate it on the target (publish, sync, generate). Do not copy it from the worktree unless the user asks.

### What merges later

| Artifact | Merge? | How |
|----------|--------|-----|
| **Source + schema files** | **Yes** | Git-merge the branch (including migration / `init` schema files). After merge, apply pending schema **once** on the target env’s file. |
| **Data** (`.db` + WAL, uploads, published assets) | **No** | Not a git merge and not a 3-way merge of two session files. Default: discard with worktree remove + folder delete. |
| **Specific rows you still want on primary** | **Optional, explicit** | Re-run the script/gate on the target DB, or `sqlite3 .backup` **replace** while nothing has the target open. |

## What does not enforce isolation

- Branch name alone
- `SESSION.md` without AGENTS (or user) causing a read
- Two sessions both opened on the umbrella primary trees
- Assuming agents see sibling worktrees automatically
- Two worktrees / processes pointing `DB_PATH` at the **same** SQLite file
- Assuming “we will merge the databases” (or published assets) when the branch merges
- A compose/deploy folder sitting next to worktrees — it is usually a copy, not a worktree

## When you do **not** need a worktree

- Plan/docs only under the umbrella (often not a git repo)
- Other session finished; primary tree is free — branch **in place** is enough
- Single session implementing the feature

## Checklist (seed a parallel feature)

1. [ ] Confirm other session owns the primary child checkout(s)
2. [ ] Create the thin feature umbrella folder (sibling of primary; not a git repo)
3. [ ] `git worktree add` for each **git** child to edit; shared branch name; record any nested **copies** in `SESSION.md`
4. [ ] Write feature-umbrella `SESSION.md`; optional `.git/info/exclude`
5. [ ] **Own DB file** if that child opens SQLite: `.backup` primary into the worktree; set worktree `.env`; record the absolute path in `SESSION.md`. Never reuse primary `db_dev/` or `db/`
6. [ ] Ensure child `AGENTS.md` has the “if SESSION.md exists, read it” hook
7. [ ] Open the **new session** with workspace root = the feature umbrella only
8. [ ] Record concrete paths in the feature plan if useful
9. [ ] One line on primary `AGENTS.md` / `tasks/todo.md` pointing at the feature umbrella while it is active

## Retire on the primary

When the slice is done, the next session must resume on the **primary**, not the feature umbrella.

1. **Handoff on primary** — `HANDOFF.md` (or equivalent) plus a self-contained section in primary `tasks/todo.md`. Pointer stubs in each affected child. Feature-umbrella `SESSION.md` redirects here.
2. **Stop** feature-umbrella servers (same host ports / compose project name as primary).
3. **Merge code** into the primary branch **from the primary checkout** (`git merge feature/…` while on that branch). Do **not** check the feature branch out on primary while a worktree still holds it.
4. **Re-apply data on the target** — schema on the primary DB file; republish / sync gitignored assets. Do **not** copy worktree SQLite, audio, or published files unless the user asks.
5. **Cleanup** (same session as merge, after publish/sync if the worktree still holds the only copy of needed data):

```bash
# unregister + delete each git worktree
git -C /path/to/primary/child-a worktree remove /path/to/feature-umbrella/child-a
git -C /path/to/primary/child-b worktree remove /path/to/feature-umbrella/child-b

# leftover copies + the feature umbrella itself
rm -rf /path/to/feature-umbrella   # restricted: user confirmation first

git -C /path/to/primary/child-a worktree list   # no feature-umbrella paths
```

If `worktree remove` refuses (dirty tree), stop processes and retry; `--force` only after merge when the worktree DB is disposable.

6. **Do not touch** unrelated worktrees (other feature umbrellas, leftover tool checkouts you were not asked to remove).
7. **Trim** the primary `AGENTS.md` “open the feature umbrella” line; archive the merge task; drop aliases that still write into the deleted folder.

Optional: delete the local feature branch after it is merged. Keep it until merge is on the intended primary branch.
