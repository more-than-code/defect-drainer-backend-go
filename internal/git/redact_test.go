package git

import (
	"bytes"
	"strings"
	"testing"
)

func TestRedactWriterSplitAtEveryOffset(t *testing.T) {
	secret := "S3cr3t-Value-42"
	for i := 0; i <= len(secret); i++ {
		var buf bytes.Buffer
		w := newRedactWriter(&buf, secret)
		if _, err := w.Write([]byte("pre-" + secret[:i])); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(secret[i:] + "-post")); err != nil {
			t.Fatal(err)
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if strings.Contains(got, secret) {
			t.Fatalf("offset %d: secret leaked", i)
		}
		if got != "pre-[redacted]-post" {
			t.Fatalf("offset %d: got %q", i, got)
		}
	}
}

func TestRedactWriterTwoCopiesAndTail(t *testing.T) {
	secret := "S3cr3t-Value-42"
	var buf bytes.Buffer
	w := newRedactWriter(&buf, secret)
	payload := "a" + secret + "b" + secret + "c"
	// Split one byte at a time across the whole payload.
	for i := 0; i < len(payload); i++ {
		if _, err := w.Write([]byte(payload[i : i+1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatal("secret leaked")
	}
	if buf.String() != "a[redacted]b[redacted]c" {
		t.Fatalf("got %q", buf.String())
	}
}
