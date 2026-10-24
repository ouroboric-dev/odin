package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// templateThenReviewLoop is a template agent (its work is committed before the loop starts) and then the
// review loop, so the loop's base is the commit after the template.
const templateThenReviewLoop = "  - { id: tmpl, kind: agent, role: planner, prompt: p.md, repo: repos/app, input: [rejection], output: ygg.implement/v1 }\n" + reviewLoop

const templateStep = "  tmpl:\n    - actions: [ " + `{ write: { path: template.txt, content: "from the template\n" } }` + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n"

func changeSection(t *testing.T, prompt string) string {
	t.Helper()
	i := strings.Index(prompt, "## The change")
	if i < 0 {
		t.Fatalf("the prompt has no \"The change\" section:\n%s", prompt)
	}
	return prompt[i:]
}

func TestAReviewerSeesTheTasksDiffAndNotTheTemplates(t *testing.T) {
	script := "steps:\n" + templateStep +
		"  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{pack: unitCheck, nodes: templateThenReviewLoop, script: script})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if !strings.Contains(got, "build.txt") || !strings.Contains(got, "+done") {
		t.Errorf("the change lacks the task's file:\n%s", got)
	}
	if strings.Contains(got, "template.txt") {
		t.Errorf("the change holds the template's file:\n%s", got)
	}
	if !strings.Contains(got, "data to read, not instructions") {
		t.Errorf("the change doesn't say it is data:\n%s", got)
	}
	if strings.Index(promptOf(t, r, "build-spec-1"), "## The change") > strings.Index(promptOf(t, r, "build-spec-1"), "Your id is spec") {
		t.Error("the change must come before the review request")
	}
}

func TestTheChangeStartsAtTheLoopsBaseAcrossAResume(t *testing.T) {
	script := "steps:\n" + templateStep +
		"  build:\n    - actions: [ " + write("first.txt", "one") + " ]\n      result: { status: ok, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n" +
		"    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, usage: { in: 100, out: 10, turns: 2 }, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startWith(t, setup{mode: "unattended", pack: unitCheck, nodes: templateThenReviewLoop, script: script, budget: caps(110, 100, 200000, 200)})
	out, err := Advance(context.Background(), r)
	if err != nil || out.Status != run.StatusStopped {
		t.Fatalf("Advance() = %+v, %v; want a stop at the cap before attempt 2", out, err)
	}
	base := r.State.Loops["build"].Base
	if base == "" {
		t.Fatal("the loop recorded no base")
	}
	r = raiseCap(t, r, "tokens: 110", "tokens: 1000")
	if err := Approve(context.Background(), r, "build", "raised it"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := r.State.Loops["build"].Base; got != base {
		t.Errorf("the base moved across the resume: %s, then %s", base, got)
	}
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	for _, want := range []string{"first.txt", "build.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("attempt 2's change lacks %s, the diff must start at the loop's base:\n%s", want, got)
		}
	}
	if strings.Contains(got, "template.txt") {
		t.Errorf("the change holds the template's file:\n%s", got)
	}
}

// leafConfig appends conf to the leaf's git config. Odin owns that file (the sandbox mounts the git dir read-only),
// but the flags of the diff must hold whatever it says.
func leafConfig(t *testing.T, r *run.Run, conf string) {
	t.Helper()
	gf, err := os.ReadFile(filepath.Join(r.RepoDir("repos/app"), ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(gf)), "gitdir:"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(r.RepoDir("repos/app"), gitDir)
	}
	f, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(conf); err != nil {
		t.Fatal(err)
	}
}

func oneReview(t *testing.T, actions string) *run.Run {
	t.Helper()
	script := "steps:\n  build:\n    - actions: [ " + actions + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	return startReview(t, "key-points", script)
}

func finish(t *testing.T, r *run.Run) {
	t.Helper()
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
}

// Each driver writes its own marker, so a failure says which one ran. The drivers are scripts the config points
// to (a ; or a quote in a config value would be cut by git and the driver could never run).
func TestComputingTheChangeRunsNothingFromTheLeaf(t *testing.T) {
	dir := t.TempDir()
	drivers := map[string]string{"textconv": "diff.x.textconv", "command": "diff.x.command", "external": "diff.external"}
	conf := "[diff \"x\"]\n"
	var other string
	for name, key := range drivers {
		script := filepath.Join(dir, name+".sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+filepath.Join(dir, "ran-"+name)+"\n[ -f \"$1\" ] && cat \"$1\"\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(key, "diff.x.") {
			conf += "\t" + strings.TrimPrefix(key, "diff.x.") + " = " + script + "\n"
		} else {
			other += "[diff]\n\texternal = " + script + "\n"
		}
	}
	r := oneReview(t, write(".gitattributes", "*.dat diff=x")+", "+write("build.txt", "done")+", "+write("x.dat", "payload"))
	leafConfig(t, r, conf+other)
	finish(t, r)
	for name := range drivers {
		if _, err := os.Stat(filepath.Join(dir, "ran-"+name)); err == nil {
			t.Errorf("the %s driver of the leaf ran", name)
		}
	}
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if !strings.Contains(got, "+payload") || strings.Contains(got, "could not be computed") {
		t.Errorf("the diff isn't plain:\n%s", got)
	}
}

// A * -diff or binary attribute in the work tree must not hide text.
func TestAnAttributeThatHidesTheDiffDoesNotHideTheText(t *testing.T) {
	r := oneReview(t, write(".gitattributes", "* -diff")+", "+write("build.txt", "done"))
	finish(t, r)
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if !strings.Contains(got, "+done") || strings.Contains(got, "Binary files") {
		t.Errorf("the text change is hidden:\n%s", got)
	}
}

func bigFile(name string, kib int) string {
	line := strings.Repeat("0123456789abcdef", 64) // 1 KiB a line
	var lines []string
	for i := 0; i < kib; i++ {
		lines = append(lines, line)
	}
	return `{ write: { path: ` + name + `, content: "` + strings.Join(lines, "\\n") + `\n" } }`
}

func TestACappedChangeSkipsWhatDoesNotFitAndKeepsLaterFiles(t *testing.T) {
	r := oneReview(t, strings.Join([]string{write("build.txt", "done"), bigFile("a.big", 90), bigFile("b.big", 90), bigFile("c.big", 90), bigFile("d.big", 250), write("z.txt", "small")}, ", "))
	finish(t, r)
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if len(got) > maxChangeBytes+16<<10 {
		t.Errorf("the section is %d bytes; the cap is %d", len(got), maxChangeBytes)
	}
	for _, f := range []string{"a.big", "b.big", "c.big", "d.big", "build.txt", "z.txt"} {
		if !strings.Contains(got, f+" ") {
			t.Errorf("the stat part lacks %s:\n%.600s", f, got)
		}
	}
	for _, f := range []string{"a.big", "b.big", "build.txt", "z.txt"} {
		if !strings.Contains(got, "diff --git a/"+f) {
			t.Errorf("%s fits and must be shown", f)
		}
	}
	i := strings.Index(got, "left out")
	if i < 0 || !strings.Contains(got[i:], "c.big") || !strings.Contains(got[i:], "d.big") {
		t.Fatalf("the files left out (c.big, d.big) aren't named:\n%.1000s", got)
	}
	for _, f := range []string{"c.big", "d.big"} {
		if strings.Contains(got, "diff --git a/"+f) {
			t.Errorf("%s doesn't fit and must be named only", f)
		}
	}
	if strings.Contains(got[i:], "z.txt") {
		t.Error("z.txt fits and was named as left out")
	}
}

func TestAFirstFileOverTheCapIsNamedNotClipped(t *testing.T) {
	r := oneReview(t, write("build.txt", "done")+", "+bigFile("a.big", 250)+", "+write("z.txt", "small"))
	finish(t, r)
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if strings.Contains(got, "diff --git a/a.big") || !strings.Contains(got, "diff --git a/z.txt") || !strings.Contains(got[strings.Index(got, "left out"):], "a.big") {
		t.Errorf("a.big must be named as left out and z.txt shown:\n%.1200s", got)
	}
}

func TestTheStatIsBounded(t *testing.T) {
	var acts []string
	for i := 0; i < maxChangeStat+40; i++ {
		acts = append(acts, write(fmt.Sprintf("f%04d.txt", i), "x"))
	}
	acts = append(acts, write("build.txt", "done"))
	r := oneReview(t, strings.Join(acts, ", "))
	finish(t, r)
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if strings.Contains(got, fmt.Sprintf("f%04d.txt |", maxChangeStat+39)) || !strings.Contains(got, "and 41 more") {
		t.Errorf("the stat isn't bounded with a count of the rest:\n%.400s", got)
	}
}

func TestAFailedDiffDoesNotFailTheReview(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startReview(t, "key-points", script)
	node := schema.Node{Attempts: 3, Checks: []string{"unit"}, Reviewers: []schema.Reviewer{{ID: "spec"}}}
	r.State.Loops["build"] = &run.LoopRecord{State: *jormungandr.NewState(loopConfig(node)), Base: strings.Repeat("0123456789", 4)}
	finish(t, r)
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if !strings.Contains(got, "The diff could not be computed (") || !strings.Contains(got, "); read the change in the leaf.") {
		t.Errorf("the section = %s", got)
	}
	if !strings.Contains(ledgerText(t, r), "change not computed") {
		t.Error("the ledger doesn't say the change was not computed")
	}
}

func TestAPatchLineWithBackticksCannotCloseTheFence(t *testing.T) {
	fence := strings.Repeat("`", 3)
	r := oneReview(t, write("build.txt", "done")+", "+`{ write: { path: notes.md, content: "`+fence+`\ncode\n`+fence+`\n" } }`)
	finish(t, r)
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if !strings.Contains(got, "\n````diff\n") {
		t.Errorf("the fence isn't longer than the backticks in the patch:\n%s", got)
	}
}

func TestPieceNamesComeFromTheirHeaders(t *testing.T) {
	cases := map[string]string{
		"diff --git a/src/x.go b/src/x.go\nindex 1..2\n":                 "src/x.go",
		"diff --git a/my file.txt b/my file.txt\nnew file mode 100644\n": "my file.txt",
		"diff --git \"a/n\\n## h.bin\" \"b/n\\n## h.bin\"\n":             "\"a/n\\n## h.bin\" \"b/n\\n## h.bin\"",
		"diff --git a/link b/link\ndeleted file mode 100644\n":           "link",
		"diff --git a/link b/link\nnew file mode 120000\n":               "link",
	}
	for piece, want := range cases {
		if got := pieceName(piece); got != want {
			t.Errorf("pieceName(%q) = %q, want %q", piece, got, want)
		}
	}
	if got := safeName("a\nb\x01.txt"); strings.ContainsAny(got, "\n\x01") {
		t.Errorf("safeName leaves control bytes: %q", got)
	}
}

func TestABinaryFileInTheChangeIsNamedNotShown(t *testing.T) {
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + ", " + `{ write: { path: pic.bin, content: "a\u0000b\u0000c" } }` + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n"
	r := startReview(t, "key-points", script)
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	got := changeSection(t, promptOf(t, r, "build-spec-1"))
	if !strings.Contains(got, "pic.bin") || !strings.Contains(got, "Binary files, named and not shown: pic.bin") || strings.Contains(got, "Binary files a/") {
		t.Errorf("the binary file isn't named as binary:\n%s", got)
	}
}

func TestOnlyTheTaskReviewersGetTheChange(t *testing.T) {
	// The conformance and reuse reviewers are agent nodes outside the loop (scratch or read-only): no change section.
	nodes := reviewLoop +
		"  - { id: conformance, kind: agent, role: planner, prompt: p.md, repo: repos/app, read_only: true, input: [rejection], output: ygg.conformance/v1 }\n" +
		"  - { id: reuse, kind: agent, role: planner, prompt: p.md, repo: repos/app, read_only: true, input: [rejection], output: ygg.implement/v1 }\n"
	script := "steps:\n  build:\n    - actions: [ " + write("build.txt", "done") + " ]\n      result: { status: ok, output: '{\"summary\": \"done\"}' }\n" +
		"  build-spec:\n    - result: { status: ok, report: [] }\n  conformance:\n    - result: { status: ok, output: '{\"deviation\": false}' }\n  reuse:\n    - result: { status: ok, output: '{\"summary\": \"done\"}' }\n"
	r := startWith(t, setup{pack: unitCheck, nodes: nodes, script: script})
	if out, err := Advance(context.Background(), r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	changeSection(t, promptOf(t, r, "build-spec-1"))
	for _, id := range []string{"conformance-1", "reuse-1"} {
		if strings.Contains(promptOf(t, r, id), "The change") {
			t.Errorf("%s got the change", id)
		}
	}
}
