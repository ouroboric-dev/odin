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
