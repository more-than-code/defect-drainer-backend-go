package jobs

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joe/defect-drainer-go/internal/git"
)

// skillDirs are the per-repo skill directories scanned for a worktree stanza.
// ~/.grok/skills is retired and is not scanned; ~/.agents/skills is a footnote,
// not a scan — DD teaches paths, it does not vendor skills (K11).
var skillDirs = []string{".agents/skills", ".claude/skills", ".grok/skills"}

// frontmatterMaxLines bounds the hand-rolled scanner so a pathological
// SKILL.md cannot turn brief writing into a file read of unbounded size.
const frontmatterMaxLines = 20

// processMarkdown is DD-owned and deliberately fixed: it records only the
// mappings a general process file cannot know about a headless job. It must
// not paraphrase Skill Forge's core.md — a copy drifts from the registry (K2).
const processMarkdown = "# Worker contract (Defect Drainer job)\n" + `
You are the **worker** for this job. This file + BRIEF.md outrank any
per-turn instruction that contradicts them. Do not delegate onward.
Do not spawn another coding-agent CLI. Do not run dispatch.sh.

## Approval already given
The operator started this batch. That is spec approval. Do not wait
for "Approve this spec?". Write the short plan in NOTES.md and proceed.

## Scope
Fix only the defect ids listed in BRIEF.md. If a fix needs a change
outside those defects or outside listed worktrees: stop, record it in
NOTES.md, do not edit extra files.

## Skills
1. Read each worktree contract file listed in BRIEF.md BEFORE editing
   that worktree. Where the repo is stricter than this file, the repo wins.
2. Activate skills from SKILLS.md (absolute paths). Do not rely on cwd
   discovery — cwd is the job handoff, not the product repo.
3. Default implementation set when the repo does not name one:
   security-baseline, coding-discipline, code-quality, plus
   testing-strategy (DD adds this because the job is a bug fix;
   Skill Forge core's default trio does not include it).
4. Flutter worktrees: fvm-flutter (the toolchain is already provisioned;
   do not clone a second SDK).

## Bug-fix sequence (per defect)
1. Explore the listed evidence and the worktree code.
2. Write or update a reproducer test first when the repo has a test
   suite the defect can hit. If you cannot, say why in fix-notes.
3. Fix the root cause only (no drive-by format/refactors).
4. Run the VERIFICATION commands listed in the spawn prompt yourself
   inside the worktree. DD will re-run them; only DD's result counts.
5. Answer every acceptance criterion in fix-notes/<DEF>.md.

## Git
- Commit in the worktree branch already checked out
  (defect-drainer/<BATCH-id>). One intent per repo.
- Do not push. Do not commit on main/master. Do not edit primaries.
- Do not add BRIEF.md / PROCESS.md / NOTES.md / review/ to product commits.

## Forbidden
- Inventing test results you did not observe
- Authoring verification commands for DD to run
- Writing tasks/todo.md (use NOTES.md)
- Expanding the batch
`

// briefSafe strips the characters that would break the markdown fence or forge
// a line, the same concern as inventoryEvidencePaths. Frontmatter is repo
// content, so it is data, not instructions.
func briefSafe(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\x00', '\n', '\r', '`':
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > limit {
		s = strings.TrimRight(s[:limit], " ") + "…"
	}
	return s
}

func unquoteScalar(v string) string {
	if len(v) >= 2 {
		if (v[0] == '\'' && v[len(v)-1] == '\'') || (v[0] == '"' && v[len(v)-1] == '"') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// isBlockMarker reports a YAML block scalar header — `|` or `>`, with the
// optional chomping and indent indicators (`|-`, `>+`, `>2`). Anything else
// after the marker means it is ordinary text, not a block.
func isBlockMarker(v string) bool {
	if v == "" || (v[0] != '|' && v[0] != '>') {
		return false
	}
	return strings.Trim(v[1:], "-+0123456789") == ""
}

// foldBlock joins a block scalar's indented continuation into one line. It
// stops at the first non-indented line, which is the next key or the end of
// the frontmatter. Blank lines are paragraph breaks in YAML; one line is all
// we want, so they collapse to the joining space.
func foldBlock(lines []string, start int) string {
	var parts []string
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line == strings.TrimLeft(line, " \t") {
			break
		}
		parts = append(parts, strings.TrimSpace(line))
	}
	return strings.Join(parts, " ")
}

// skillFrontmatter reads name/description from a SKILL.md without a YAML
// module (go.mod stays sqlite-only, and a full parser is a large hazard
// surface for what is effectively untrusted repo content). Requires a leading
// `---`, then reads at most frontmatterMaxLines until the closing `---`.
// Values may be single-line scalars or a `|` / `>` block folded into one line.
// Nested maps and every other key are ignored.
// ok is false when the file has no frontmatter or no usable name — the caller
// lists it as unreadable and carries on rather than failing the job.
func skillFrontmatter(path string) (name, description string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", "", false
	}
	// Collect once so a block scalar can look ahead by index. The cap bounds
	// the read and any pathological continuation in the same stroke.
	var lines []string
	for i := 0; i < frontmatterMaxLines && sc.Scan(); i++ {
		if strings.TrimSpace(sc.Text()) == "---" {
			break
		}
		lines = append(lines, sc.Text())
	}
	for i, line := range lines {
		// Indented lines belong to a nested map, or to a block already folded
		// by the key that introduced it.
		if line != strings.TrimLeft(line, " \t") {
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		val = strings.TrimSpace(val)
		if isBlockMarker(val) {
			val = foldBlock(lines, i+1)
		}
		if val == "" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = briefSafe(unquoteScalar(val), 120)
		case "description":
			description = briefSafe(unquoteScalar(val), 300)
		}
	}
	return name, description, name != ""
}

type skillEntry struct {
	name        string
	description string
	path        string
	readable    bool
}

// skillsIn globs one worktree's skill directories. De-dupe is by name and
// within this stanza only: two worktrees may vendor the same skill at
// different paths, and printing one path would teach the worker the wrong repo.
func skillsIn(worktreeAbs string) []skillEntry {
	var out []skillEntry
	seen := map[string]bool{}
	for _, dir := range skillDirs {
		matches, err := filepath.Glob(filepath.Join(worktreeAbs, filepath.FromSlash(dir), "*", "SKILL.md"))
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, m := range matches {
			name, desc, ok := skillFrontmatter(m)
			if !ok {
				out = append(out, skillEntry{path: m})
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, skillEntry{name: name, description: desc, path: m, readable: true})
		}
	}
	return out
}

// buildSkillsMarkdown emits one stanza per worktree with absolute paths and
// names only. Bodies are never copied (K11).
func buildSkillsMarkdown(worktrees []git.WorktreeBinding) string {
	lines := []string{
		"# Skills and contracts for this job",
		"",
		"cwd is the job handoff, not a product repo, so directory-based skill",
		"discovery does not fire. Use these absolute paths.",
		"",
	}
	if len(worktrees) == 0 {
		lines = append(lines, "**No worktrees in this job — do not edit product code.**", "")
		return strings.Join(lines, "\n")
	}
	for _, w := range worktrees {
		lines = append(lines, "## "+w.Repo+" (`"+w.WorktreeAbs+"`)")
		contracts := contractFilesIn(w.WorktreeAbs)
		if len(contracts) > 0 {
			quoted := make([]string, 0, len(contracts))
			for _, f := range contracts {
				quoted = append(quoted, "`"+filepath.Join(w.WorktreeAbs, f)+"`")
			}
			lines = append(lines, "- contract: "+strings.Join(quoted, ", "))
		} else {
			lines = append(lines, "- contract: (none found — follow BRIEF.md and PROCESS.md)")
		}
		entries := skillsIn(w.WorktreeAbs)
		if len(entries) == 0 {
			lines = append(lines,
				"- skills: (none vendored in this worktree — use the PROCESS.md default set)",
				"")
			continue
		}
		lines = append(lines, "- skills (read SKILL.md only after the name matches the defect):")
		for _, e := range entries {
			if !e.readable {
				lines = append(lines, "  - (unreadable frontmatter) — `"+e.path+"`")
				continue
			}
			item := "  - `" + e.name + "` — `" + e.path + "`"
			if e.description != "" {
				item += " — " + e.description
			}
			lines = append(lines, item)
		}
		lines = append(lines, "")
	}
	lines = append(lines,
		"Home-profile skills at `~/.agents/skills` also apply; do not activate two copies of one name.",
		"")
	return strings.Join(lines, "\n")
}
