package git

import (
	"fmt"
	"os"
	"path/filepath"
)

// GrokHome is the directory grok keeps its user-level state in: $GROK_HOME,
// else ~/.grok (verified on 1.0.40 — a GROK_HOME redirects the user-level
// sandbox.toml and the sessions store alike). The agent child inherits this
// process's environment, so this is the agent's grok home too. An
// unresolvable or relative home is an error, never a path under the cwd.
func GrokHome() (string, error) {
	if h := os.Getenv("GROK_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return "", fmt.Errorf("GROK_HOME %q is not an absolute path", h)
		}
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("home directory %q is not an absolute path", home)
	}
	return filepath.Join(home, ".grok"), nil
}
