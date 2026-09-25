package git

import (
	"bytes"
	"io"
)

const redactedMark = "[redacted]"

// redactWriter replaces every exact occurrence of secret with [redacted].
// It holds back len(secret)-1 bytes so a value split across Write calls is
// still one match. Flush emits the hold. An empty secret is a plain pass-through.
type redactWriter struct {
	w      io.Writer
	secret []byte
	buf    []byte
}

func newRedactWriter(w io.Writer, secret string) *redactWriter {
	return &redactWriter{w: w, secret: []byte(secret)}
}

func (r *redactWriter) Write(p []byte) (int, error) {
	if r == nil || r.w == nil {
		return len(p), nil
	}
	if len(r.secret) == 0 {
		_, err := r.w.Write(p)
		return len(p), err
	}
	r.buf = append(r.buf, p...)
	r.buf = bytes.ReplaceAll(r.buf, r.secret, []byte(redactedMark))
	keep := len(r.secret) - 1
	if keep < 1 || len(r.buf) <= keep {
		return len(p), nil
	}
	emit := r.buf[:len(r.buf)-keep]
	r.buf = append([]byte(nil), r.buf[len(r.buf)-keep:]...)
	_, err := r.w.Write(emit)
	return len(p), err
}

// Flush writes the held tail. Call it when the producer is finished.
func (r *redactWriter) Flush() error {
	if r == nil || r.w == nil || len(r.buf) == 0 {
		return nil
	}
	out := r.buf
	if len(r.secret) > 0 {
		out = bytes.ReplaceAll(r.buf, r.secret, []byte(redactedMark))
	}
	r.buf = nil
	_, err := r.w.Write(out)
	return err
}
