package db

import (
	"strconv"
	"strings"
)

// RebindDollar rewrites SQLite-style ? placeholders to Postgres $1..$n.
// ? inside single-quoted strings, double-quoted identifiers, -- line comments,
// and /* */ block comments is left alone. ” and "" are escaped quotes.
func RebindDollar(q string) string {
	if strings.IndexByte(q, '?') < 0 {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := 0; i < len(q); {
		switch q[i] {
		case '\'', '"':
			i = copyQuoted(q, i, &b)
		case '-':
			if i+1 < len(q) && q[i+1] == '-' {
				for i < len(q) && q[i] != '\n' {
					b.WriteByte(q[i])
					i++
				}
				continue
			}
			b.WriteByte('-')
			i++
		case '/':
			if i+1 < len(q) && q[i+1] == '*' {
				b.WriteString("/*")
				i += 2
				for i < len(q) {
					if q[i] == '*' && i+1 < len(q) && q[i+1] == '/' {
						b.WriteString("*/")
						i += 2
						break
					}
					b.WriteByte(q[i])
					i++
				}
				continue
			}
			b.WriteByte('/')
			i++
		case '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(q[i])
			i++
		}
	}
	return b.String()
}

// copyQuoted copies a quote starting at q[i], including doubled escapes.
func copyQuoted(q string, i int, b *strings.Builder) int {
	quote := q[i]
	b.WriteByte(quote)
	i++
	for i < len(q) {
		b.WriteByte(q[i])
		if q[i] == quote {
			if i+1 < len(q) && q[i+1] == quote {
				b.WriteByte(q[i+1])
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}
