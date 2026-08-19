package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestBindingAliasesEntryNameMatchesURLLeaf(t *testing.T) {
	b := git.WorktreeBinding{
		Repo:        "ttd-webapp",
		RepoURL:     "https://github.com/ex/ttd-webapp.git",
		WorktreeAbs: "/tmp/worktrees/ttd-webapp",
	}
	entries := []store.AppRepoEntry{
		{Name: "webapp", URL: "https://github.com/ex/ttd-webapp.git"},
	}
	got := BindingAliases(b, entries)
	if _, ok := got["webapp"]; !ok {
		t.Fatalf("entry name webapp should alias binding ttd-webapp: %#v", got)
	}
	if _, ok := got["ttd-webapp"]; !ok {
		t.Fatalf("binding repo missing from aliases: %#v", got)
	}
	if _, ok := got["web"]; ok {
		t.Fatalf("substring-only name must not match: %#v", got)
	}
	if _, ok := got["app"]; ok {
		t.Fatalf("substring-only name must not match: %#v", got)
	}
}

func TestBindingAliasesNeverSubstring(t *testing.T) {
	b := git.WorktreeBinding{Repo: "ttd-webapp", WorktreeAbs: "/tmp/ttd-webapp"}
	got := BindingAliases(b, nil)
	if _, ok := got["webapp"]; ok {
		t.Fatalf("webapp must not match ttd-webapp without repo_entries: %#v", got)
	}
}

func TestUnrunnableIsBlockedEvenWhenBaselineAlsoSkipped(t *testing.T) {
	key := VerifyResult{Repo: "no-such-repo", Command: "true", SkippedReason: "no worktree", Ok: false}
	baseline := &VerificationRun{
		Ran: true,
		Results: []VerifyResult{
			{Repo: "no-such-repo", Command: "true", SkippedReason: "no worktree", Ok: false},
		},
	}
	if v := classifyResult(key, baseline); v != VerdictBlocked {
		t.Fatalf("classify=%s want blocked", v)
	}
	j := judgeVerification(&VerificationRun{Ran: true, Results: []VerifyResult{key}}, baseline)
	if j.Ok {
		t.Fatal("unrunnable must not be excused by a matching skipped baseline")
	}
	if j.Blocked != 1 {
		t.Fatalf("blocked=%d", j.Blocked)
	}
}

func TestJudgePreExistingVsRegression(t *testing.T) {
	cmd := VerifyResult{Repo: "demo", Command: "false", Ok: false, ExitCode: intPtr(1)}
	green := VerifyResult{Repo: "demo", Command: "false", Ok: true, ExitCode: intPtr(0)}

	pre := judgeVerification(
		&VerificationRun{Ran: true, Results: []VerifyResult{cmd}},
		&VerificationRun{Ran: true, Results: []VerifyResult{cmd}},
	)
	if pre.Ok != true || pre.PreExisting != 1 || pre.Regressions != 0 {
		t.Fatalf("pre-existing: %+v", pre)
	}

	reg := judgeVerification(
		&VerificationRun{Ran: true, Results: []VerifyResult{cmd}},
		&VerificationRun{Ran: true, Results: []VerifyResult{green}},
	)
	if reg.Ok || reg.Regressions != 1 {
		t.Fatalf("regression: %+v", reg)
	}
}

func TestRunVerificationSkipsUnknownRepo(t *testing.T) {
	dir := t.TempDir()
	wts := []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: dir}}
	run := runVerification(
		context.Background(),
		[]store.VerifyCommand{{Repo: "no-such-repo", Command: "true"}},
		wts,
		dir,
		nil,
		nil,
		snapshotWorktreeReals(wts),
		func(string, string) {},
	)
	if !run.Ran || len(run.Results) != 1 {
		t.Fatalf("%+v", run)
	}
	if run.Results[0].Ok || run.Results[0].SkippedReason == "" {
		t.Fatalf("want skipped unrunnable: %+v", run.Results[0])
	}
	raw, err := os.ReadFile(filepath.Join(dir, "verify.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"skipped_reason"`) {
		t.Fatalf("verify.json: %s", raw)
	}
}

func TestRunVerificationAliasExecutesInWorktree(t *testing.T) {
	wt := t.TempDir()
	mark := filepath.Join(wt, "ran")
	wts := []git.WorktreeBinding{{
		Repo:        "ttd-webapp",
		RepoURL:     "https://github.com/ex/ttd-webapp.git",
		WorktreeAbs: wt,
	}}
	run := runVerification(
		context.Background(),
		[]store.VerifyCommand{{Repo: "webapp", Command: "echo ok > ran"}},
		wts,
		t.TempDir(),
		[]store.AppRepoEntry{{Name: "webapp", URL: "https://github.com/ex/ttd-webapp.git"}},
		nil,
		snapshotWorktreeReals(wts),
		func(string, string) {},
	)
	if !run.Ok || len(run.Results) != 1 || !run.Results[0].Ok {
		t.Fatalf("alias should run: %+v", run)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("command did not run in worktree: %v", err)
	}
}

func TestRunVerificationRefusesSymlinkedWorktree(t *testing.T) {
	realWT := t.TempDir()
	wts := []git.WorktreeBinding{{Repo: "demo", WorktreeAbs: realWT}}
	reals := snapshotWorktreeReals(wts)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realWT, link); err != nil {
		t.Fatal(err)
	}
	run := runVerification(
		context.Background(),
		[]store.VerifyCommand{{Repo: "demo", Command: "true"}},
		[]git.WorktreeBinding{{Repo: "demo", WorktreeAbs: link}},
		t.TempDir(),
		nil,
		nil,
		reals,
		func(string, string) {},
	)
	if run.Ok || len(run.Results) != 1 || run.Results[0].SkippedReason == "" {
		t.Fatalf("symlink cwd must skip: %+v", run)
	}
}
