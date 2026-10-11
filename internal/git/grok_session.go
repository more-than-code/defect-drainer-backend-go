package git

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// grokSessionID is the shape of a session directory name. Grok treats a
// UUID-shaped -r argument as an ID and anything else as a title search, and a
// leading "-" would read as a flag, so only this shape is ever passed on.
var grokSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// GrokSessionsDir is where Grok Build keeps the sessions for one cwd:
// <GrokHome>/sessions/<url-escaped absolute cwd>/<uuid>/ (verified on 1.0.40).
// GrokHome honours $GROK_HOME, which redirects the sessions store too.
func GrokSessionsDir(cwd string) (string, error) {
	home, err := GrokHome()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions", url.PathEscape(abs)), nil
}

// LatestGrokSession returns the newest session for cwd that has a chat
// history, which is what `grok -c` would continue. ok is false when there is
// none, including when Grok's layout has moved: Resume then fails closed.
func LatestGrokSession(cwd string) (id string, ok bool) {
	dir, err := GrokSessionsDir(cwd)
	if err != nil {
		return "", false
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var newest time.Time
	for _, e := range ents {
		if !e.IsDir() || !grokSessionID.MatchString(e.Name()) {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, e.Name(), "chat_history.jsonl"))
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if id == "" || st.ModTime().After(newest) {
			id, newest = e.Name(), st.ModTime()
		}
	}
	return id, id != ""
}
