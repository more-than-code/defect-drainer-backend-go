package store

import (
	"encoding/json"
	"regexp"
	"strings"
)

// TestAccount is an operator login hint. A password is never a field.
type TestAccount struct {
	Label       string `json:"label"`
	Username    string `json:"username"`
	Environment string `json:"environment"`
	SecretRef   string `json:"secret_ref"`
}

const testAccountPasswordMsg = "test_accounts only accept label, username, environment, secret_ref; passwords are never stored"

var envSecretRefRe = regexp.MustCompile(`^env:[A-Z_][A-Z0-9_]*$`)

// ParseTestAccounts decodes a JSON array and rejects any key other than the four allowed.
func ParseTestAccounts(raw any) ([]TestAccount, error) {
	arr, ok := raw.([]any)
	if !ok {
		return nil, badRequest("test_accounts must be an array")
	}
	out := make([]TestAccount, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, badRequest("test_accounts must be objects")
		}
		for k := range m {
			switch k {
			case "label", "username", "environment", "secret_ref":
			default:
				return nil, badRequest(testAccountPasswordMsg)
			}
		}
		label, err := accountString(m, "label")
		if err != nil {
			return nil, err
		}
		username, err := accountString(m, "username")
		if err != nil {
			return nil, err
		}
		environment, err := accountString(m, "environment")
		if err != nil {
			return nil, err
		}
		secretRef, err := accountString(m, "secret_ref")
		if err != nil {
			return nil, err
		}
		out = append(out, TestAccount{
			Label: label, Username: username, Environment: environment, SecretRef: secretRef,
		})
	}
	return NormalizeTestAccounts(out)
}

func accountString(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", badRequest("test account " + key + " must be a string")
	}
	return s, nil
}

// ReservedSecretEnv is true for env:NAME values that would clobber the
// process environment or a Defect Drainer control variable.
func ReservedSecretEnv(name string) bool {
	switch name {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "PWD", "LANG", "TERM", "CI":
		return true
	}
	for _, p := range []string{"LC_", "GROK_", "SKILL_FORGE_", "DEFECT_DRAINER_", "DEFECT_CHANNEL_", "DD_", "GIT_", "GH_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ReservedSecretMessage is the 400 and the spawn refusal for a reserved name.
func ReservedSecretMessage(name string) string {
	return "secret_ref env:" + name + " is reserved; use a dedicated variable such as QA_PASSWORD"
}

// NormalizeTestAccounts trims fields and enforces label and secret_ref rules.
func NormalizeTestAccounts(in []TestAccount) ([]TestAccount, error) {
	return normalizeTestAccounts(in, true)
}

func normalizeTestAccounts(in []TestAccount, rejectReserved bool) ([]TestAccount, error) {
	if len(in) == 0 {
		return []TestAccount{}, nil
	}
	seen := map[string]bool{}
	out := make([]TestAccount, 0, len(in))
	for _, a := range in {
		label := strings.TrimSpace(a.Label)
		if label == "" {
			return nil, badRequest("test account label is required")
		}
		fold := strings.ToLower(label)
		if seen[fold] {
			return nil, badRequest("duplicate test account label")
		}
		seen[fold] = true
		ref := strings.TrimSpace(a.SecretRef)
		if len(ref) > 200 {
			return nil, badRequest("secret_ref too long")
		}
		if strings.HasPrefix(ref, "env:") && !envSecretRefRe.MatchString(ref) {
			return nil, badRequest("invalid secret_ref")
		}
		if rejectReserved && envSecretRefRe.MatchString(ref) {
			name := strings.TrimPrefix(ref, "env:")
			if ReservedSecretEnv(name) {
				return nil, badRequest(ReservedSecretMessage(name))
			}
		}
		out = append(out, TestAccount{
			Label:       label,
			Username:    strings.TrimSpace(a.Username),
			Environment: strings.TrimSpace(a.Environment),
			SecretRef:   ref,
		})
	}
	return out, nil
}

// EnvSecretName reports the host variable when ref is env:NAME.
// Free text and an empty ref return "", false.
func EnvSecretName(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if !envSecretRefRe.MatchString(ref) {
		return "", false
	}
	return strings.TrimPrefix(ref, "env:"), true
}

// AccountEnvVar is the host variable named by env.account.secret_ref when
// that reference is env:NAME. Free text and a missing account inject nothing.
func AccountEnvVar(env map[string]any) (string, bool) {
	if env == nil {
		return "", false
	}
	acc, ok := env["account"].(map[string]any)
	if !ok || acc == nil {
		return "", false
	}
	ref, _ := acc["secret_ref"].(string)
	ref = strings.TrimSpace(ref)
	if !envSecretRefRe.MatchString(ref) {
		return "", false
	}
	return strings.TrimPrefix(ref, "env:"), true
}

func parseStoredTestAccounts(raw string) []TestAccount {
	if strings.TrimSpace(raw) == "" {
		return []TestAccount{}
	}
	var v []TestAccount
	if err := json.Unmarshal([]byte(raw), &v); err != nil || v == nil {
		return []TestAccount{}
	}
	// Load does not apply the reserved-name refusal. Rows saved before that
	// check must still be visible so a spawn can refuse them.
	got, err := normalizeTestAccounts(v, false)
	if err != nil {
		return []TestAccount{}
	}
	return got
}

func storedTestAccountsJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "[]"
	}
	return raw
}
