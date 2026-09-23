package jobs

import (
	"os"
	"path/filepath"
	"strings"
)

// suggestShipMessage is the commit text shown before Merge.
// The subject is the defect title. The body is the agent's own fix summary
// from fix-notes (**Fix:**) or, if that file is absent, the "What was done"
// bullets in NOTES.md. Paths are not repeated; the dialog already lists them.
func suggestShipMessage(titles []string, handoff string) string {
	var subjects []string
	for _, title := range titles {
		line := firstSentence(title)
		if line != "" {
			subjects = append(subjects, line)
		}
	}
	subject := strings.Join(subjects, "; ")
	body := strings.Join(agentFixLines(handoff), "\n")
	if subject == "" {
		subject = body
		body = ""
	}
	if subject == "" {
		return ""
	}
	if body == "" || strings.EqualFold(body, subject) {
		return clipMessage(subject)
	}
	return clipMessage(subject + "\n\n" + body)
}

func agentFixLines(handoff string) []string {
	if handoff == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(handoff, "fix-notes", "*.md"))
	var lines []string
	for _, path := range matches {
		if line := markdownLabeled(path, "Fix"); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > 0 {
		return lines
	}
	return notesWhatWasDone(filepath.Join(handoff, "NOTES.md"))
}

func markdownLabeled(path, label string) string {
	text, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := "**" + label + ":**"
	for _, line := range strings.Split(string(text), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return plainMarkdown(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		}
	}
	return ""
}

func notesWhatWasDone(path string) []string {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var bullets []string
	in := false
	for _, line := range strings.Split(string(text), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "### ") || strings.HasPrefix(trim, "## ") {
			if in {
				break
			}
			in = strings.EqualFold(trim, "### What was done") || strings.EqualFold(trim, "## What changed")
			continue
		}
		if !in || !strings.HasPrefix(trim, "- ") {
			continue
		}
		item := plainMarkdown(strings.TrimSpace(strings.TrimPrefix(trim, "- ")))
		if item == "" {
			continue
		}
		bullets = append(bullets, item)
		if len(bullets) == 3 {
			break
		}
	}
	return bullets
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return plainMarkdown(s)
}

func plainMarkdown(s string) string {
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "**", "")
	return strings.Join(strings.Fields(s), " ")
}

func clipMessage(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max])
}
