package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	safeIDPat     = `^DEF-[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`
	safeAppIDPat  = `^app_[a-f0-9]{16}$`
	safeBatchPat  = `^BATCH-[A-Za-z0-9][A-Za-z0-9._-]{0,120}$`
	safeJobIDPat  = `(?i)^bjob_[a-z0-9_]+$`
	gitRefNamePat = `^[A-Za-z0-9._/-]{1,120}$`
)

var (
	safeIDRe     = regexp.MustCompile(safeIDPat)
	safeAppIDRe  = regexp.MustCompile(safeAppIDPat)
	safeBatchRe  = regexp.MustCompile(safeBatchPat)
	safeJobIDRe  = regexp.MustCompile(safeJobIDPat)
	gitRefNameRe = regexp.MustCompile(gitRefNamePat)
)

// IsSafeID is the defect/evidence id gate.
func IsSafeID(id string) bool { return safeIDRe.MatchString(id) }

// IsSafeAppID is app_[16 hex].
func IsSafeAppID(id string) bool { return safeAppIDRe.MatchString(id) }

// IsSafeBatchID gates batch ids.
func IsSafeBatchID(id string) bool { return safeBatchRe.MatchString(id) }

// IsSafeJobID gates batch-job delete ids only (not normalize job ids).
func IsSafeJobID(id string) bool { return safeJobIDRe.MatchString(id) }

// AppIDFromSeed is "app_" + first 16 hex of SHA-256("defect-drainer:app:" + seed).
func AppIDFromSeed(seed string) string {
	sum := sha256.Sum256([]byte("defect-drainer:app:" + seed))
	return "app_" + hex.EncodeToString(sum[:])[:16]
}

// SeededTutoredWebappAppID is the default seeded app.
var SeededTutoredWebappAppID = AppIDFromSeed("tutored-webapp")

// SeededTutoredMobileAppID is the mobile seed.
var SeededTutoredMobileAppID = AppIDFromSeed("tutored-mobileapp")

var legacyUmbrellaHash = AppIDFromSeed("tutored")

func legacyAppIDs() map[string]string {
	return map[string]string{
		"tutored":           SeededTutoredWebappAppID,
		"tutored-webapp":    SeededTutoredWebappAppID,
		"tutored-web":       SeededTutoredWebappAppID,
		"tutored-mobileapp": SeededTutoredMobileAppID,
		"tutored-mobile":    SeededTutoredMobileAppID,
		legacyUmbrellaHash:  SeededTutoredWebappAppID,
	}
}

// CanonicalizeAppID maps legacy aliases; empty → seeded webapp.
func CanonicalizeAppID(id string) string {
	t := strings.TrimSpace(id)
	if t == "" {
		return SeededTutoredWebappAppID
	}
	if mapped, ok := legacyAppIDs()[t]; ok {
		return mapped
	}
	return t
}

// GenerateAppID is app_ + 16 hex from 8 random bytes.
func GenerateAppID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "app_" + hex.EncodeToString(b[:])
}

// ParseGrokSandbox maps restrict/restricted/strict → strict.
func ParseGrokSandbox(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "workspace" {
		return "workspace"
	}
	return "strict"
}

// ParseAgentToolchain mirrors the TS parseAgentToolchain: unknown means none.
func ParseAgentToolchain(v string) string {
	if strings.ToLower(strings.TrimSpace(v)) == "flutter" {
		return "flutter"
	}
	return "none"
}

// ParseGitRefName allowlists ref names; hostile values fall back.
func ParseGitRefName(v, fallback string) string {
	s := strings.TrimSpace(v)
	if s == "" {
		return fallback
	}
	if !gitRefNameRe.MatchString(s) {
		return fallback
	}
	if strings.HasPrefix(s, "-") || strings.Contains(s, "..") || strings.HasSuffix(s, "/") {
		return fallback
	}
	return s
}

// ParseBaseSource maps github → origin.
func ParseBaseSource(v, fallback string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "local" {
		return "local"
	}
	if s == "origin" || s == "github" {
		return "origin"
	}
	if fallback != "" {
		return fallback
	}
	return "origin"
}

// Slugify matches store.slugify.
func Slugify(input string, max int) string {
	if max <= 0 {
		max = 40
	}
	s := strings.ToLower(input)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > max {
		out = strings.TrimRight(out[:max], "-")
	}
	if out == "" {
		return "defect"
	}
	return out
}

// MakeDefectID uses the host local calendar (TS getFullYear/getMonth/getDate).
func MakeDefectID(comment string, now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	y, m, d := now.Date()
	slug := Slugify(comment, 40)
	suffix := randomBase36(4)
	return fmt.Sprintf("DEF-%04d%02d%02d-%s-%s", y, int(m), d, slug, suffix)
}

func randomBase36(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var b [8]byte
	_, _ = rand.Read(b[:])
	out := make([]byte, 0, n)
	for i := 0; len(out) < n && i < len(b); i++ {
		out = append(out, alphabet[int(b[i])%36])
	}
	return string(out)
}
