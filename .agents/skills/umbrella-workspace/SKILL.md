---
name: umbrella-workspace
description: >
  Cooperation rules for multi-repo (umbrella) workspaces: where docs and tasks live,
  self-contained task entries, pointer stubs in child repos, handoff checklists,
  plan-vs-status split, where skill profiles belong (every session root declares its
  own; a session reads only its own root),
  and parallel agent sessions via git worktrees (SESSION.md
  markers, AGENTS load hooks, thin feature umbrellas, worktree vs copy, retire
  on primary). Use when working in a multi-repo container directory, starting or
  resuming cross-repo plans, writing umbrella tasks/todo.md, adding pointer
  stubs, scaffolding or vendoring skills into a child repo, seeding a second checkout
  for parallel work, or when the user mentions
  umbrella workspace, cross-repo handoff, multi-repo cooperation, worktree,
  feature umbrella, or parallel session.
  Slash: /umbrella-workspace.
---

# Umbrella workspace cooperation

An **umbrella workspace** is a directory that contains sibling git repos (children) but is usually **not** a git repo itself. Cross-repo work needs one place for plans and live status so agents and sessions can hand off without chat history.

This skill is **portable**. Each workspace keeps a thin project `AGENTS.md` (or equivalent) with: child-repo inventory, absolute path to **that** umbrella’s `tasks/todo.md`, and product-specific examples. Do not put those paths in this skill.

## Where things live

**Per child repo, always:**
- `tasks/` (`todo.md`, `archive.md`, `lessons.md`) — work scoped to that repo only
- `docs/` — that repo’s own architecture, features, contracts

Do **not** put single-repo work in the umbrella `tasks/` or `docs/`.

**Umbrella only for cross-repo concerns:**
- `docs/` — specs/plans whose scope spans **more than one** child repo (service splits, contract migrations, shared schema, multi-repo cutovers)
- `tasks/` (`todo.md`, `archive.md`) — live status for those same efforts (Tier 3 tracking, gates, approvals)

**Rule of thumb:** if the work is fully executable from one repo’s files alone, track it in that repo. If correct planning/execution needs **≥2 repos**, the plan and status live at the umbrella and reference children by path.

## Skill profiles across the workspace

Two sources: the **`$HOME` profile** for what applies regardless of stack, and **one project profile** for stack-specific skills. Don't re-declare a home skill in the project profile.

Not because it double-loads - it doesn't. Observed 2026-09-22, six names present in *both* the project and `$HOME` trees surfaced exactly once each, resolved project-over-home, on two tools independently (`grok inspect` labels the winner `project`; the other listed ten unique names from sixteen directories). The real costs are two: you store, commit and update a second copy, and **the project copy shadows the home one unconditionally**. While the two match, the duplication is invisible. The moment the project's pin lags a `$HOME` bump, the stale project copy is what loads - `sync --check` passes at both levels, because each is internally consistent, and nothing reports the shadowing. A skill that applies regardless of stack belongs at `$HOME` only.

**Every level that is a session root declares its own profile.** The umbrella declares what umbrella-rooted sessions need; each child declares what its own sessions and worktrees need. Both are ordinary consumers - `skf project init`, `add`, `sync`, `sync --check` - and neither is a view onto the other.

This follows from how skills are read. **Observed 2026-09-22 on Claude Code: a session reads only its own root.** A control skill placed in the session root's store loaded; two probes placed in a child directory's `.claude/skills` and `.agents/skills`, present on disk in the same scan, did not. So an umbrella-rooted session cannot see a child's skills, and a child-rooted session - including every `git worktree` of that child - cannot see the umbrella's. Neither level can serve the other, which is why both declare.

**Verify this for any other tool before relying on it.** Some resolve a directory-scoped copy against the files being edited. Guessing wrong fails *silently*: the profile is declared, `skf sync` vendors it, the lockfile is written, `sync --check` passes - and the skills never load. Probe it: a real skill at the session root as a control, another in a child directory, start a session, ask which it lists.

**A symlinked skill directory needs its own probe.** `readdirSync(dir, { withFileTypes: true })` reports a symlink as `isSymbolicLink()` and **not** `isDirectory()`, so a scan filtering on `isDirectory()` skips it while a `*/SKILL.md` glob resolves through it. Which idiom a tool uses is not inferable from the outside, and the failure is the silent kind again. Probe it: a real skill and a symlinked one side by side in a scratch directory, start a session there, ask which it lists - the real one is the control that proves the directory was read at all.

**The umbrella** declares every skill its own cross-repo sessions need - which in practice is the union of what the children's work touches, because that is the work being done from there.

**Each child** declares what its own signals justify, and *commits* it. A worktree materializes the child's tracked content, so committed skills reach every worker checkout for free; an umbrella-held profile never does, sitting one level above and untracked besides.

```bash
# at the umbrella, and again in each child
skf project init
skf project add <name>...
skf sync
skf sync --check            # must exit 0 - run it at every level
```

### Fallback: umbrella links into children

Reach for this **only when the umbrella cannot hold a profile of its own** - a container directory nobody wants to add a manifest to. The umbrella carries no `skill-forge.json`; its skill directories hold symlinks into the children instead.

It is a fallback rather than the shape because of three costs the declared umbrella does not pay:

- **Collisions have no answer.** Two children pinning the same skill at different ranges leave one link slot and two candidates. Whichever you pick is silently wrong for the other child.
- **Nothing validates it.** `skf sync` refuses to run where there is no `skill-forge.json`, so there is no `sync --check` at the umbrella. A child that gains a skill leaves the link simply absent, the child's own check still passes, and nothing reports the gap.
- **The links are hand-maintained and, at a container-only umbrella, unversioned.** A lost container takes them with it.

That `sync` refusal cuts both ways, and it is why this layout is safe as far as it goes: nothing at the umbrella prunes or overwrites the links. Putting a profile there *and* keeping links fires both destructive paths - a declared name is removed and re-copied as a real directory, an undeclared one is pruned by name. Choose one or the other, never both.

Re-link after any child profile change:

```bash
cd <umbrella>
# Only the real store is linked per skill. `.claude/skills` is itself a shim
# symlink to `.agents/skills`, so linking into it would write inside the store.
mkdir -p .agents/skills
[ -e .claude/skills ] || ln -s ../.agents/skills .claude/skills
for c in <child>...; do
  for d in "$c"/.agents/skills/*/; do
    [ -d "$d" ] || continue
    n=$(basename "$d")
    ln -sfn "../../$c/.agents/skills/$n" ".agents/skills/$n"
  done
done
```

**Declaring the same skill at both levels is correct, not duplication.** The umbrella and a child are different session roots, so their stores are never read by the same session - there is no second copy to surface and no precedence to resolve. (Dedupe *within* one root is measured, above; that two roots never interact in one session follows from the root-only reading and has not been separately measured.)

The hazard is not duplication but **drift between the two pins**. The umbrella at `^0.3.0` and a child at `^0.5.0` means the same skill behaves differently depending on which directory the session started in, and each level's `sync --check` passes because each is internally consistent. Keep the pins aligned deliberately, and treat a level you have not synced in a while as serving stale skills until proven otherwise.

**A child whose signals `$HOME` already covers needs nothing.** A stub with no code yet is not a consumer - revisit when it becomes one.

**When the profile lives at an umbrella that is not a git repo, it is unversioned.** Children commit their own; an umbrella that is only a container commits nothing. Record which skill serves which child in the umbrella's `AGENTS.md`: that table plus `skf sync` is the profile's only durable record, and it is what rebuilds the workspace after the container is lost.

**A child's own local skill reaches the umbrella by symlink, not by copy.** A repo-local overlay (`skills.local`) has its source inside the child that owns it, and that child is the only place it can be edited and committed. An umbrella-rooted session still needs to see it, so link down rather than duplicating:

```bash
# from the umbrella, pointing into the child that owns the source.
# One link, into the real store: `.claude/skills` is a shim to `.agents/skills`,
# so it resolves the same name without a second link.
# A relative target resolves from the link's own directory, not the umbrella root.
ln -s ../../<child>/.agents/skills/<name> .agents/skills/<name>
```

`skf sync` leaves these alone - it vendors registry skills and prunes by registry name, so a link named after a local skill is neither created nor removed for you. Where the umbrella holds the profile, declare the child's real path in its `skills.local` so `integrity` tracks the source; under the child-owned layout the umbrella declares nothing and the child's own lock already tracks it.

**Put the local skill's source in the child's real store**, `<child>/.agents/skills/<name>`, and declare that path in `skills.local`. Every tool directory is a shim onto that store, so no second link is needed inside the child, and one relink loop at the umbrella picks up registry and local skills together with nothing to special-case. A copy instead of a link is the worse failure: nothing refreshes it, and the two drift silently while `sync --check` passes.

Umbrella-level links are unversioned when the umbrella is not a git repo - the child commits the source and its own links, the umbrella commits nothing. Note them in the umbrella `AGENTS.md` beside the skill table, or a lost container takes them with it.

**A newly vendored skill loads on the next session, not the current one.** The skill list is read at session start, so a profile changed mid-session is a handoff note, not a live change.

Observed 2026-09-02: skills were declared and vendored per child on the assumption that the tool would resolve them by directory. It read only the session root, so for a full working session five skills were installed, in sync, and never loaded - including the UI skill for the frontend work being done at the time.

## Task entries must be self-contained

Agents and later sessions have **no memory** of the conversation that created a task — only what is written in `tasks/`. Every umbrella (and per-repo) entry must be resumable cold:

1. **Goal** — one sentence, what “done” means
2. **Spec reference** — exact doc path (+ section/heading if needed), not “the plan we discussed”
3. **Key files** — exact paths (and line anchors when useful)
4. **Current status** — not started / in progress / blocked / complete, **dated**; say what’s done and what’s left
5. **Dependencies** — which other tasks/phases must be green first (explicit, not list order)
6. **Gate / acceptance check** — exact commands or manual checks for **this** slice
7. **Blockers, if any** — specific open question; never leave “blocked” without on what

**Multi-phase plans:** one task section per phase/milestone, not one blob for the whole initiative.

## Discoverability: pointer stubs in affected repos

When a cross-repo plan will modify a child repo, add a **pointer stub** to that repo’s `tasks/todo.md` so an agent opened **only** in the child still finds the work:

```markdown
## [Cross-repo] <plan title> — see umbrella tasks/todo.md
**Status:** tracked at umbrella level, not here.
This repo is a participant in a cross-repo plan: `<path to plan doc from umbrella or relative>`.
Live status/phase tracking: `<absolute path to umbrella tasks/todo.md>`.
Do not duplicate status here — update it at the umbrella level only.
```

**Single source of truth:** status lives **only** in umbrella `tasks/todo.md`. Stubs link; they never carry their own status (avoids drift).

## Tracking `tasks/`: stub committed, ledger ignored

`tasks/` holds two different things and they have opposite tracking rules. Split them when you
author a repo or seed a worktree — retrofitting after a worker has run is the expensive order.

| File | Tracked? | Why |
|---|---|---|
| Pointer stub (`tasks/todo.md` in a participant child) | **Committed** | Its whole job is discoverability for an agent opened only in that child. A gitignored stub cannot do that job. Static: no status, no specs. |
| Live ledger (`todo.md` status blocks, `archive.md`, `notes.md`, `lessons.md`) | **Gitignored** | Untracked files do not travel with a branch checkout, so ignoring keeps the ledger out of every worktree and every merge — structurally, not by discipline. |

A repo that participates in an umbrella **and** keeps local tasks cannot use one file for both.
Keep the stub at `tasks/todo.md` and put local tracking under the ignored paths.

```gitignore
# Live ledger: never travels with a branch. Pointer stub stays tracked.
/tasks/*
!/tasks/todo.md   # stub only — status lives at the umbrella
```

A repo with no umbrella stub ignores the directory outright (`/tasks/`).

**The commit absorbs what the ledger cannot keep.** An untracked ledger has no history and no audit
trail for anyone else — accept that for status narration, which the diff and commit message already
tell better. Do not accept it for authorizations, which git cannot reconstruct. When an intent
commit involved any of these, the **commit message** carries them:

- Tier 3 spec approval (that it was given, and for what scope)
- Gate waiver text and timestamp
- `Delegation: external-worker`, when a worker produced the change
- Worker branches rejected along the way — `--remove` deletes them and no commit holds them otherwise

Skip this and gitignoring the ledger deletes the only record that the work was authorized.

**Run bookkeeping goes the other way.** Worker cost, iteration counts, and wall-clock belong in the
ledger's task block, never in the commit message — they describe a run, not the code, and they are
only meaningful next to the primary-token figure the task block already tracks. Accept that they
die with the ledger.

## Parallel sessions (git worktrees)

When **two sessions** would edit the **same child repo** at once:

- A **branch** is not enough if both share one working directory.
- Prefer **`git worktree add`** for each **git** child you will change (same branch name across related children).
- Prefer a **thin feature umbrella** (sibling folder, not a git repo) that *contains* those worktrees; open that folder as the session root.
- Distinguish **git worktrees** from **folder copies**. Cleanup must remove both.
- Open each session on the **correct root** only (primary vs feature umbrella).
- Soft ownership: feature-umbrella **`SESSION.md`** + child **`AGENTS.md`** hook: *if `SESSION.md` exists, read it at session start*.
- Feature-specific absolute paths belong in the **plan** / `SESSION.md`, not in this skill.
- When the feature is done: handoff on the **primary** umbrella, merge **code**, then remove worktrees and leftover copies. Do not merge gitignored runtime data unless asked.

**Full procedure:** companion **`PARALLEL-WORKTREES.md`** (load when seeding, retiring, or the user mentions worktree / feature umbrella / parallel session).

## Handoff checklist (end of session on cross-repo work)

- [ ] Umbrella `tasks/todo.md` phase entry matches reality (not stale)
- [ ] Every repo touched has a pointer stub (add once; don’t re-add)
- [ ] Completed phases: status complete + gate evidence, or moved to `tasks/archive.md` with `**Archived:** YYYY-MM-DD`
- [ ] Spec divergences written back into the plan doc, not only in chat
- [ ] If this session used a worktree: `SESSION.md` still accurate; do not leave the other session editing the same tree
- [ ] If retiring a feature umbrella: cold resume lives on the **primary** (`HANDOFF.md` or equivalent + primary `tasks/todo.md`); feature-umbrella `SESSION.md` redirects; do not continue there

## Cross-repo plan implementer protocol

Plan docs keep **only** plan-specific key files, phase gates, and restricted ops. Cooperation mechanics are this skill + the workspace `AGENTS.md` inventory:

1. **Status** — umbrella `tasks/todo.md`, one section per phase; archive completed phases
2. **Discoverability** — pointer stubs in each affected child
3. **Self-contained entries** — seven fields above
4. **Gates** — project/global gate rules after every implementation batch; evidence in the phase section
5. **Divergence** — stop, update the plan doc, re-approve if material (Tier 3)
6. **Restricted ops** — explicit user confirmation for `git init`, deletes, commit/push, prod deploy, destructive commands
7. **Parallel sessions** — if another agent owns the primary checkout, use worktrees per `PARALLEL-WORKTREES.md`; do not fight over one folder

## Scaffold (new umbrella)

```text
umbrella/                 # usually not a git repo
  AGENTS.md               # child inventory + path to this umbrella’s tasks/todo.md
  CLAUDE.md               # @AGENTS.md (if using Claude)
  docs/                   # multi-repo plans only
  tasks/                  # live ledger; gitignored in any child that carries one
    todo.md
    archive.md
  child-a/                # each child its own git repo when ready
    AGENTS.md
    tasks/todo.md         # pointer stub when participating — committed, status-free
    skill-forge.json      # this child's own skills; committed, so worktrees get them.
                          #   Absent only when $HOME already covers everything it needs
    skill-forge.lock.json
    .agents/skills/       # the one real store, vendored by `skf sync`; never hand-copied
    .claude/skills        # symlink -> ../.agents/skills, written by `skf sync`
  child-b/
    ...
```

## Anti-patterns

- Assuming an agent tool loads skills from child directories without confirming it - the failure is silent, and `sync --check` passes either way
- Declaring a stack-specific skill at `$HOME` so "every repo has it", when one project profile would do
- Adding a child repo without deciding which skills it needs, leaving it silently on the home baseline
- Hand-copying a vendored skill directory between children instead of declaring it in that child's profile
- Leaving a session root without its own profile on the theory that a neighbouring level covers it - a session reads only its own root
- Letting an umbrella pin and a child pin of the same skill drift apart; each level's `sync --check` passes while they disagree
- Linking a skill into `.claude/skills` by name - it is a shim onto `.agents/skills`, so the link lands inside the store
- Copying a child's local overlay up to the umbrella instead of symlinking it - the copy never refreshes and `sync --check` still passes
- Splitting a profile's `skill-forge.json`, lockfile and vendored skill directories across separate commits, where they are committed at all
- Duplicating live status in both umbrella and child `todo.md`
- One giant “in progress” section for a multi-phase plan
- Plan-only docs with no umbrella task tracking for Tier 3 work
- Putting single-repo bugs/features in umbrella `tasks/`
- Committing a live ledger, or gitignoring a pointer stub (each defeats the other's purpose)
- Assuming the next agent has chat context
- Two sessions editing the same child **folder** on different branches (or the same)
- Putting ephemeral worktree absolute paths into this portable skill or into long-lived AGENTS as the only SSOT
- Relying on `SESSION.md` without an AGENTS (or user) instruction to **read** it
- Treating a nested folder copy (deploy, vendor) as a `git worktree`
- Merging worktree SQLite / uploads / published assets as if they were source
- Continuing work in a feature umbrella after the retire handoff is on the primary

## Related

- Global agent process: risk tiers, mandatory gates, `tasks/` format (§13 style) — already in core instructions
- `skill-forge-project` - choosing and applying a child's skill set; this skill covers only where profiles belong in a multi-repo layout
- Companion: `PARALLEL-WORKTREES.md` — seed, thin feature umbrella, worktree vs copy, one DB file per tree, retire on primary, cleanup
