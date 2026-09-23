#!/usr/bin/env sh
# Dispatch an external worker: isolated worktree, verified brief, role marked.
#
#   dispatch.sh <task-slug> [--allow-dirty] -- <worker-cli> [args...]   dispatch
#   dispatch.sh <task-slug> --remove [--force]                        tear down
#
# First dispatch scaffolds the worktree and stops so you can write the brief.
# Second run verifies the brief and execs the worker with its role set.
#
# The role marker exists because both ends run the same shared instructions and a
# worker cannot otherwise tell which end of the delegation it is on. Setting it
# here rather than by hand is the point: a flag you must remember is a flag you
# will forget.

set -eu

usage() {
  echo "usage: dispatch.sh <task-slug> [--allow-dirty] -- <worker-cli> [args...]" >&2
  echo "       dispatch.sh <task-slug> --remove [--force]" >&2
  exit 64
}

[ $# -ge 1 ] || usage
slug=$1
shift
case $slug in -*|'') usage ;; esac

allow_dirty=0
if [ "${1:-}" = "--allow-dirty" ]; then
  allow_dirty=1
  shift
fi

root=$(git rev-parse --show-toplevel 2>/dev/null) || {
  echo "dispatch.sh: not a git repository" >&2
  exit 69
}
# A linked worktree means this is already a worker checkout; dispatching from one
# would nest delegations, which the no-recursion rule forbids.
if [ "$(git rev-parse --git-dir)" != "$(git rev-parse --git-common-dir)" ]; then
  echo "dispatch.sh: refusing to dispatch from a linked worktree ($root)" >&2
  exit 69
fi

tree="$root/../work-$slug"
branch="worker/$slug"

# Teardown. The orchestrator created the worktree, so the orchestrator removes it —
# and in this order: git refuses to delete a branch while a worktree holds it, and
# deleting the directory alone leaves a prunable stale entry behind.
if [ "${1:-}" = "--remove" ]; then
  # Check mergedness first: git will not delete a branch a worktree holds, so a
  # naive remove-then-delete strands the branch after the worktree is already gone.
  if [ "${2:-}" != "--force" ] && git show-ref --verify --quiet "refs/heads/$branch"; then
    git merge-base --is-ancestor "$branch" HEAD 2>/dev/null || {
      echo "dispatch.sh: $branch is not merged into HEAD; re-run with --force to discard it" >&2
      exit 65
    }
  fi
  # A branch with no commits trivially satisfies the ancestor check above, so a worker
  # that produced everything and committed nothing looks merged. `git worktree remove
  # --force` would then delete work that exists only in the working tree.
  if [ -n "$(git -C "$tree" status --porcelain 2>/dev/null)" ] && [ "${2:-}" != "--force" ]; then
    echo "dispatch.sh: $tree has uncommitted changes; commit or discard them, or re-run with --force" >&2
    git -C "$tree" status --short >&2
    exit 65
  fi

  # Last chance to lift the worker's cost: the run logs are the only place it exists
  # and the next line deletes them. One `end` record per iteration; sum them. Printed rather
  # than enforced — record it in the orchestrator's ledger (the main checkout's
  # tasks/), not in the commit message: it is operational bookkeeping, not history.
  if ls "$tree"/run*.jsonl >/dev/null 2>&1; then
    # Every round, not just the first: a resumed run appends here, and a round driven
    # into its own run2.jsonl would otherwise be torn down uncounted. `grep -h` because
    # a filename prefix would land in awk's second field and silently sum to zero.
    records=$(grep -ho '"total_cost_usd":[0-9.]*' "$tree"/run*.jsonl || true)
    cost=$(printf '%s\n' "$records" | awk -F: '{ sum += $2 } END { printf "%.4f", sum }')
    iters=$(printf '%s\n' "$records" | grep -c . || true)
    echo "Worker cost before teardown: \$$cost over $iters iteration(s) — record it in the main checkout's tasks/ ledger."
  fi
  [ -d "$tree" ] && git worktree remove --force "$tree"
  git show-ref --verify --quiet "refs/heads/$branch" && git branch -D "$branch"
  echo "Removed $tree and branch $branch."
  exit 0
fi

[ "${1:-}" = "--" ] || usage
shift
[ $# -ge 1 ] || usage

# Orchestrator-side files live inside the worktree, so a worker's `git add -A` would
# commit them onto the branch and the merge would carry them into the main branch.
# info/exclude is resolved from the common git dir (a per-worktree copy is ignored),
# it is never committed, and it cannot affect a file the repo already tracks — so a
# committed pointer stub at tasks/todo.md stays tracked and visible either way.
#
# The block is rewritten rather than appended-once, so a repo excluded by an older
# version of this script picks up entries added since.
exclude="$(git rev-parse --git-common-dir)/info/exclude"
mkdir -p "$(dirname "$exclude")"
if grep -q '^# >>> skill-forge worker >>>' "$exclude" 2>/dev/null; then
  sed '/^# >>> skill-forge worker >>>$/,/^# <<< skill-forge worker <<<$/d' "$exclude" > "$exclude.tmp"
  mv "$exclude.tmp" "$exclude"
fi
{
  echo '# >>> skill-forge worker >>>'
  echo 'BRIEF.md'
  echo 'NOTES*.md'
  echo 'tasks/'
  echo 'run*.jsonl'
  echo 'driver*.log'
  echo '# <<< skill-forge worker <<<'
} >> "$exclude"

if [ ! -d "$tree" ]; then
  # A worktree checks out committed content only, so anything uncommitted is absent
  # from what the worker sees. Untracked files are the silent case: the checkout
  # succeeds, the worker gets a codebase the brief was not written against, and the
  # first error it hits looks like its own. Modified tracked files at least arrive in
  # their committed form, and the delta is recoverable — those only warn.
  # Both lists are post-exclude, so this script's own BRIEF.md/tasks/ block is skipped.
  untracked=$(git status --porcelain | grep '^??' | cut -c4- || true)
  if [ -n "$untracked" ] && [ "$allow_dirty" -eq 0 ]; then
    echo "dispatch.sh: these new files are uncommitted and would be missing from $tree:" >&2
    printf '%s\n' "$untracked" | sed 's/^/  /' >&2
    echo "Commit them, or re-run with --allow-dirty if the brief does not reference them." >&2
    echo "If they cannot be committed, do not dispatch a worktree — see the dirty-tree" >&2
    echo "fallback in the external-worker-delegation skill." >&2
    exit 65
  fi
  modified=$(git status --porcelain | grep -v '^??' | cut -c4- || true)
  if [ -n "$modified" ]; then
    echo "dispatch.sh: warning — $tree checks out HEAD; these local edits are not in it:" >&2
    printf '%s\n' "$modified" | sed 's/^/  /' >&2
  fi

  git worktree add "$tree" -b "$branch"
  echo
  echo "Worktree ready: $tree"
  echo "Write $tree/BRIEF.md, then re-run this command to dispatch."
  echo "Required first section: '## Role' — the worker executes this brief and does not delegate onward."
  exit 0
fi

brief="$tree/BRIEF.md"
[ -f "$brief" ] || {
  echo "dispatch.sh: $brief is missing; the worker starts cold and needs it" >&2
  exit 66
}
# The Role section is the guarantee the env var only makes machine-readable.
grep -qi '^#\{1,\}[[:space:]]*Role\b' "$brief" || {
  echo "dispatch.sh: $brief has no '## Role' section; add it before dispatching" >&2
  exit 65
}

cd "$tree"
SKILL_FORGE_AGENT_ROLE=worker
export SKILL_FORGE_AGENT_ROLE
exec "$@"
