package git

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// StartCodingAgent execs the resolved bin. Empty worktrees refuse spawn.
func StartCodingAgent(handoffAbs, batchID, defectsRoot string, worktrees []WorktreeBinding) (*exec.Cmd, error) {
	if len(worktrees) == 0 {
		return nil, fmt.Errorf("refusing to spawn agent batch fix without worktrees")
	}
	bin := ResolveGrokBin()
	if bin == "" {
		return nil, fmt.Errorf("coding agent binary not found")
	}
	var lines []string
	for _, w := range worktrees {
		lines = append(lines, "- "+w.Repo+": EDIT ONLY "+w.WorktreeAbs)
	}
	prompt := strings.Join([]string{
		"Batch defect fix session for " + batchID + ".",
		"Handoff directory (cwd): " + handoffAbs,
		"Inventory root: " + defectsRoot,
		"WORKTREE ENFORCEMENT:",
		strings.Join(lines, "\n"),
		"Read BRIEF.md first. Reply: DONE " + batchID,
	}, "\n")
	maxTurns := os.Getenv("DEFECT_DRAINER_GROK_BATCH_MAX_TURNS")
	if maxTurns == "" {
		maxTurns = os.Getenv("GROK_BATCH_MAX_TURNS")
	}
	if maxTurns == "" {
		maxTurns = "80"
	}
	args := []string{
		"-p", prompt,
		"--cwd", handoffAbs,
		"--sandbox", "strict",
		"--always-approve",
		"--max-turns", maxTurns,
		"--output-format", "plain",
		"--verbatim",
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = handoffAbs
	wtPaths := make([]string, 0, len(worktrees))
	for _, w := range worktrees {
		wtPaths = append(wtPaths, w.WorktreeAbs)
	}
	cmd.Env = append(os.Environ(),
		"CI=1",
		"GROK_SANDBOX=strict",
		"DEFECT_DRAINER_WORKTREES="+strings.Join(wtPaths, ":"),
	)
	logf, err := os.OpenFile(handoffAbs+"/agent.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}
