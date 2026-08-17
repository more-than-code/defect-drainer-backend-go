package main

import (
	"bytes"
	"io"
	"os"
	"testing"
)

func TestRunVersion(t *testing.T) {
	got := capture(t, func() int { return run([]string{"version"}) })
	if got.code != 0 {
		t.Fatalf("exit %d stderr %q", got.code, got.err)
	}
	if got.out != version+"\n" {
		t.Fatalf("stdout %q", got.out)
	}
}

func TestRunWorkerStillStub(t *testing.T) {
	got := capture(t, func() int { return run([]string{"worker"}) })
	if got.code != 1 {
		t.Fatalf("exit %d", got.code)
	}
	if !bytes.Contains([]byte(got.err), []byte("not implemented")) {
		t.Fatalf("stderr %q", got.err)
	}
}

func TestRunUnknown(t *testing.T) {
	got := capture(t, func() int { return run([]string{"frobnicate"}) })
	if got.code != 2 {
		t.Fatalf("exit %d", got.code)
	}
}

func TestHealthcheckFailsWhenNothingListens(t *testing.T) {
	t.Setenv("DEFECT_DRAINER_HOST", "127.0.0.1")
	t.Setenv("DEFECT_DRAINER_PORT", "1")
	got := capture(t, func() int { return run([]string{"healthcheck"}) })
	if got.code == 0 {
		t.Fatal("healthcheck must fail when /health is unreachable")
	}
}

type captured struct {
	code int
	out  string
	err  string
}

func capture(t *testing.T, fn func() int) captured {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wOut, wErr
	code := fn()
	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(rOut)
	errb, _ := io.ReadAll(rErr)
	return captured{code: code, out: string(out), err: string(errb)}
}
