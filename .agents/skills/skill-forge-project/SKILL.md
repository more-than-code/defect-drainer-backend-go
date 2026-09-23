---
name: skill-forge-project
description: >
  Drive skill selection for a Skill Forge consumer repository: detect the
  stack, propose a fitting set of registry skills with rationale, wait for
  user confirmation, then apply it via the `skf project`/`skf sync` CLI.
  Also covers the consumer's own staleness duty: checking the repo's profile
  against the registry and against `$HOME` before relying on it, and saying so
  when it has drifted. Activate when the repo contains `skill-forge.json`, or
  the user asks which skills a project should use, add, or drop.
---

# Skill Forge Project Skill Selection

Helps an agent pick which registry skills a repository should depend on, and
apply that choice safely. This is the *fit-analysis* layer on top of the
lower-level `skill-forge-authoring` skill (which mutates skill content/versions —
not used here).

## When to activate

- The repo has a `skill-forge.json` (a Skill Forge consumer project).
- The user asks "which skills should this project use", "set up skills
  here", "add a skill for X", or "why is this project's skill set out of
  sync".

**Do not use this skill for:** authoring or versioning registry skills under
`inventory/skills/` (that's `skill-forge-authoring`), or installing managed
agents/subagents/hooks (`skf agent|subagent|hook install`).

## Decision workflow

1. **Inspect the repo.** Detect languages, frameworks, test runners, containers/
   Compose, CI config, and whether it's frontend-, backend-, or full-stack. Use
   whatever's fastest and accurate — manifest files (`package.json`,
   `pyproject.toml`, `go.mod`, `Cargo.toml`), `Dockerfile`/`compose.yaml`,
   existing CI workflows, directory shape.
2. **List candidates.** Run `skf skill list --json` (add `--all` only if
   non-installable/tracked skills are relevant to the question). Each entry
   now carries `name`, `version`, `tags`, and `description` — use those to
   match against what step 1 found. Filter client-side; there is no server-side
   `--query`.
3. **Propose a minimal set**, not everything that could plausibly apply:
   - **Declare what the repo needs, whether or not `$HOME` also provides it.**
     A repo's manifest should state its own requirements, so it resolves the
     same way on a machine whose `$HOME` profile is empty. The baseline trio
     (`security-baseline`, `coding-discipline`, `code-quality`) therefore
     belongs in each repo's profile regardless of what the machine happens to
     carry.

     Overlap does **not** double-load. Observed 2026-09-22 on two tools
     independently: names present in both the project and `$HOME` trees
     surfaced exactly once each, resolved project-over-home. The at-rest cost
     of an overlapping name is zero, because only frontmatter is read at
     discovery and that name was already being read.

     The cost that is real is **shadowing**: the project copy wins
     unconditionally, so once the repo's pin lags a `$HOME` bump, the stale
     project copy is what loads — and `sync --check` passes at both levels,
     because each is internally consistent. Keep the pins deliberate and
     re-sync repos you have not touched in a while.
   - Add stack-specific skills only where a detected signal maps directly to a
     skill's tags/description (e.g. a `Dockerfile`/compose file →
     `podman-utilization`; a frontend framework → `frontend-engineering` and/or
     `ui-portability-baseline`).
   - One line of rationale per proposed skill, naming the signal that justified it.
4. **Wait for explicit confirmation.** Present the proposed set and rationale,
   then stop. Do not run `project add`/`sync` until the user confirms the set
   (or an adjusted version of it).
5. **Apply the confirmed set:**
   - If `skill-forge.json` doesn't exist yet: `skf project init` (add
     `--shims <list>` only to narrow which compat shims are created; the default
     is all known shims, today just `claude-code`).
   - `skf project add <name...>` for the confirmed skills (omit names to fall
     back to the CLI's own interactive search instead of picking for the user).
   - `skf sync` to vendor the skills and write `skill-forge.lock.json`.
   - Verify with `skf sync --check` (must exit 0) and `skf project status --json`
     to confirm the resolved `skills` list matches what was proposed.
6. **Report back** the final skill set and sync state.

## Staleness is the consumer's job — check it, and say so

Nothing central knows which repositories are consumers. Observed 2026-09-22:
`skf`'s pin rewriting walks exactly two roots — the registry checkout and
`$HOME` — so a repo it has never been run in is invisible to every registry-side
command, and its skills can lag indefinitely with nothing reporting it. The repo
you are working in is the only place the check can happen, which makes it yours
to run.

**Two checks, at the start of work in a consumer repo. Both are read-only.**

1. **Drift against the registry** — one command:

```bash
skf sync --check          # exit 0 = current; non-zero prints what is wrong
```

   It catches both failure modes: a lockfile that predates a registry change,
   and a pin the registry can no longer satisfy (that one errors outright,
   e.g. `Skill "x"@0.1.1 does not satisfy range "^9.0.0"`). The fix is `skf
   sync`, except for an unsatisfiable pin, which needs `skf project add <name>`
   to widen the range first.

2. **Shadowing against `$HOME`** — which no `sync --check` can see, because each
   profile is internally consistent with its own declaration and both report
   "in sync" while disagreeing with each other. Compare the two manifests for
   names they share:

```bash
skf project status --json     # this repo's resolved versions
cat ~/skill-forge.json        # the machine-wide ranges
```

   A name in both loads the **project** copy, always. So a project pinned
   `^0.8.0` against a `$HOME` at `^0.9.0` silently serves the older skill to
   every session rooted here, and nothing in either profile is wrong on its own
   terms.

**Triage before you shout — "stale" has three severities, and only two matter.**
A consumer's lockfile records the *registry's* commit, so **any** registry change
marks **every** consumer stale, even one whose own skills did not move. Observed
2026-09-22: a Flutter consumer reported stale after an unrelated skill was bumped;
none of its four declared skills had changed, and re-syncing rewrote only the
pointer.

| What `--check` found | Severity | What to say |
|---|---|---|
| Pin the registry cannot satisfy | **High** — blocks | Name the skill and range; it needs `project add` to widen before anything syncs |
| A declared skill resolved to a new version | **Medium** | Name the skills and the version change; the guidance you are about to follow just moved |
| Registry moved, none of your skills did | **Low** | One line: pointer was behind, re-synced, content unchanged |

To tell the last two apart, sync and look at what actually changed on disk:

```bash
skf sync
git status --porcelain .agents/skills    # empty = it was only the registry pointer
```

**Then say it out loud, at the severity it deserves.** A stale profile is not a detail to note and move past:
it means the guidance you are about to follow is not the guidance the registry
currently holds. State which skills are affected, which version is actually
loading, and the one command that fixes it — before doing the work, not after.
Silently proceeding on skills you know are stale is the failure this section
exists to prevent.

## Rules

- **Never hand-copy skill directories.** Only `skf sync` writes into
  `.agents/skills/` or `.claude/skills/`; don't `cp -r` from `inventory/skills/`
  or another project.
- **Never write to `~/.<tool>/skills`.** Global per-tool skill installs are
  retired; project skills are repo-scoped via `skill-forge.json` + `sync`. A
  project that wants skills available everywhere should instead be summarized
  to the user as "declare it in the `$HOME` profile" (`skf home add <name>` +
  `skf home sync`), not solved by writing into a tool's global skills
  directory.
- **Respect `skills.local` collisions.** If `sync` fails with "not managed by
  Skill Forge", a pre-existing hand-authored directory is at that vendor path —
  ask the user whether to declare it under `skills.local` or move it aside.
  Never delete it to make sync pass.
- **Local skills are declared where they already live.** `sync` does not copy
  `skills.local` entries anywhere — the source dir must already be at a path the
  tool reads. See "Authoring a repo-local skill" below.
- **Commit together.** `skill-forge.json`, `skill-forge.lock.json`, and the
  vendored `.agents/skills/`/`.claude/skills/` directories change as one unit —
  don't split them across commits.

## Authoring a repo-local skill

Some doctrine is true only for one repo — a house pattern, a workaround for a
quirk in that codebase, a convention no other project shares. That belongs in
`skills.local`, not the registry. The registry rule applies in reverse here: if a
line stops being true when pointed at a different product, it must **not** go to
`inventory/skills`.

**The mechanic that surprises people: `sync` does not vendor local skills.** It
copies registry skills into every target dir; for a local skill it only records
`path` + `integrity` in the lockfile. Nothing is moved or copied. So the source
directory has to already sit where the agent tool looks:

```
<repo>/.agents/skills/<name>/SKILL.md      # the source itself, not a copy
```

There is exactly one such path. `.claude/skills` is a symlink onto
`.agents/skills`, so the skill is reachable through it without a second source,
a second link, or a second declaration.

Declare it against that same path:

```json
{ "skills": { "local": { "my-house-rule": ".agents/skills/my-house-rule" } } }
```

Then `skf sync` and confirm `skf sync --check` exits 0.

**What declaring it buys you.** Without the declaration the directory is an
undeclared stranger in a vendor dir: `sync` refuses to run (`exists but is not
managed by Skill Forge`), or prunes it as an orphan. Declaring it exempts the
path from pruning and puts the tree under integrity tracking, so a later edit
shows up as drift in `sync --check` instead of passing silently.

**Constraints the CLI enforces:**

- A name cannot be both a registry dependency and a local skill — `sync` fails
  outright rather than picking one.
- The path must be relative and inside the repo; `..` and absolute paths are
  rejected.
- The path must exist when `sync` runs, or it fails with
  `skills.local["<name>"] path "<p>" does not exist`.

**Constraints the CLI does *not* enforce** — the agent tools do, silently:

- Frontmatter still needs `name` (matching the directory) and `description`.
  A local skill is never validated by `skf validate`; a malformed one simply
  never activates.
- No `version:` in frontmatter. Versions live in the registry, and a local
  skill has no registry row — its lockfile `integrity` hash is its identity.

**Never link a local skill into a tool directory by name.** A tool directory is
already a shim onto `.agents/skills`, so `ln -s … .claude/skills/<name>` writes
the link *inside* the store it points at. One source, declared once, is the whole
arrangement — `sync` writes the shim and `integrity` tracks the real directory.

**One source, one place.** A local skill's source directory lives in exactly one
repo. Copying it to a second location — say, an umbrella mirroring a child's
skill so umbrella-rooted sessions can see it — produces a copy `sync` will never
refresh and `integrity` never covers, because tracking follows the declared path
only. If a skill is needed at both levels, promote it to the registry
(`skill-forge-authoring`) and declare it as a dependency in both profiles.

## Command map

```bash
skf skill list --json                 # candidates: name, version, tags, description
skf project init [--shims <list>]      # only if skill-forge.json is missing
skf project add [<name>...]            # confirmed names, or interactive if omitted
skf project status [--json]            # resolved skills, sources, sync state, issues
skf home init|add|status|sync          # same flow for the machine-wide $HOME profile
skf sync                               # vendor + write lockfile
skf sync --check                       # read-only staleness check (exit non-zero on drift)
```

## Anti-patterns

| Wrong | Right |
|-------|-------|
| Decide the skill set and run `project add` + `sync` without confirmation | Propose with rationale, wait for explicit user confirmation |
| `cp -r inventory/skills/foo .agents/skills/foo` | `skf project add foo` then `skf sync` |
| Install a skill to `~/.claude/skills` for "everywhere" access | `skf home add <name>` + `skf home sync` |
| Delete a colliding hand-authored `.agents/skills/<name>` dir to unblock `sync` | Ask the user; use `skills.local` or move it aside |
| Propose every tag-matching skill in the registry | Propose the minimal set that maps to a detected signal |
| Leave a needed skill out because `$HOME` happens to provide it | Declare what the repo needs; it should resolve the same with an empty `$HOME` |
| Start work in a consumer repo without checking whether its skills are current | `skf sync --check` first; nothing else will ever run it for this repo |
| Notice drift and carry on quietly | Say which skills are stale, which version loads, and the fix — before doing the work |
| Assume an overlapping name double-loads | It dedupes, project-over-home; the hazard is a stale project pin shadowing a newer `$HOME` one |
| Declare `skills.local` and expect `sync` to place the directory for you | Put the source at `.agents/skills/<name>` first, then declare that path |
| Copy a local skill's source into a second repo or level so both see it | One source dir; if both levels need it, promote it to the registry |
| Put a product-specific house rule in `inventory/skills` so it is "reusable" | `skills.local` in the repo it is true for |

## Related

- `skill-forge-authoring` skill — authoring/versioning **registry** skills; it refuses paths
  outside `inventory/skills`, so it cannot touch a consumer repo's `skills.local`
- `docs/DESIGN.md` § "Project Skill Profiles" — why skills are project-scoped
