package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestOasdiffCLIAsksChangelogAndBreaking(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "oasdiff")
	// oasdiff reads two files an agent's leaf wrote; it gets PATH and nothing else of Odin's environment.
	t.Setenv("YGG_TEST_SECRET", "must not reach oasdiff")
	script := `#!/bin/sh
[ -z "${YGG_TEST_SECRET:-}" ] || { echo "the environment leaked" >&2; exit 9; }
# a stand-in: "changelog" prints a line when the files differ; "breaking" exits 1 when the new file says BREAKING
cmd=$1; shift
for last; do :; done
case $cmd in
changelog) cmp -s "$1" "$2" || echo "changed";;
breaking) grep -q BREAKING "$last" && exit 1; exit 0;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	d := OasdiffCLI{Bin: bin}
	for _, c := range []struct {
		old, new string
		want     Delta
	}{
		{"a", "a", Delta{}},
		{"a", "b", Delta{Changed: true}},
		{"a", "BREAKING", Delta{Changed: true, Breaking: true}},
	} {
		got, err := d.Diff(context.Background(), []byte(c.old), []byte(c.new))
		if err != nil || got != c.want {
			t.Errorf("Diff(%q, %q) = %+v, %v; want %+v", c.old, c.new, got, err, c.want)
		}
	}
	if _, err := (OasdiffCLI{Bin: filepath.Join(t.TempDir(), "missing")}).Diff(context.Background(), nil, nil); err == nil {
		t.Error("a missing binary is not an error")
	}
}
