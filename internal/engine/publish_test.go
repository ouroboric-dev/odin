package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

const publishNodes = "  - { id: design, kind: agent, role: planner, prompt: p.md, scratch: true }\n  - { id: design-gate, kind: gate, gate: design }\n" +
	"  - { id: design-docs, kind: publish, from: design, to: docs }\n"

const publishScript = "steps:\n  design:\n    - actions:\n        - write: { path: out/1-product/design.md, content: \"# Design\\n\" }\n        - write: { path: out/leaves.yaml, content: \"leaves: {}\\n\" }\n" +
	"      result: { status: ok, summary: wrote the design }\n"

func headFiles(t *testing.T, dir string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, "show", "--name-only", "--format=", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPublishCopiesWhatAScratchAgentWroteIntoTheProjectAndCommitsIt(t *testing.T) {
	r := start(t, "unattended", "gates:\n  design: auto\n", publishNodes, publishScript)
	before, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if b, err := os.ReadFile(filepath.Join(r.Project, "docs", "1-product", "design.md")); err != nil || string(b) != "# Design\n" {
		t.Errorf("docs/1-product/design.md = %q, %v", b, err)
	}
	if got := headFiles(t, r.Project); got != "docs/1-product/design.md\ndocs/leaves.yaml" {
		t.Errorf("the commit holds %q; want the two documents, by their paths", got)
	}
	if msg, _ := git.Run(bg, r.Project, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, "Publish design's documents (run "+r.State.ID+", node design-docs)") {
		t.Errorf("commit message = %q", msg)
	}
	if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); after == before {
		t.Error("nothing was committed")
	}
	if st, _ := git.Run(bg, r.Project, "status", "--porcelain", "--", "docs"); st != "" {
		t.Errorf("docs/ is left uncommitted: %q", st)
	}
	if text := ledgerText(t, r); !strings.Contains(text, `"event":"docs_published"`) || !strings.Contains(text, "2 file(s) under docs") {
		t.Errorf("the ledger lacks docs_published:\n%s", text)
	}
}

func TestPublishingTheSameDocumentsAgainCommitsNothing(t *testing.T) {
	r := start(t, "unattended", "gates:\n  design: auto\n", publishNodes, publishScript)
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	head, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	n := r.Nodes()[2]
	if err := runPublish(bg, r, n); err != nil {
		t.Fatal(err)
	}
	if again, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); again != head {
		t.Error("publishing identical documents made a commit")
	}
	if got := r.State.Summaries["design-docs"]; !strings.Contains(got, "unchanged") {
		t.Errorf("summary = %q", got)
	}
}

func TestPublishRefusesWhatIsNotADocument(t *testing.T) {
	out := t.TempDir()
	put := func(rel, body string) {
		p := filepath.Join(out, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("docs/ok.md", "fine\n")
	if files, _, err := listDocuments(out); err != nil || strings.Join(files, ",") != "docs/ok.md" {
		t.Fatalf("listDocuments() = %v, %v", files, err)
	}
	cases := map[string]func(){
		"a script":             func() { put("run.sh", "rm -rf /\n") },
		"a hidden file":        func() { put(".env", "KEY=1\n") },
		"a hidden folder":      func() { put(".github/workflows/x.md", "x\n") },
		"a name with a space":  func() { put("a b.md", "x\n") },
		"a name with a dollar": func() { put("$(x).md", "x\n") },
		"a symlink":            func() { os.Symlink("/etc/passwd", filepath.Join(out, "docs", "link.md")) },
		"a big file":           func() { put("big.md", strings.Repeat("x", maxPublishFile+1)) },
		"an upper case ending is still an ending": nil,
	}
	delete(cases, "an upper case ending is still an ending")
	for name, add := range cases {
		os.RemoveAll(out)
		os.MkdirAll(out, 0o755)
		put("docs/ok.md", "fine\n")
		add()
		if _, _, err := listDocuments(out); err == nil {
			t.Errorf("%s: listDocuments() = nil error", name)
		}
	}
	os.RemoveAll(out)
	os.MkdirAll(out, 0o755)
	for i := 0; i <= maxPublishFiles; i++ {
		put("d/f"+strings.Repeat("a", 1+i%20)+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))+string(rune('a'+(i/676)%26))+".md", "x\n")
	}
	if _, _, err := listDocuments(out); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("too many files: %v", err)
	}
}

func TestPublishWontCommitOverStagedWorkItDidNotMake(t *testing.T) {
	r := start(t, "unattended", "gates:\n  design: auto\n", publishNodes, publishScript)
	os.WriteFile(filepath.Join(r.Project, "notes.txt"), []byte("mine\n"), 0o644)
	if _, err := git.Run(bg, r.Project, "add", "notes.txt"); err != nil {
		t.Fatal(err)
	}
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusFailed || !strings.Contains(out.Err.Error(), "staged changes Odin didn't make") {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if files := headFiles(t, r.Project); strings.Contains(files, "notes.txt") {
		t.Error("the operator's staged file was committed with the documents")
	}
}

func TestSecretsInTheDocumentsAreAHardStopAndNothingIsPublished(t *testing.T) {
	r, fake := startSandboxed(t, publishNodes, publishScript)
	fake.Scan.Exit, fake.Scan.Report = 7, secretReport
	head, _ := git.Run(bg, r.Project, "rev-parse", "HEAD")
	out, err := Advance(bg, r)
	if err != nil || out.Status != run.StatusStopped || r.State.PendingGate != gungnir.GateRealSecrets || out.Gate != "design-docs" {
		t.Fatalf("Advance() = %+v, %v, gate %s; want a stop at real-secrets-or-user-data", out, err, r.State.PendingGate)
	}
	if after, _ := git.Run(bg, r.Project, "rev-parse", "HEAD"); after != head {
		t.Error("documents with a secret in them were committed")
	}
	if _, err := os.Stat(filepath.Join(r.Project, "docs")); err == nil {
		t.Error("documents with a secret in them were copied into the project")
	}
	if len(fake.ScanFiles) != 1 || strings.Join(fake.ScanFiles[0], ",") != "out/1-product/design.md,out/leaves.yaml" {
		t.Errorf("the scan was given %q; want the two documents, as paths in the workspace", fake.ScanFiles)
	}
	b, _ := os.ReadFile(filepath.Join(r.Dir, "gates", "design-docs-1.md"))
	if !strings.Contains(string(b), "Nothing was published") || !strings.Contains(string(b), "scratch") {
		t.Errorf("the gate file = %s", b)
	}
	fake.Scan.Exit, fake.Scan.Report = 0, ""
	if err := Approve(bg, r, "design-docs", "a test key, removed"); err != nil {
		t.Fatal(err)
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() after approval = %+v, %v", out, err)
	}
	if got := headFiles(t, r.Project); !strings.Contains(got, "docs/1-product/design.md") {
		t.Errorf("after approval the commit holds %q", got)
	}
}

// What is published is what the secret scan read: Odin reads each document once, gives the scan those
// bytes, and writes those bytes, so a file changed in out/ after the scan read it is not what lands in the
// project (review M-2).
func TestPublishCommitsTheBytesTheScanRead(t *testing.T) {
	r, fake := startSandboxed(t, publishNodes, publishScript)
	scanned := map[string]string{}
	fake.OnScan = func(env gungnir.Env, c gungnir.Cmd, files []string) {
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(env.Workdir, c.Dir, f))
			if err != nil {
				t.Errorf("the scan can't read %s: %v", f, err)
			}
			scanned[f] = string(b)
		}
		if err := os.WriteFile(filepath.Join(r.ScratchDir("design"), "out", "1-product", "design.md"), []byte("# Changed after the scan\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := Advance(bg, r); err != nil || out.Status != run.StatusDone {
		t.Fatalf("Advance() = %+v, %v", out, err)
	}
	if got := scanned["out/1-product/design.md"]; got != "# Design\n" {
		t.Errorf("the scan read %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Project, "docs", "1-product", "design.md")); string(b) != scanned["out/1-product/design.md"] {
		t.Errorf("published %q; the scan read %q", b, scanned["out/1-product/design.md"])
	}
}
