package jobs

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

// CONTRACT_FILES are repo-owned rules the brief tells the agent to read first.
// Port of backend/src/batches.ts CONTRACT_FILES.
var contractFiles = []string{"AGENTS.md", "CLAUDE.md", "CONTRIBUTING.md"}

var (
	acceptanceHeadingRe = regexp.MustCompile(`(?i)^#{1,6}\s+acceptance\b`)
	nextHeadingRe       = regexp.MustCompile(`^#{1,6}\s`)
	bulletPrefixRe      = regexp.MustCompile(`^[-*+]\s+`)
	numberedPrefixRe    = regexp.MustCompile(`^\d+[.)]\s+`)
	checkboxPrefixRe    = regexp.MustCompile(`^\[[ xX]\]\s*`)
)

// batchSQLitePath is the TS writeBatchManifest path: sqlite:batches/<id>.
func batchSQLitePath(id string) string {
	return "sqlite:batches/" + id
}

func contractFilesIn(worktreeAbs string) []string {
	var out []string
	for _, f := range contractFiles {
		if _, err := os.Stat(filepath.Join(worktreeAbs, f)); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// inventoryEvidencePaths returns a cleaned slash-rel (from defectsRoot) and
// the absolute path only when rel is a well-formed path under
// {defectsRoot}/evidence. Values containing NUL, newline, CR, or backtick
// are rejected so they cannot break the markdown fence or forge an abs line.
func inventoryEvidencePaths(defectsRoot, rel string) (cleanRel, abs string) {
	if rel == "" || strings.ContainsAny(rel, "\x00\n\r`") {
		return "", ""
	}
	norm := strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/")
	joined := filepath.Join(defectsRoot, filepath.FromSlash(norm))
	// Confine to {defectsRoot}/evidence, not the whole inventory root — a
	// crafted rel can otherwise name job.json / the sqlite file. Same Rel /
	// ".." shape as underRoot, but on cleaned paths so a missing evidence
	// file is still listed. Do not switch this site to underRoot: EvalSymlinks
	// the root on macOS /var/folders rejects every not-yet-created path.
	cleanRoot := filepath.Clean(filepath.Join(defectsRoot, "evidence"))
	cleanJoin := filepath.Clean(joined)
	r, err := filepath.Rel(cleanRoot, cleanJoin)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", ""
	}
	fromDef, err := filepath.Rel(filepath.Clean(defectsRoot), cleanJoin)
	if err != nil || fromDef == ".." || strings.HasPrefix(fromDef, ".."+string(filepath.Separator)) {
		return "", ""
	}
	return filepath.ToSlash(fromDef), cleanJoin
}

func evidenceBriefItems(defectsRoot string, rels []string) []string {
	var out []string
	for _, e := range rels {
		rel, abs := inventoryEvidencePaths(defectsRoot, e)
		if abs == "" {
			continue
		}
		out = append(out, "  - rel: `"+rel+"`", "  - abs: `"+abs+"`")
	}
	return out
}

// AcceptanceCriteria pulls the ## Acceptance checklist out of a defect body.
// Port of backend/src/batches.ts acceptanceCriteria — same heading, strip, and
// "next heading ends the block" rules.
func AcceptanceCriteria(body string) []string {
	if body == "" {
		return nil
	}
	lines := strings.Split(body, "\n")
	start := -1
	for i, l := range lines {
		if acceptanceHeadingRe.MatchString(strings.TrimSpace(l)) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var out []string
	for _, raw := range lines[start+1:] {
		line := strings.TrimSpace(raw)
		if nextHeadingRe.MatchString(line) {
			break
		}
		if line == "" {
			continue
		}
		item := bulletPrefixRe.ReplaceAllString(line, "")
		item = numberedPrefixRe.ReplaceAllString(item, "")
		item = checkboxPrefixRe.ReplaceAllString(item, "")
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// BatchFixBriefInput is the TS buildBatchFixBrief argument.
type BatchFixBriefInput struct {
	Batch       BatchRecord
	Defects     []store.DefectRecord
	DefectsRoot string
	GrokSandbox string
	Worktrees   []git.WorktreeBinding
}

// BuildBatchFixBrief ports backend/src/batches.ts buildBatchFixBrief.
func BuildBatchFixBrief(input BatchFixBriefInput) string {
	sandbox := "strict"
	if input.GrokSandbox == "workspace" {
		sandbox = "workspace"
	}
	var lines []string
	lines = append(lines,
		"# Batch agent fix — "+input.Batch.ID,
		"",
		"## Goal",
		input.Batch.Goal,
		"",
		"## App",
		"`"+input.Batch.AppID+"`",
		"",
		"## Agent sandbox (App Settings)",
	)
	if sandbox == "workspace" {
		lines = append(lines,
			"- Profile: **workspace** — host reads allowed (iOS Simulator / simctl).",
			"- Writes only under handoff cwd + temp. Put post-fix screenshots in `fix-evidence/<DEF-id>/fix-01.png`.",
		)
	} else {
		lines = append(lines,
			"- Profile: **strict** (restrict) — no reliable Simulator access.",
			"- Code-only in worktrees; operator attaches fix_evidence in console if needed.",
		)
	}
	lines = append(lines,
		"",
		"## HARD isolation (enforced by operator — do not violate)",
		"- Product code edits are **only** allowed inside the **worktree paths** listed below.",
		"- An app may have **multiple repos**; decide which worktree(s) need changes for each defect.",
		"- **Never** edit primary checkouts (the \"primary\" paths). Other agents may own those trees.",
		"- Branch name for this batch: use the worktree branch already checked out (do not switch primary).",
		"- Inventory SSOT is SQLite + evidence files under `"+input.DefectsRoot+"/evidence`.",
		"- **Read** report + historical fix evidence from inventory (paths listed per defect below).",
		"- **Write** new fix proof under handoff `fix-evidence/` (this job only); backend harvests onto the defect after the run.",
		"",
	)
	if len(input.Worktrees) > 0 {
		lines = append(lines, "## Worktrees (edit ONLY these product paths)", "")
		for _, w := range input.Worktrees {
			lines = append(lines,
				"### "+w.Repo,
				"- **worktree (EDIT HERE):** `"+w.WorktreeAbs+"`",
				"- **branch:** `"+w.Branch+"`",
				"- **primary (DO NOT EDIT):** `"+w.PrimaryAbs+"`",
			)
			contracts := contractFilesIn(w.WorktreeAbs)
			if len(contracts) > 0 {
				quoted := make([]string, 0, len(contracts))
				for _, f := range contracts {
					quoted = append(quoted, "`"+filepath.Join(w.WorktreeAbs, f)+"`")
				}
				lines = append(lines,
					"- **read first — this repo's own rules:** "+strings.Join(quoted, ", "),
					"  They define that repo's mandatory gates, skills and conventions.",
					"  They bind you as much as this brief; where stricter, they win.",
				)
			}
			lines = append(lines, "")
		}
	} else {
		lines = append(lines,
			"## Worktrees",
			"",
			"**NONE — refuse to edit product code.** Worktree setup failed or was skipped.",
			"",
		)
	}
	lines = append(lines,
		"## Rules",
		"- Fix **only** the listed defects.",
		"- Prefer minimal, verified fixes with tests inside the worktree.",
		"- Use **historical fix evidence** (if listed) as context: prior proof of what “fixed” looked like; do not ignore regressions.",
		"",
		"## New fix evidence this run (REQUIRED — handoff paths, sandbox-writable)",
		"Backend imports these after the run onto the **defect** SSOT. Do **not** rely on writing inventory directly.",
		"",
		"For **each** defect id you claim fixed:",
		"1. Screenshots (png/jpg/webp): `fix-evidence/<DEF-id>/fix-01.png`, `fix-02.png`, …",
		"2. Short resolution text: `fix-notes/<DEF-id>.md` (what changed + how verified).",
		"",
		"Without **new** images under this job's `fix-evidence/<DEF-id>/`, harvest will not update the defect for this run.",
		"",
		"## Defects ("+strconv.Itoa(len(input.Defects))+")",
		"",
	)
	for _, d := range input.Defects {
		lines = append(lines,
			"### "+d.ID,
			"- **title:** "+d.Title,
			"- **severity:** "+d.Severity,
			"- **status:** "+d.Status,
			"- **summary:** "+d.Summary,
		)
		if d.Surface != "" {
			lines = append(lines, "- **surface:** "+d.Surface)
		}
		if len(d.Repos) > 0 {
			lines = append(lines, "- **related repos:** "+strings.Join(d.Repos, ", "))
		}
		if d.Resolution != "" {
			lines = append(lines, "- **prior resolution note (historical):** "+d.Resolution)
		}
		acceptance := AcceptanceCriteria(d.Body)
		if len(acceptance) > 0 {
			lines = append(lines, "- **acceptance criteria (THE BAR — address each one in fix-notes):**")
			for _, a := range acceptance {
				lines = append(lines, "  - [ ] "+a)
			}
		}
		if ev := evidenceBriefItems(input.DefectsRoot, d.Evidence); len(ev) > 0 {
			lines = append(lines, "- **report evidence (intake — read for bug context):**")
			lines = append(lines, ev...)
		}
		if fx := evidenceBriefItems(input.DefectsRoot, d.FixEvidence); len(fx) > 0 {
			lines = append(lines, "- **historical fix evidence (owned by defect SSOT — read for prior proof / regressions):**")
			lines = append(lines, fx...)
		} else {
			lines = append(lines, "- **historical fix evidence:** (none yet — first fix run or never harvested)")
		}
		lines = append(lines,
			"- **deliver this run:** `fix-evidence/"+d.ID+"/fix-01.png` + `fix-notes/"+d.ID+".md`",
			"",
		)
	}
	lines = append(lines,
		"## Done",
		"When code fixes are in worktrees and **this job's** fix-evidence/notes exist for each fixed defect: reply `DONE "+input.Batch.ID+"`.",
		"",
	)
	return strings.Join(lines, "\n")
}

// WriteHandoffBrief writes BRIEF.md plus the sandbox-writable harvest dirs.
func WriteHandoffBrief(handoff string, input BatchFixBriefInput) error {
	if err := os.MkdirAll(filepath.Join(handoff, "fix-evidence"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(handoff, "fix-notes"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(handoff, "BRIEF.md"), []byte(BuildBatchFixBrief(input)), 0o644)
}
