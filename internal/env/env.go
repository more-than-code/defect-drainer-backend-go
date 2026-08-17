// Package env reads DEFECT_DRAINER_* (and legacy DEFECT_CHANNEL_*) process env.
package env

import (
	"os"
	"strings"
)

// EnvFirst returns the first non-empty value among keys.
func EnvFirst(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// EnvDrainer reads DEFECT_DRAINER_<suffix>, then legacy DEFECT_CHANNEL_<suffix>.
func EnvDrainer(suffix string) string {
	return EnvFirst("DEFECT_DRAINER_"+suffix, "DEFECT_CHANNEL_"+suffix)
}

// EnvDrainerFlag is true iff EnvDrainer(suffix) is exactly "1".
func EnvDrainerFlag(suffix string) bool {
	return EnvDrainer(suffix) == "1"
}

// LoadEnvFile applies KEY=VALUE lines from path. Does not override keys already
// set in the process environment. Missing file is a no-op (returns "").
func LoadEnvFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		eq := strings.IndexByte(trimmed, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		value := strings.TrimSpace(trimmed[eq+1:])
		if n := len(value); n >= 2 {
			if (value[0] == '"' && value[n-1] == '"') || (value[0] == '\'' && value[n-1] == '\'') {
				value = value[1 : n-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return "", err
			}
		}
	}
	return path, nil
}
