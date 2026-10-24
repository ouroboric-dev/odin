package views

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"go.yaml.in/yaml/v3"
)

var update = flag.Bool("update", false, "write the golden files anew")

// fixture decodes one of gungnir's ok fixtures of contract c (copied into testdata/fixtures from gungnir's
// schema/testdata/contracts).
func fixture[T any](t *testing.T, c schema.Contract, name string) T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "fixtures", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := schema.Decode[T](c, b)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

const day = "2026-10-24"

func TestEachViewMatchesItsGoldenFile(t *testing.T) {
	design := fixture[schema.Design](t, schema.ContractDesign, "design-ok-three-questions")
	spec := fixture[schema.Spec](t, schema.ContractSpec, "spec-ok-leaves-deviation")
	plan := fixture[schema.Plan](t, schema.ContractPlan, "plan-ok-two-tasks")
	reuse := fixture[schema.ReuseReview](t, schema.ContractReuseReview, "reuse-ok-one-item")
	toSpec := Front{Title: "Photo Share tasks", Date: day, Related: []string{"../../specs/2026-10-24-photo-share.md"}}
	for golden, render := range map[string]func() ([]byte, error){
		"design.md": func() ([]byte, error) {
			return Render(schema.ContractDesign, design, Front{Title: "Photo Share design", Date: day})
		},
		"spec.md": func() ([]byte, error) {
			return Render(schema.ContractSpec, &spec, Front{Title: "Photo Share spec", Date: day, Related: []string{"../../1-product/design/2026-10-24-photo-share.md"}})
		},
		"plan.md": func() ([]byte, error) {
			return Render(schema.ContractPlan, plan, Front{Title: "Photo Share plan", Date: day, Related: toSpec.Related})
		},
		"tasks.md": func() ([]byte, error) { return Tasks(plan, toSpec) },
		"reuse-review.md": func() ([]byte, error) {
			return Render(schema.ContractReuseReview, reuse, Front{Title: "Photo Share reuse review", Date: day, Related: []string{"../plans/2026-10-24-photo-share/plan.md"}})
		},
	} {
		got, err := render()
		if err != nil {
			t.Errorf("%s: %v", golden, err)
			continue
		}
		again, _ := render()
		if !bytes.Equal(got, again) {
			t.Errorf("%s: two renderings of the same document differ", golden)
		}
		path := filepath.Join("testdata", golden+".golden")
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%v (go test -run TestEachViewMatchesItsGoldenFile -update writes it)", err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s does not match %s:\n%s", golden, path, got)
		}
	}
}

func TestAViewRefusesAContractItHasNoViewForAndAValueOfAnotherType(t *testing.T) {
	if Has(schema.ContractImplement) || !Has(schema.ContractSpec) {
		t.Error("Has() is wrong: the design, the spec, the plan and the reuse review have views, and nothing else")
	}
	if _, err := Render(schema.ContractImplement, schema.Implement{Summary: "done"}, Front{Date: day}); err == nil || !strings.Contains(err.Error(), "no view") {
		t.Errorf("Render(implement) = %v", err)
	}
	if _, err := Render(schema.ContractSpec, schema.Design{}, Front{Date: day}); err == nil {
		t.Error("Render took a design as a spec")
	}
	if _, err := Render(schema.ContractDesign, schema.Design{}, Front{Date: "24 October"}); err == nil {
		t.Error("Render took a date that is not Odin's")
	}
}

// evil is a field that tries every way out of its place: front matter, headings, an underline, a quote, a list, a
// fence it leaves open, a pipe for a table and a run of backticks.
const evil = "x\n---\nstatus: accepted\n## Brief\n### evil\n  # also evil\n===\n> ## quoted\n- ## listed\n| a | b |\n````\nan open fence"

func TestAViewCannotBeBrokenByAField(t *testing.T) {
	title := "Photo Share\n---\nstatus: accepted"
	decided := []schema.Decided{{Date: day, Decision: evil, Reason: "a reason\n```\nits own open fence", Cites: []string{"a"}}}
	design := schema.Design{Title: evil, Slug: "x", Summary: evil, Goals: evil, NonGoals: evil, SuccessCriteria: evil, Context: evil,
		ProductAtUserLevel: evil, Decisions: decided, OpenQuestions: []schema.Question{{ID: "q1", Question: evil, WhoCanAnswer: evil}},
		Blocked: &gungnir.Blocked{Reason: evil}, Notes: []string{evil}}
	spec := schema.Spec{Title: evil, Slug: "x", Summary: evil, Architecture: evil, Interfaces: evil, DataModel: evil, AcceptanceCriteria: evil,
		Security: evil, Testing: evil, Decisions: decided, Deviations: []schema.Deviation{{What: evil, Where: evil, Why: evil, Cites: []string{"a"}}},
		Leaves: schema.Leaves{Runtime: &schema.RuntimePin{Realm: evil, RealmVersion: evil},
			Leaves: map[string]schema.LeafSpec{"repos/api": {Realm: evil, RealmVersion: evil, Description: evil, Consumes: []string{evil}}}},
		Notes: []string{evil}}
	plan := schema.Plan{Title: evil, Slug: "x", Goal: evil, Decisions: decided, Risks: []string{evil}, Notes: []string{evil},
		Tasks: []schema.PlanTask{{ID: "x-T001", Title: evil, Criteria: []string{evil, evil}, NamedTest: evil, Layer: evil, Size: evil,
			DependsOn: []string{evil}, Text: evil, Contract: evil}}}
	reuse := schema.ReuseReview{Verdict: evil, Items: []schema.ReuseItem{{Task: evil, Adds: evil, AlreadyDoneBy: evil, SmallestChange: evil}}, Notes: []string{evil}}
	front := Front{Title: title, Date: day, Related: []string{"a.md\n---\nstatus: accepted"}}
	views := map[string]func() ([]byte, error){
		"design":       func() ([]byte, error) { return Render(schema.ContractDesign, design, front) },
		"spec":         func() ([]byte, error) { return Render(schema.ContractSpec, spec, front) },
		"plan":         func() ([]byte, error) { return Render(schema.ContractPlan, plan, front) },
		"tasks":        func() ([]byte, error) { return Tasks(plan, front) },
		"reuse review": func() ([]byte, error) { return Render(schema.ContractReuseReview, reuse, front) },
	}
	for name, render := range views {
		b, err := render()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := string(b)
		// The front matter is Odin's: four keys, each one scalar or one list, whatever the fields hold.
		rest, ok := strings.CutPrefix(got, "---\n")
		matter, body, ok2 := strings.Cut(rest, "\n---\n")
		if !ok || !ok2 {
			t.Fatalf("%s: the view does not start with front matter:\n%s", name, got)
		}
		var keys map[string]yaml.Node
		var fm struct {
			Title, Status, Date string
			Related             []string
		}
		if err := yaml.Unmarshal([]byte(matter), &keys); err != nil {
			t.Fatalf("%s: the front matter is not YAML: %v\n%s", name, err, matter)
		}
		if err := yaml.Unmarshal([]byte(matter), &fm); err != nil {
			t.Fatalf("%s: the front matter is not YAML: %v\n%s", name, err, matter)
		}
		if fm.Status != "draft" || fm.Title != title || fm.Date != day || len(keys) != 4 {
			t.Errorf("%s: front matter = %+v, keys %d; want the title as one scalar and status: draft", name, fm, len(keys))
		}
		if len(fm.Related) != 1 || fm.Related[0] != front.Related[0] {
			t.Errorf("%s: related = %q", name, fm.Related)
		}
		// Outside a fence, the only lines that start a heading are the view's own, and no line opens front matter
		// or underlines a heading.
		var fence mdFence
		outside := map[string]bool{} // the view's own words after a field, which a fence the field left open would take in
		for _, line := range strings.Split(body, "\n") {
			if fence.line(line) {
				continue
			}
			outside[strings.TrimSpace(line)] = true
			t2 := strings.TrimLeft(stripMarkers(line), " ")
			switch {
			case len(line)-len(strings.TrimLeft(line, " ")) >= 4:
			case strings.HasPrefix(t2, "#") && !ownHeading(line):
				t.Errorf("%s: a field's line is a heading of its own: %q", name, line)
			case strings.HasPrefix(t2, "---") || strings.Trim(t2, "=") == "" && t2 != "":
				t.Errorf("%s: a field's line can start front matter or underline a heading: %q", name, line)
			}
		}
		for _, own := range map[string][]string{"design": {"(Owner decisions: a)", "(Who can answer: x --- status: accepted ## Brief ### evil   # also evil === > ## quoted - ## listed | a | b | ```` an open fence)"},
			"spec": {"(Owner decisions: a)", "- Owner decisions: a"}, "plan": {"(Owner decisions: a)"}, "tasks": {"- **Named test:** x --- status: accepted ## Brief ### evil   # also evil === > ## quoted - ## listed | a | b | ```` an open fence"},
			"reuse review": {"## Notes"}}[name] {
			if !outside[own] {
				t.Errorf("%s: %q is not outside a fence: a field's open fence took it in:\n%s", name, own, got)
			}
		}
		if fence.open != "" {
			t.Errorf("%s: a fence is left open (%q), so what follows it reads as code:\n%s", name, fence.open, got)
		}
		// A task's row of the plan's table keeps its four cells.
		if _, row, ok := strings.Cut(got, "| --- | --- | --- | --- |\n"); ok {
			row, _, _ = strings.Cut(row, "\n")
			if cells := len(regexp.MustCompile(`(^|[^\\])\|`).FindAllString(row, -1)); cells != 5 {
				t.Errorf("%s: the task's row has %d unescaped pipes, want 5: %q", name, cells, row)
			}
		} else if name == "plan" {
			t.Errorf("plan: no table of the order:\n%s", got)
		}
	}
}

// ownHeading reports whether line is one of the views' own headings: a section, or a task's, whose text is one
// line of its id and title.
func ownHeading(line string) bool {
	own := []string{"## Blocked", "## Summary", "## Goals, non-goals and success criteria", "### Goals", "### Non-goals", "### Success criteria",
		"## Context and constraints", "## The product at user level", "## Decisions", "## Open questions", "## Notes",
		"## Architecture and components", "## Interfaces and contracts", "## Data model", "## Acceptance criteria per feature",
		"## Security considerations", "## Testing approach", "## Deviations from the design doc", "## Leaves", "## Goal", "## Order",
		"## Risks", "### The text", "### The contract", "## Verdict", "## Findings"}
	return slices.Contains(own, line) || taskHeading.MatchString(line)
}

var taskHeading = regexp.MustCompile(`^## [a-z0-9-]+-T[0-9]{3}: [^\n]*$`)

// mdFence follows the code fences of a document as CommonMark does: an opener is three or more backticks or tildes
// after at most three spaces, and a backtick opener's info string holds no backtick; a closer is a run of the same
// character, at least as long, with nothing after it. It returns whether line is inside a fence or is one.
type mdFence struct{ open string }

func (f *mdFence) line(line string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || t == "" || (t[0] != '`' && t[0] != '~') {
		return f.open != ""
	}
	n := len(t) - len(strings.TrimLeft(t, t[:1]))
	run, rest := t[:n], t[n:]
	switch {
	case n < 3:
	case f.open == "" && !(run[0] == '`' && strings.Contains(rest, "`")):
		f.open = run
		return true
	case f.open != "" && run[0] == f.open[0] && n >= len(f.open) && strings.TrimSpace(rest) == "":
		f.open = ""
		return true
	}
	return f.open != ""
}

var markerAtStart = regexp.MustCompile(`^ {0,3}(> ?|[-*+] +|[0-9]{1,9}[.)] +)`)

// stripMarkers is a line without the blockquote and list markers it starts with.
func stripMarkers(line string) string {
	for {
		next := markerAtStart.ReplaceAllString(line, "")
		if next == line {
			return line
		}
		line = next
	}
}
