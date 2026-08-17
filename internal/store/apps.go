package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/joe/defect-drainer-go/internal/db"
	"github.com/joe/defect-drainer-go/internal/env"
)

// AppRepoEntry is a Settings repo row.
type AppRepoEntry struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	BaseSource string `json:"base_source,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
}

// VerifyCommand is an operator-defined check DD re-runs in a worktree after a
// fix job. Operator-authored on purpose: the runner is not sandboxed, so
// executing strings written by the coding agent would be unsandboxed execution
// on the host.
type VerifyCommand struct {
	Repo    string `json:"repo"`
	Command string `json:"command"`
}

// NormalizeVerifyCommands drops blanks and caps size, matching the TS store.
func NormalizeVerifyCommands(in []VerifyCommand) []VerifyCommand {
	out := []VerifyCommand{}
	for _, v := range in {
		repo := strings.TrimSpace(v.Repo)
		cmd := strings.TrimSpace(v.Command)
		if repo == "" || cmd == "" || len(cmd) > 500 {
			continue
		}
		out = append(out, VerifyCommand{Repo: repo, Command: cmd})
		if len(out) == 20 {
			break
		}
	}
	return out
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func parseStoredVerifyCommands(raw string) []VerifyCommand {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var v []VerifyCommand
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil
	}
	got := NormalizeVerifyCommands(v)
	if len(got) == 0 {
		return nil
	}
	return got
}

// AppRecord is the console App JSON object.
type AppRecord struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Description   string         `json:"description,omitempty"`
	WorkspaceRoot string         `json:"workspace_root,omitempty"`
	Repos         []string       `json:"repos,omitempty"`
	RepoEntries   []AppRepoEntry `json:"repo_entries,omitempty"`
	RepoURL       string         `json:"repo_url,omitempty"`
	RepoURLs      []string       `json:"repo_urls,omitempty"`
	GrokSandbox   string         `json:"grok_sandbox,omitempty"`
	// AgentToolchain pre-provisions a toolchain into the job handoff: none | flutter.
	AgentToolchain string `json:"agent_toolchain,omitempty"`
	// AllowSimulatorWrites grants the job Simulator device-tree writes.
	AllowSimulatorWrites bool `json:"allow_simulator_writes,omitempty"`
	// VerifyCommands are re-run by DD after a fix job; all must pass to resolve.
	VerifyCommands []VerifyCommand `json:"verify_commands,omitempty"`
	BaseRemote     string          `json:"base_remote,omitempty"`
	BaseBranch     string          `json:"base_branch,omitempty"`
	Default        bool            `json:"default"`
}

const appSelect = `id, name, description, workspace_root, repos_json, repo_url, repo_urls_json, grok_sandbox, agent_toolchain, allow_simulator_writes, verify_commands_json, base_remote, base_branch, is_default`

func nameFromRepoURL(url string) string {
	u := strings.TrimRight(url, "/")
	parts := strings.Split(u, "/")
	leaf := "repo"
	if len(parts) > 0 && parts[len(parts)-1] != "" {
		leaf = parts[len(parts)-1]
	}
	leaf = strings.TrimSuffix(leaf, ".git")
	if strings.HasSuffix(strings.ToLower(leaf), ".git") {
		leaf = leaf[:len(leaf)-4]
	}
	if leaf == "" {
		return "repo"
	}
	return leaf
}

func inferBaseSource(url, explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return ParseBaseSource(explicit, "origin")
	}
	u := strings.TrimSpace(url)
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, "file://") {
		return "local"
	}
	return "origin"
}

func parseStoredRepoEntries(repoURLsJSON, repoURL, reposJSON string) []AppRepoEntry {
	names := db.ParseJSONArray(reposJSON)
	var raw any
	if repoURLsJSON == "" {
		raw = []any{}
	} else if err := json.Unmarshal([]byte(repoURLsJSON), &raw); err != nil {
		raw = []any{}
	}
	arr, ok := raw.([]any)
	if !ok {
		arr = nil
	}
	if len(arr) > 0 {
		first := arr[0]
		if _, isObj := first.(map[string]any); isObj {
			var entries []AppRepoEntry
			for _, item := range arr {
				o, ok := item.(map[string]any)
				if !ok {
					continue
				}
				name := fmt.Sprint(firstNonNil(o["name"], o["repo"], ""))
				url := fmt.Sprint(firstNonNil(o["url"], o["repo_url"], ""))
				if name == "<nil>" {
					name = ""
				}
				if url == "<nil>" {
					url = ""
				}
				src, _ := o["base_source"].(string)
				br, _ := o["base_branch"].(string)
				entries = append(entries, normalizeOne(name, url, src, br))
			}
			return uniqByName(entries)
		}
		var urls []string
		for _, item := range arr {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s != "" {
				urls = append(urls, s)
			}
		}
		if strings.TrimSpace(repoURL) != "" {
			urls = append([]string{strings.TrimSpace(repoURL)}, urls...)
		}
		seen := map[string]bool{}
		var uniq []string
		for _, u := range urls {
			if seen[u] {
				continue
			}
			seen[u] = true
			uniq = append(uniq, u)
		}
		out := make([]AppRepoEntry, 0, len(uniq))
		for i, u := range uniq {
			name := nameFromRepoURL(u)
			if i < len(names) && names[i] != "" {
				name = names[i]
			}
			out = append(out, AppRepoEntry{Name: name, URL: u})
		}
		return out
	}
	if strings.TrimSpace(repoURL) != "" {
		name := nameFromRepoURL(repoURL)
		if len(names) > 0 && names[0] != "" {
			name = names[0]
		}
		out := []AppRepoEntry{{Name: name, URL: strings.TrimSpace(repoURL)}}
		for _, n := range names[1:] {
			out = append(out, AppRepoEntry{Name: n, URL: ""})
		}
		return out
	}
	out := make([]AppRepoEntry, 0, len(names))
	for _, n := range names {
		out = append(out, AppRepoEntry{Name: n, URL: ""})
	}
	return out
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return ""
}

func normalizeOne(name, url, src, branch string) AppRepoEntry {
	url = strings.TrimSpace(url)
	name = strings.TrimSpace(name)
	if name == "" && url != "" {
		name = nameFromRepoURL(url)
	}
	e := AppRepoEntry{
		Name:       name,
		URL:        url,
		BaseSource: inferBaseSource(url, src),
	}
	if b := ParseGitRefName(branch, ""); b != "" {
		e.BaseBranch = b
	}
	return e
}

func uniqByName(in []AppRepoEntry) []AppRepoEntry {
	by := map[string]AppRepoEntry{}
	var order []string
	for _, e := range in {
		if e.Name == "" && e.URL == "" {
			continue
		}
		if _, ok := by[e.Name]; !ok {
			order = append(order, e.Name)
		}
		by[e.Name] = e
	}
	out := make([]AppRepoEntry, 0, len(order))
	for _, n := range order {
		out = append(out, by[n])
	}
	return out
}

func persistRepoColumns(entries []AppRepoEntry) (reposJSON string, repoURL sql.NullString, repoURLsJSON string) {
	clean := uniqByName(entries)
	names := make([]string, 0, len(clean))
	var withURL []AppRepoEntry
	type stored struct {
		Name       string `json:"name"`
		URL        string `json:"url"`
		BaseSource string `json:"base_source"`
		BaseBranch string `json:"base_branch,omitempty"`
	}
	st := make([]stored, 0, len(clean))
	for _, e := range clean {
		names = append(names, e.Name)
		if e.URL != "" {
			withURL = append(withURL, e)
		}
		s := stored{
			Name:       e.Name,
			URL:        e.URL,
			BaseSource: e.BaseSource,
		}
		if s.BaseSource == "" {
			s.BaseSource = inferBaseSource(e.URL, "")
		}
		if e.BaseBranch != "" {
			s.BaseBranch = e.BaseBranch
		}
		st = append(st, s)
	}
	reposJSON = db.JSONArray(names)
	if len(withURL) == 1 {
		repoURL = sql.NullString{String: withURL[0].URL, Valid: true}
	}
	b, err := json.Marshal(st)
	if err != nil {
		repoURLsJSON = "[]"
	} else {
		repoURLsJSON = string(b)
	}
	return
}

func rowToApp(id, name string, description, workspace, reposJSON, repoURL, repoURLsJSON, sandbox, toolchain, verifyJSON, remote, branch sql.NullString, simWrites, isDefault int) AppRecord {
	entries := parseStoredRepoEntries(repoURLsJSON.String, repoURL.String, reposJSON.String)
	var urls []string
	var names []string
	for _, e := range entries {
		if e.Name != "" {
			names = append(names, e.Name)
		}
		if e.URL != "" {
			urls = append(urls, e.URL)
		}
	}
	if len(names) == 0 {
		names = db.ParseJSONArray(reposJSON.String)
	}
	rec := AppRecord{
		ID:                   id,
		Name:                 name,
		Repos:                names,
		RepoEntries:          entries,
		GrokSandbox:          ParseGrokSandbox(sandbox.String),
		AgentToolchain:       ParseAgentToolchain(toolchain.String),
		AllowSimulatorWrites: simWrites != 0,
		VerifyCommands:       parseStoredVerifyCommands(verifyJSON.String),
		BaseRemote:           ParseGitRefName(remote.String, "origin"),
		BaseBranch:           ParseGitRefName(branch.String, "main"),
		Default:              isDefault != 0,
	}
	if description.Valid && description.String != "" {
		rec.Description = description.String
	}
	if workspace.Valid && workspace.String != "" {
		rec.WorkspaceRoot = workspace.String
	}
	if len(urls) == 1 {
		rec.RepoURL = urls[0]
	} else if len(urls) > 1 {
		rec.RepoURLs = urls
	}
	return rec
}

func scanApp(scanner interface {
	Scan(dest ...any) error
}) (AppRecord, error) {
	var id, name string
	var desc, ws, reposJSON, repoURL, urlsJSON, sandbox, toolchain, verifyJSON, remote, branch sql.NullString
	var simWrites, isDefault int
	if err := scanner.Scan(&id, &name, &desc, &ws, &reposJSON, &repoURL, &urlsJSON, &sandbox, &toolchain, &simWrites, &verifyJSON, &remote, &branch, &isDefault); err != nil {
		return AppRecord{}, err
	}
	return rowToApp(id, name, desc, ws, reposJSON, repoURL, urlsJSON, sandbox, toolchain, verifyJSON, remote, branch, simWrites, isDefault), nil
}

// ListApps ORDER BY is_default DESC, name ASC.
func ListApps(sqlDB *sql.DB) ([]AppRecord, error) {
	rows, err := sqlDB.Query(`SELECT ` + appSelect + ` FROM apps ORDER BY is_default DESC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppRecord
	for rows.Next() {
		rec, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if out == nil {
		out = []AppRecord{}
	}
	return out, rows.Err()
}

// GetApp canonicalizes id and returns the row or nil.
func GetApp(sqlDB *sql.DB, appID string) (*AppRecord, error) {
	id := CanonicalizeAppID(appID)
	row := sqlDB.QueryRow(`SELECT `+appSelect+` FROM apps WHERE id = ?`, id)
	rec, err := scanApp(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetDefaultAppID prefers is_default=1.
func GetDefaultAppID(sqlDB *sql.DB) string {
	var id string
	err := sqlDB.QueryRow(`SELECT id FROM apps WHERE is_default = 1 LIMIT 1`).Scan(&id)
	if err == nil && id != "" {
		return id
	}
	err = sqlDB.QueryRow(`SELECT id FROM apps ORDER BY name LIMIT 1`).Scan(&id)
	if err == nil && id != "" {
		return id
	}
	return SeededTutoredWebappAppID
}

// EnsureSeededApps inserts the two Tutored seeds when apps is empty.
func EnsureSeededApps(sqlDB *sql.DB) error {
	var c int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM apps`).Scan(&c); err != nil {
		return err
	}
	if c > 0 {
		return nil
	}
	ts := db.NowIso()
	seeds := []struct {
		id, name, desc, ws string
		repos              []string
		def                bool
	}{
		{
			id:    SeededTutoredWebappAppID,
			name:  "Tutored Webapp",
			desc:  "Tutored web product — repos: ttd-webapp, ttd-backend (Grok picks fix surface)",
			ws:    "/Users/joe/workspace/tutored",
			repos: []string{"ttd-webapp", "ttd-backend"},
			def:   true,
		},
		{
			id:    SeededTutoredMobileAppID,
			name:  "Tutored Mobileapp",
			desc:  "Tutored mobile product — repos: ttd-mobileapp, ttd-backend (Grok picks fix surface)",
			ws:    "/Users/joe/workspace/tutored",
			repos: []string{"ttd-mobileapp", "ttd-backend"},
			def:   false,
		},
	}
	ins, err := sqlDB.Prepare(`
    INSERT INTO apps (
      id, name, description, workspace_root, repos_json, repo_url, repo_urls_json,
      grok_sandbox, base_remote, base_branch, is_default, created_at, updated_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  `)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, a := range seeds {
		def := 0
		if a.def {
			def = 1
		}
		if _, err := ins.Exec(
			a.id, a.name, a.desc, a.ws,
			db.JSONArray(a.repos),
			nil,
			db.JSONArray(nil),
			ParseGrokSandbox(""),
			ParseGitRefName("", "origin"),
			ParseGitRefName("", "main"),
			def, ts, ts,
		); err != nil {
			return err
		}
	}
	return nil
}

// CreateApp inserts a new app (write path).
func CreateApp(sqlDB *sql.DB, input AppRecord) (AppRecord, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return AppRecord{}, fmt.Errorf("name is required")
	}
	if len(name) > 120 {
		return AppRecord{}, fmt.Errorf("name too long")
	}
	id := GenerateAppID()
	entries := input.RepoEntries
	if len(entries) == 0 {
		entries = entriesFromLegacy(input)
	}
	reposJSON, repoURL, urlsJSON := persistRepoColumns(entries)
	sandbox := ParseGrokSandbox(input.GrokSandbox)
	if input.GrokSandbox == "" {
		sandbox = ParseGrokSandbox(env.EnvDrainer("GROK_SANDBOX"))
	}
	var existingDefault int
	_ = sqlDB.QueryRow(`SELECT COUNT(*) FROM apps WHERE is_default = 1`).Scan(&existingDefault)
	makeDefault := input.Default || existingDefault == 0
	if makeDefault {
		_, _ = sqlDB.Exec(`UPDATE apps SET is_default = 0`)
	}
	ts := db.NowIso()
	def := 0
	if makeDefault {
		def = 1
	}
	var desc, ws any
	if strings.TrimSpace(input.Description) != "" {
		desc = strings.TrimSpace(input.Description)
	}
	if strings.TrimSpace(input.WorkspaceRoot) != "" {
		ws = strings.TrimSpace(input.WorkspaceRoot)
	}
	_, err := sqlDB.Exec(`
    INSERT INTO apps (
      id, name, description, workspace_root, repos_json, repo_url, repo_urls_json,
      grok_sandbox, agent_toolchain, allow_simulator_writes, verify_commands_json,
      base_remote, base_branch, is_default, created_at, updated_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, desc, ws, reposJSON, nullStr(repoURL), urlsJSON,
		sandbox, ParseAgentToolchain(input.AgentToolchain), boolToInt(input.AllowSimulatorWrites),
		mustJSON(NormalizeVerifyCommands(input.VerifyCommands)),
		ParseGitRefName(input.BaseRemote, "origin"), ParseGitRefName(input.BaseBranch, "main"),
		def, ts, ts,
	)
	if err != nil {
		return AppRecord{}, err
	}
	got, err := GetApp(sqlDB, id)
	if err != nil || got == nil {
		return AppRecord{}, fmt.Errorf("create app failed")
	}
	return *got, nil
}

func nullStr(n sql.NullString) any {
	if n.Valid {
		return n.String
	}
	return nil
}

func entriesFromLegacy(input AppRecord) []AppRepoEntry {
	if len(input.RepoURLs) > 0 || input.RepoURL != "" {
		urls := append([]string{}, input.RepoURLs...)
		if input.RepoURL != "" {
			urls = append(urls, input.RepoURL)
		}
		var out []AppRepoEntry
		for i, u := range urls {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			name := nameFromRepoURL(u)
			if i < len(input.Repos) && input.Repos[i] != "" {
				name = input.Repos[i]
			}
			out = append(out, AppRepoEntry{Name: name, URL: u, BaseSource: inferBaseSource(u, "")})
		}
		return out
	}
	var out []AppRepoEntry
	for _, n := range input.Repos {
		n = strings.TrimSpace(n)
		if n != "" {
			out = append(out, AppRepoEntry{Name: n, URL: ""})
		}
	}
	return out
}

// UpdateAppSettings patches an app.
func UpdateAppSettings(sqlDB *sql.DB, appID string, patch AppRecord, has map[string]bool) (AppRecord, error) {
	id := CanonicalizeAppID(appID)
	cur, err := GetApp(sqlDB, id)
	if err != nil {
		return AppRecord{}, err
	}
	if cur == nil {
		return AppRecord{}, fmt.Errorf("unknown app_id: %s", appID)
	}
	next := *cur
	if has["name"] && strings.TrimSpace(patch.Name) != "" {
		next.Name = strings.TrimSpace(patch.Name)
	}
	if has["description"] {
		next.Description = strings.TrimSpace(patch.Description)
	}
	if has["workspace_root"] {
		next.WorkspaceRoot = strings.TrimSpace(patch.WorkspaceRoot)
	}
	if has["grok_sandbox"] {
		next.GrokSandbox = ParseGrokSandbox(patch.GrokSandbox)
	}
	if has["agent_toolchain"] {
		next.AgentToolchain = ParseAgentToolchain(patch.AgentToolchain)
	}
	if has["allow_simulator_writes"] {
		next.AllowSimulatorWrites = patch.AllowSimulatorWrites
	}
	if has["verify_commands"] {
		next.VerifyCommands = NormalizeVerifyCommands(patch.VerifyCommands)
	}
	if has["base_remote"] {
		next.BaseRemote = ParseGitRefName(patch.BaseRemote, "origin")
	}
	if has["base_branch"] {
		next.BaseBranch = ParseGitRefName(patch.BaseBranch, "main")
	}
	entries := next.RepoEntries
	if has["repo_entries"] {
		entries = patch.RepoEntries
		if entries == nil {
			entries = []AppRepoEntry{}
		}
	} else if has["repo_urls"] || has["repo_url"] {
		entries = entriesFromLegacy(patch)
		if has["repos"] && len(patch.Repos) > 0 {
			for i := range entries {
				if i < len(patch.Repos) {
					entries[i].Name = patch.Repos[i]
				}
			}
		}
	} else if has["repos"] {
		var nextE []AppRepoEntry
		for i, n := range patch.Repos {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			url := ""
			if i < len(entries) {
				url = entries[i].URL
			}
			nextE = append(nextE, AppRepoEntry{Name: n, URL: url})
		}
		entries = nextE
	}
	next.RepoEntries = entries
	if has["default"] && patch.Default {
		_, _ = sqlDB.Exec(`UPDATE apps SET is_default = 0`)
		next.Default = true
	} else if has["default"] && !patch.Default {
		next.Default = false
	}
	reposJSON, repoURL, urlsJSON := persistRepoColumns(entries)
	ts := db.NowIso()
	def := 0
	if next.Default {
		def = 1
	}
	var desc, ws any
	if next.Description != "" {
		desc = next.Description
	}
	if next.WorkspaceRoot != "" {
		ws = next.WorkspaceRoot
	}
	_, err = sqlDB.Exec(`UPDATE apps SET
      name = ?, description = ?, workspace_root = ?, repos_json = ?,
      repo_url = ?, repo_urls_json = ?, grok_sandbox = ?, agent_toolchain = ?,
      allow_simulator_writes = ?, verify_commands_json = ?, base_remote = ?,
      base_branch = ?, is_default = ?, updated_at = ?
     WHERE id = ?`,
		next.Name, desc, ws, reposJSON, nullStr(repoURL), urlsJSON,
		ParseGrokSandbox(next.GrokSandbox),
		ParseAgentToolchain(next.AgentToolchain),
		boolToInt(next.AllowSimulatorWrites),
		mustJSON(NormalizeVerifyCommands(next.VerifyCommands)),
		ParseGitRefName(next.BaseRemote, "origin"),
		ParseGitRefName(next.BaseBranch, "main"),
		def, ts, id,
	)
	if err != nil {
		return AppRecord{}, err
	}
	got, err := GetApp(sqlDB, id)
	if err != nil || got == nil {
		return AppRecord{}, fmt.Errorf("update app failed")
	}
	return *got, nil
}

// DeleteApp removes an app (FK may fail if children exist).
func DeleteApp(sqlDB *sql.DB, appID string) error {
	id := CanonicalizeAppID(appID)
	if !IsSafeAppID(id) {
		return fmt.Errorf("invalid app_id: %s", appID)
	}
	cur, err := GetApp(sqlDB, id)
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("unknown app_id: %s", appID)
	}
	if _, err := sqlDB.Exec(`DELETE FROM apps WHERE id = ?`, id); err != nil {
		return err
	}
	if cur.Default {
		var next string
		if err := sqlDB.QueryRow(`SELECT id FROM apps ORDER BY name LIMIT 1`).Scan(&next); err == nil && next != "" {
			_, _ = sqlDB.Exec(`UPDATE apps SET is_default = 1 WHERE id = ?`, next)
		}
	}
	return nil
}

// ResolveAppID canonicalizes and verifies.
func ResolveAppID(sqlDB *sql.DB, input string) (string, error) {
	base := input
	if strings.TrimSpace(base) == "" {
		base = GetDefaultAppID(sqlDB)
	}
	id := CanonicalizeAppID(base)
	if !IsSafeAppID(id) {
		return "", fmt.Errorf("invalid app_id: %s", input)
	}
	got, err := GetApp(sqlDB, id)
	if err != nil {
		return "", err
	}
	if got == nil {
		return "", fmt.Errorf("unknown app_id: %s", input)
	}
	return id, nil
}

// ClientHintForApp matches apps.clientHintForApp.
func ClientHintForApp(app *AppRecord) string {
	if app == nil {
		return "unknown"
	}
	var names []string
	for _, e := range app.RepoEntries {
		names = append(names, e.Name)
	}
	if len(names) == 0 {
		names = app.Repos
	}
	blob := strings.ToLower(app.Name + " " + strings.Join(names, " "))
	if strings.Contains(blob, "mobile") {
		return "mobile"
	}
	if strings.Contains(blob, "web") {
		return "web"
	}
	return "unknown"
}
