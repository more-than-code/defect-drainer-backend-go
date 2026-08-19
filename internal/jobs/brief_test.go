package jobs

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/joe/defect-drainer-go/internal/git"
	"github.com/joe/defect-drainer-go/internal/store"
)

func TestAcceptanceCriteria(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "missing section",
			body: "## Repro\n\n- do a thing\n",
			want: nil,
		},
		{
			name: "checkbox unchecked",
			body: "## Acceptance\n\n- [ ] Confirm title\n- [ ] Add a test\n",
			want: []string{"Confirm title", "Add a test"},
		},
		{
			name: "checkbox checked",
			body: "## Acceptance\n\n- [x] already done\n- [X] also done\n",
			want: []string{"already done", "also done"},
		},
		{
			name: "plain bullets",
			body: "## Acceptance\n\n- first item\n* second item\n+ third item\n",
			want: []string{"first item", "second item", "third item"},
		},
		{
			name: "numbered items",
			body: "## Acceptance\n\n1. first\n2. second\n3) third\n",
			want: []string{"first", "second", "third"},
		},
		{
			name: "next heading terminates",
			body: "## Acceptance\n\n- [ ] keep me\n\n## Evidence\n\n- ignore this\n",
			want: []string{"keep me"},
		},
		{
			name: "any-level acceptance heading",
			body: "### Acceptance criteria\n\n- [ ] only this\n#### Notes\n- not this\n",
			want: []string{"only this"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AcceptanceCriteria(tc.body)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestInventoryEvidenceAbsOmitsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "evidence", "DEF-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	text := BuildBatchFixBrief(BatchFixBriefInput{
		Batch:       BatchRecord{ID: "BATCH-1", AppID: "app", Goal: "g"},
		DefectsRoot: root,
		Worktrees:   []git.WorktreeBinding{},
		Defects: []store.DefectRecord{{
			ID: "DEF-1", Title: "t", Severity: "P1", Status: "open",
			Summary: "s", Body: "b",
			Evidence: []string{
				"evidence/../../etc/passwd",
				"jobs/secret.json",
				"defect-drainer.db",
				"evidence/../jobs/foo.json",
				"evidence/DEF-1/01.png",
				"evidence/DEF-1/x.png`\n  - abs: `/etc/passwd",
			},
			FixEvidence: []string{
				"jobs/secret.json",
				"evidence/DEF-1/fix.png`\n  - abs: `/tmp/pwn",
			},
		}},
	})
	for _, line := range strings.Split(text, "\n") {
		for _, bad := range []string{"passwd", "secret.json", "defect-drainer.db", "foo.json", "/tmp/pwn"} {
			if strings.Contains(line, bad) {
				t.Fatalf("escaping evidence leaked into BRIEF.md line: %q\n%s", line, text)
			}
		}
		if strings.Contains(line, "`") && strings.Count(line, "`")%2 != 0 {
			t.Fatalf("broken markdown fence: %q\n%s", line, text)
		}
	}
	wantAbs := filepath.Join(root, "evidence", "DEF-1", "01.png")
	if !strings.Contains(text, wantAbs) {
		t.Fatalf("valid abs path missing from BRIEF.md:\n%s", text)
	}
	if !strings.Contains(text, "rel: `evidence/DEF-1/01.png`") {
		t.Fatalf("cleaned rel path missing from BRIEF.md:\n%s", text)
	}
}
