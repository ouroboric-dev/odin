package metrics

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/leaf"
	"github.com/ouroboric-dev/odin/internal/tree"
)

// Lines is what a task's diff added, by kind: tests, generated files, and the rest, written by hand.
type Lines struct {
	HandWritten int `json:"hand_written"`
	Tests       int `json:"tests"`
	Generated   int `json:"generated"`
}

// headLines is how far into a file a generated-code marker counts.
const headLines = 5

// sizes gives each task the plan's estimate, the loop attempts it took, its findings by source and severity (raised
// and closed, from the finding lines of its nodes), and for a task that is done the lines it
// added: git diff --numstat in its leaf, from where its loop started (the loop record's base) to the commit its
// merge landed (the merge node's node_finished line). A pointer node's line names the umbrella's commit, which is
// not the leaf's, and is passed over. A leaf that can't be read leaves the task without lines, with a warning.
func (r *Run) sizes(state *record, events []gungnir.Event) {
	if len(r.Tasks) != len(state.Features) { // a run with no ledger lines lists no tasks
		return
	}
	slugs := taskSlugs(state)
	pointers := map[string]bool{}
	for _, e := range events {
		if e.Name == gungnir.EventPointerMoved {
			pointers[e.Node] = true
		}
	}
	attempts, landed := map[string]int{}, map[string]string{}
	findings, unknown := map[string]map[[2]string]*TaskFindings{}, map[string]int{} // task: (source, severity): counts
	for _, e := range events {
		switch e.Name {
		case gungnir.EventFindingRaised, gungnir.EventFindingClosed:
			t := e.Task
			if t == "" {
				t = taskOf(e.Node, slugs)
			}
			if t == "" {
				continue
			}
			if e.FindingSource == "" || severityRank(e.FindingSeverity) < 0 {
				unknown[t]++
				continue
			}
			if findings[t] == nil {
				findings[t] = map[[2]string]*TaskFindings{}
			}
			k := [2]string{e.FindingSource, e.FindingSeverity}
			c := findings[t][k]
			if c == nil {
				c = &TaskFindings{Source: e.FindingSource, Severity: e.FindingSeverity}
				findings[t][k] = c
			}
			if e.Name == gungnir.EventFindingRaised {
				c.Raised++
			} else {
				c.Closed++
			}
		case gungnir.EventLoopAttempt:
			if t := taskOf(e.Node, slugs); t != "" {
				attempts[t] = max(attempts[t], e.Attempt)
			}
		case gungnir.EventNodeFinished:
			if e.Task != "" && e.Commit != "" && !pointers[e.Node] {
				landed[e.Task] = e.Commit
			}
		}
	}
	bases := map[string]string{}
	for node, l := range state.Loops {
		if t := taskOf(node, slugs); t != "" && l != nil && l.Base != "" {
			bases[t] = l.Base
		}
	}
	for i, f := range state.Features {
		t, slug := &r.Tasks[i], f.Slug()
		switch f.Size {
		case schema.SizeS, schema.SizeM, schema.SizeL:
			t.Size = f.Size
		}
		t.Attempts = attempts[slug]
		for _, c := range findings[slug] {
			t.Findings = append(t.Findings, *c)
		}
		slices.SortFunc(t.Findings, func(a, b TaskFindings) int {
			return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(severityRank(a.Severity), severityRank(b.Severity)))
		})
		if n := unknown[slug]; n > 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("task %s: %d finding lines name no source or no known severity, and are not counted", f.ID, n))
		}
		land, base := landed[slug], bases[slug]
		if f.Status != schema.TaskDone || land == "" || base == "" {
			continue
		}
		lines, err := diffLines(context.Background(), state, f.Layer, base, land)
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("task %s has no line counts: %v", f.ID, err))
			continue
		}
		t.Lines = &lines
	}
}

// diffLines counts the lines added in the leaf at layer from base to land. The leaf is found in the tree as it
// is, and nothing is checked out.
func diffLines(ctx context.Context, state *record, layer, base, land string) (Lines, error) {
	want := filepath.ToSlash(filepath.Join(state.ProjectPath, layer))
	var found *tree.Node
	errFound := errors.New("found")
	err := tree.Walk(ctx, state.Root, func(n tree.Node) error {
		if filepath.ToSlash(n.Path) == want {
			found = &n
			return errFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFound) {
		return Lines{}, err
	}
	if found == nil {
		return Lines{}, fmt.Errorf("the tree at %s has no leaf %s", state.Root, want)
	}
	l, err := leaf.Open(*found)
	if err != nil {
		return Lines{}, err
	}
	out, err := l.Output(ctx, "diff", "--numstat", "-z", "--no-renames", base, land, "--")
	if err != nil {
		return Lines{}, err
	}
	var n Lines
	for rec := range bytes.SplitSeq(out, []byte{0}) {
		added, rest, ok := strings.Cut(string(rec), "\t")
		if !ok {
			continue
		}
		_, name, ok := strings.Cut(rest, "\t")
		if !ok || added == "-" { // a binary file has no lines
			continue
		}
		count, err := strconv.Atoi(added)
		if err != nil {
			return Lines{}, fmt.Errorf("git diff --numstat gave %q", rec)
		}
		switch {
		case count == 0:
		case generated(ctx, l, land, name):
			n.Generated += count
		case isTest(name):
			n.Tests += count
		default:
			n.HandWritten += count
		}
	}
	return n, nil
}

// severityRank orders the severities, the gravest first; -1 is a severity the record does not know.
func severityRank(s string) int {
	return slices.Index([]gungnir.Severity{gungnir.SeverityBlocker, gungnir.SeverityMajor, gungnir.SeverityMinor}, gungnir.Severity(s))
}

// isTest reports whether the file at name is a test: a Go or TypeScript test file, or any file under an e2e folder.
func isTest(name string) bool {
	base := path.Base(name)
	for _, s := range []string{"_test.go", ".test.ts", ".test.tsx", ".spec.ts"} {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return slices.Contains(strings.Split(path.Dir(name), "/"), "e2e")
}

// generated reports whether the file at name is generated: a lock file, or a file whose first lines, at commit,
// say "Code generated" or "DO NOT EDIT".
func generated(ctx context.Context, l *leaf.Leaf, commit, name string) bool {
	switch path.Base(name) {
	case "package-lock.json", "go.sum":
		return true
	}
	errEnough := errors.New("enough")
	var head []string
	err := l.Pipe(ctx, func(out io.Reader) error {
		sc := bufio.NewScanner(io.LimitReader(out, 64<<10))
		sc.Buffer(make([]byte, 0, 4096), 64<<10+1)
		for len(head) < headLines && sc.Scan() {
			head = append(head, sc.Text())
		}
		return errEnough // the rest of the file is not needed: stop git
	}, "cat-file", "blob", commit+":"+name)
	if err != nil && !errors.Is(err, errEnough) {
		return false
	}
	for _, line := range head {
		if strings.Contains(line, "Code generated") || strings.Contains(line, "DO NOT EDIT") {
			return true
		}
	}
	return false
}
