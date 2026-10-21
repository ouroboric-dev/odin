package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeDiff struct {
	delta Delta
	err   error
	calls int
}

func (f *fakeDiff) Diff(context.Context, []byte, []byte) (Delta, error) {
	f.calls++
	return f.delta, f.err
}

// An index with three contracts: one an extracted leaf reads, one only a guess reads, one nobody reads.
func contractIndex() *Impact {
	return &Impact{Schema: ImpactSchema, Bound: "lower", Contracts: []ContractEntry{
		{ID: "contract:api/a.openapi.yaml", Path: "api/a.openapi.yaml", Owner: "api", Readers: Reached{Extracted: []string{"web"}}},
		{ID: "contract:api/b.openapi.yaml", Path: "api/b.openapi.yaml", Owner: "api", Readers: Reached{Inferred: []string{"worker"}}},
		{ID: "contract:api/c.openapi.yaml", Path: "api/c.openapi.yaml", Owner: "api"},
	}}
}

func change(p string) Change { return Change{Path: p, Old: []byte("old"), New: []byte("new")} }

func TestJudgeByTheEvidenceOfTheIndex(t *testing.T) {
	cases := []struct {
		name     string
		changes  []Change
		differ   Differ
		verdict  Verdict
		evidence Evidence
	}{
		{"no contract file changed", []Change{change("api/main.go"), change("web/src/app.ts")}, nil, Pass, Extracted},
		{"a contract an extracted edge reaches", []Change{change("api/a.openapi.yaml")}, nil, Fail, Extracted},
		{"a contract only a guess reaches", []Change{change("api/b.openapi.yaml")}, nil, Escalate, Inferred},
		{"a contract nobody reads", []Change{change("api/c.openapi.yaml")}, nil, Pass, Extracted},
		{"a contract file the index does not list", []Change{change("api/new.openapi.yaml")}, nil, Escalate, Inferred},
		{"the worst verdict wins", []Change{change("api/a.openapi.yaml"), change("api/b.openapi.yaml")}, nil, Escalate, Inferred},
		{"oasdiff finds a change", []Change{change("api/a.openapi.yaml")}, &fakeDiff{delta: Delta{Changed: true, Breaking: true}}, Fail, Oasdiff},
		{"oasdiff finds no change: only the layout moved", []Change{change("api/a.openapi.yaml")}, &fakeDiff{}, Pass, Oasdiff},
		{"oasdiff proves a change in a file the index does not list", []Change{change("api/new.openapi.yaml")}, &fakeDiff{delta: Delta{Changed: true}}, Fail, Oasdiff},
		{"oasdiff fails: the index decides", []Change{change("api/a.openapi.yaml")}, &fakeDiff{err: errors.New("boom")}, Fail, Extracted},
		{"oasdiff fails on an unlisted file: a guess", []Change{change("api/new.openapi.yaml")}, &fakeDiff{err: errors.New("boom")}, Escalate, Inferred},
	}
	for _, c := range cases {
		j := Judge(context.Background(), contractIndex(), c.changes, c.differ)
		if j.Verdict != c.verdict || j.Evidence != c.evidence || j.Reason == "" {
			t.Errorf("%s: %+v; want %s on %s evidence", c.name, j, c.verdict, c.evidence)
		}
	}
}

// Review focus: an inferred edge never gives pass. A change that only a guess can see goes to a person.
func TestAnInferredOnlyEdgeNeverGivesPass(t *testing.T) {
	imp := contractIndex()
	for _, d := range []Differ{nil, &fakeDiff{err: errors.New("no oasdiff")}} {
		if j := Judge(context.Background(), imp, []Change{change("api/b.openapi.yaml")}, d); j.Verdict != Escalate {
			t.Errorf("verdict %s on inferred-only evidence, want escalate", j.Verdict)
		}
	}
	// Even when the guess is the only link of a new copy.
	if j := Judge(context.Background(), &Impact{}, []Change{{Path: "web/x.openapi.yaml", New: []byte("n")}}, nil); j.Verdict == Pass {
		t.Error("a contract the index never heard of passed")
	}
}

// oasdiffStandIn models the real oasdiff (v1.x): `diff` always prints a report, and with --fail-on-diff exits 0
// when the specs do not differ and 1 when they do; `breaking --fail-on ERR` exits 1 on a breaking change; any
// other exit is an error (here: a new file that says BROKEN). It logs its arguments next to itself.
func oasdiffStandIn(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "oasdiff")
	// oasdiff reads two files an agent's leaf wrote; it gets PATH and nothing else of Odin's environment.
	t.Setenv("YGG_TEST_SECRET", "must not reach oasdiff")
	script := `#!/bin/sh
[ -z "${YGG_TEST_SECRET:-}" ] || { echo "the environment leaked" >&2; exit 9; }
echo "$@" >> "$(dirname "$0")/args.log"
cmd=$1; shift
for a; do old=$new; new=$a; done
grep -q BROKEN "$new" && { echo "load failed" >&2; exit 102; }
case $cmd in
diff) echo "info: {}"; cmp -s "$old" "$new" || exit 1;;
breaking) grep -q BREAKING "$new" && exit 1;;
*) exit 3;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestOasdiffCLIAsksDiffAndBreaking(t *testing.T) {
	bin := oasdiffStandIn(t)
	d := OasdiffCLI{Bin: bin}
	for _, c := range []struct {
		old, new string
		want     Delta
	}{
		{"a", "a", Delta{}}, // the report is never empty, yet no difference is no change
		{"a", "b", Delta{Changed: true}},
		{"a", "BREAKING", Delta{Changed: true, Breaking: true}},
	} {
		got, err := d.Diff(context.Background(), []byte(c.old), []byte(c.new))
		if err != nil || got != c.want {
			t.Errorf("Diff(%q, %q) = %+v, %v; want %+v", c.old, c.new, got, err, c.want)
		}
	}
	if _, err := d.Diff(context.Background(), []byte("a"), []byte("BROKEN")); err == nil {
		t.Error("an error exit of oasdiff is not an error")
	}
	if _, err := (OasdiffCLI{Bin: filepath.Join(t.TempDir(), "missing")}).Diff(context.Background(), nil, nil); err == nil {
		t.Error("a missing binary is not an error")
	}
	// The flag set is what the oasdiff v1.33 source accepts: diff --fail-on-diff, no external $refs.
	log, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "args.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		var want string
		switch {
		case strings.HasPrefix(line, "diff "):
			want = "diff --fail-on-diff --allow-external-refs=false old.yaml new.yaml"
		case strings.HasPrefix(line, "breaking "):
			want = "breaking --fail-on ERR --allow-external-refs=false old.yaml new.yaml"
		}
		if line != want {
			t.Errorf("oasdiff was run as %q, want %q", line, want)
		}
	}
}

// With the real exit codes the judge can reach every branch: no difference is the layout-only pass, a
// difference is a fail on oasdiff evidence, and an error leaves the decision to the index (never a pass of a guess).
func TestJudgeWithTheRealOasdiffExitCodes(t *testing.T) {
	d := OasdiffCLI{Bin: oasdiffStandIn(t)}
	imp := contractIndex()
	for _, c := range []struct {
		name     string
		ch       Change
		verdict  Verdict
		evidence Evidence
	}{
		{"layout only", Change{Path: "api/a.openapi.yaml", Old: []byte("a"), New: []byte("a")}, Pass, Oasdiff},
		{"a difference", Change{Path: "api/a.openapi.yaml", Old: []byte("a"), New: []byte("b")}, Fail, Oasdiff},
		{"an error on a listed guess", Change{Path: "api/b.openapi.yaml", Old: []byte("a"), New: []byte("BROKEN")}, Escalate, Inferred},
		{"an error on an unlisted file", Change{Path: "api/new.openapi.yaml", Old: []byte("a"), New: []byte("BROKEN")}, Escalate, Inferred},
	} {
		if j := Judge(context.Background(), imp, []Change{c.ch}, d); j.Verdict != c.verdict || j.Evidence != c.evidence {
			t.Errorf("%s: %+v; want %s on %s", c.name, j, c.verdict, c.evidence)
		}
	}
}

// An index with a contract at the root of a leaf (leaf-app) and one in a subfolder of another (leaf-web).
func folderIndex() *Impact {
	return &Impact{Schema: ImpactSchema, Bound: "lower", Contracts: []ContractEntry{
		{ID: "contract:leaf-app/openapi.yaml", Path: "leaf-app/openapi.yaml", Owner: "leaf-app"},
		{ID: "contract:leaf-web/contracts/api.openapi.yaml", Path: "leaf-web/contracts/api.openapi.yaml", Owner: "leaf-web"},
	}}
}

func modeChange(p, old, new string) Change {
	c := Change{Path: p, OldMode: old, NewMode: new}
	if new != "" {
		c.New = []byte("new")
	}
	if old != "" {
		c.Old = []byte("old")
	}
	return c
}

func TestJudgeEscalatesOnTheFolderOfAListedContract(t *testing.T) {
	const reg, link = "100644", "120000"
	cases := []struct {
		name string
		ch   Change
		want Verdict
	}{
		{"a new YAML file in a contract's folder", modeChange("leaf-web/contracts/user.yaml", "", reg), Escalate},
		{"a changed yml file", modeChange("leaf-web/contracts/user.yml", reg, reg), Escalate},
		{"a changed JSON file", modeChange("leaf-web/contracts/user.json", reg, reg), Escalate},
		{"the extension is not case sensitive", modeChange("leaf-web/contracts/user.YAML", reg, reg), Escalate},
		{"a deleted YAML file", modeChange("leaf-web/contracts/user.yaml", reg, ""), Escalate},
		{"a changed markdown file", modeChange("leaf-web/contracts/README.md", reg, reg), Pass},
		{"a changed YAML file outside every folder", modeChange("leaf-web/src/config.yaml", reg, reg), Pass},
		{"a YAML file in a folder of another leaf", modeChange("other/contracts/user.yaml", reg, reg), Pass},
		{"a folder whose name only starts like the contract's", modeChange("leaf-web/contracts-old/user.yaml", reg, reg), Pass},
		{"a subfolder of a contract's subfolder", modeChange("leaf-web/contracts/schemas/user.yaml", reg, reg), Escalate},
		{"a YAML file directly in a leaf's root", modeChange("leaf-app/sqlc.yaml", reg, reg), Escalate},
		{"a YAML file below a contract at a leaf's root", modeChange("leaf-app/.github/ci.yml", reg, reg), Pass},
		{"a JSON file below a contract at a leaf's root", modeChange("leaf-app/.devcontainer/devcontainer.json", reg, reg), Pass},
		{"a symlink with any name in the folder", modeChange("leaf-web/contracts/link", "", link), Escalate},
		{"a symlink named like a contract", modeChange("leaf-web/contracts/x.openapi.yaml", "", link), Escalate},
		{"a symlink below a contract in a subfolder", modeChange("leaf-web/contracts/deep/er/link.txt", "", link), Escalate},
		{"a file that stops being a symlink", modeChange("leaf-web/contracts/link.txt", link, reg), Escalate},
		{"a file that becomes a symlink", modeChange("leaf-web/contracts/link.txt", reg, link), Escalate},
		{"a symlink directly in a leaf's root", modeChange("leaf-app/link", "", link), Escalate},
		{"a symlink below a contract at a leaf's root", modeChange("leaf-app/sub/link", "", link), Pass},
		{"a symlink outside every folder", modeChange("leaf-web/src/link", "", link), Pass},
	}
	for _, c := range cases {
		j := Judge(context.Background(), folderIndex(), []Change{c.ch}, nil)
		if j.Verdict != c.want {
			t.Errorf("%s: %+v; want %s", c.name, j, c.want)
		}
		if c.want == Escalate && (j.Evidence != Inferred || !strings.Contains(j.Reason, c.ch.Path) || len(j.Contracts) != 1) {
			t.Errorf("%s: %+v; want inferred evidence, a reason naming %s and the file listed", c.name, j, c.ch.Path)
		}
	}
}

func TestTheFolderReasonNamesTheRule(t *testing.T) {
	yaml := Judge(context.Background(), folderIndex(), []Change{modeChange("leaf-web/contracts/user.yaml", "", "100644")}, nil)
	link := Judge(context.Background(), folderIndex(), []Change{modeChange("leaf-web/contracts/l", "", "120000")}, nil)
	if !strings.Contains(yaml.Reason, "YAML or JSON") || !strings.Contains(link.Reason, "symlink") {
		t.Errorf("the reasons don't name the rule: %q, %q", yaml.Reason, link.Reason)
	}
}

// Without a listed contract there is no folder to watch; an absent index lists nothing.
func TestNoFolderWithoutAListedContract(t *testing.T) {
	for _, imp := range []*Impact{nil, {}} {
		j := Judge(context.Background(), imp, []Change{modeChange("leaf-web/contracts/user.yaml", "100644", "100644"), modeChange("leaf-web/l", "", "120000")}, nil)
		if j.Verdict != Pass {
			t.Errorf("index %v: %+v; want pass", imp, j)
		}
	}
}

// A file named like a contract that is a regular file stays on its present path: a changed openapi.yaml
// at a listed contract's root gives the verdict the index and oasdiff give, not a folder escalation.
func TestAContractFileKeepsItsVerdictInItsFolder(t *testing.T) {
	imp := folderIndex()
	if j := Judge(context.Background(), imp, []Change{modeChange("leaf-app/openapi.yaml", "100644", "100644")}, nil); j.Verdict != Pass {
		t.Errorf("a listed contract nobody reads: %+v; want pass", j)
	}
	both := []Change{modeChange("leaf-app/openapi.yaml", "100644", "100644"), modeChange("leaf-app/sqlc.yaml", "100644", "100644")}
	if j := Judge(context.Background(), imp, both, nil); j.Verdict != Escalate || len(j.Contracts) != 1 || j.Contracts[0] != "leaf-app/sqlc.yaml" {
		t.Errorf("the contract and a sibling: %+v; want escalate on the sibling", j)
	}
}
