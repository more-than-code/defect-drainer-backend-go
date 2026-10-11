package db

import (
	"net/url"
	"os"
	"strings"
)

func isPostgresURL(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// redactDSN returns a DSN safe to put in an error. URL userinfo passwords use
// url.URL.Redacted. Query-string and keyword passwords are replaced too.
func redactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if isPostgresURL(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return "(redacted)"
		}
		red := u.Redacted()
		q := u.Query()
		for _, key := range []string{"password", "sslpassword"} {
			if v := q.Get(key); v != "" {
				red = strings.ReplaceAll(red, key+"="+v, key+"=xxxxx")
				if enc := url.QueryEscape(v); enc != v {
					red = strings.ReplaceAll(red, key+"="+enc, key+"=xxxxx")
				}
			}
		}
		return red
	}
	return redactKeywordSecrets(dsn)
}

func redactKeywordSecrets(dsn string) string {
	var b strings.Builder
	b.Grow(len(dsn))
	rest := dsn
	for rest != "" {
		i := 0
		for i < len(rest) && isDSNSpace(rest[i]) {
			i++
		}
		b.WriteString(rest[:i])
		rest = rest[i:]
		if rest == "" {
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			b.WriteString(rest)
			break
		}
		key := rest[:eq]
		b.WriteString(key)
		b.WriteByte('=')
		rest = strings.TrimLeft(rest[eq+1:], " \t")
		if rest == "" {
			break
		}
		secret := isSecretKey(key)
		if rest[0] == '\'' || rest[0] == '"' {
			raw, next := takeQuoted(rest, rest[0])
			if secret {
				b.WriteByte(rest[0])
				b.WriteString("xxxxx")
				b.WriteByte(rest[0])
			} else {
				b.WriteString(raw)
			}
			rest = next
			continue
		}
		j := 0
		for j < len(rest) && !isDSNSpace(rest[j]) {
			j++
		}
		if secret {
			b.WriteString("xxxxx")
		} else {
			b.WriteString(rest[:j])
		}
		rest = rest[j:]
	}
	return b.String()
}

func isSecretKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "password", "sslpassword":
		return true
	default:
		return false
	}
}

func isDSNSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func takeQuoted(s string, q byte) (raw string, next string) {
	i := 1
	for i < len(s) {
		if s[i] == q {
			if i+1 < len(s) && s[i+1] == q {
				i += 2
				continue
			}
			return s[:i+1], s[i+1:]
		}
		i++
	}
	return s, ""
}

func unquoteSQL(raw string) string {
	if len(raw) < 2 {
		return raw
	}
	q := raw[0]
	if raw[len(raw)-1] != q {
		return raw
	}
	return strings.ReplaceAll(raw[1:len(raw)-1], string([]byte{q, q}), string([]byte{q}))
}

func keywordValue(dsn, want string) string {
	rest := dsn
	for rest != "" {
		i := 0
		for i < len(rest) && isDSNSpace(rest[i]) {
			i++
		}
		rest = rest[i:]
		if rest == "" {
			return ""
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return ""
		}
		key := strings.TrimSpace(rest[:eq])
		rest = strings.TrimLeft(rest[eq+1:], " \t")
		if rest == "" {
			return ""
		}
		var val string
		if rest[0] == '\'' || rest[0] == '"' {
			raw, next := takeQuoted(rest, rest[0])
			val = unquoteSQL(raw)
			rest = next
		} else {
			k := 0
			for k < len(rest) && !isDSNSpace(rest[k]) {
				k++
			}
			val = rest[:k]
			rest = rest[k:]
		}
		if strings.EqualFold(key, want) {
			return val
		}
	}
	return ""
}

func dsnPassword(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if isPostgresURL(dsn) {
		u, err := url.Parse(dsn)
		if err != nil {
			return ""
		}
		if u.User != nil {
			if pw, ok := u.User.Password(); ok && pw != "" {
				return pw
			}
		}
		if pw := u.Query().Get("password"); pw != "" {
			return pw
		}
		return u.Query().Get("sslpassword")
	}
	if pw := keywordValue(dsn, "password"); pw != "" {
		return pw
	}
	return keywordValue(dsn, "sslpassword")
}

func replaceSecret(msg, pw string) string {
	if pw == "" {
		return msg
	}
	msg = strings.ReplaceAll(msg, pw, "xxxxx")
	if enc := url.QueryEscape(pw); enc != pw {
		msg = strings.ReplaceAll(msg, enc, "xxxxx")
	}
	if enc := url.PathEscape(pw); enc != pw {
		msg = strings.ReplaceAll(msg, enc, "xxxxx")
	}
	return msg
}

// scrubText removes dsn and any password it (or PGPASSWORD) carries.
// Open-path errors use this instead of wrapping the driver error.
func scrubText(msg, dsn string) string {
	msg = replaceSecret(msg, dsnPassword(dsn))
	msg = replaceSecret(msg, os.Getenv("PGPASSWORD"))
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, redactDSN(dsn))
	}
	return msg
}
