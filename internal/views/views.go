// Package views renders the documents people read from the typed outputs of the stages (change typed-boundaries,
// design §2: Markdown is a view): the design, the spec, the plan and its tasks, and the reuse review. Nothing reads
// a view back. Every field goes through the one escaping rule of prompts and views (mdsafe), so a field reads as
// its text and never as a heading, front matter or a fence of its own.
package views

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"text/template"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
)

//go:embed *.md.tmpl
var files embed.FS

// Front is the front matter of a view, which Odin writes from typed values. Each value is a quoted scalar, so no
// field can end the front matter or add a key to it.
type Front struct {
	Title   string
	Date    string   // the run's date, as 2006-01-02
	Related []string // links to the documents this one builds on, relative to it
}

// pages maps each contract that has a view to its template and the Go type of its document.
var pages = map[schema.Contract]struct {
	file string
	typ  reflect.Type
}{
	schema.ContractDesign:      {"design.md.tmpl", reflect.TypeFor[schema.Design]()},
	schema.ContractSpec:        {"spec.md.tmpl", reflect.TypeFor[schema.Spec]()},
	schema.ContractPlan:        {"plan.md.tmpl", reflect.TypeFor[schema.Plan]()},
	schema.ContractReuseReview: {"reuse-review.md.tmpl", reflect.TypeFor[schema.ReuseReview]()},
}

// Has reports whether contract c has a view: a publish node publishes only an output that has one.
func Has(c schema.Contract) bool {
	_, ok := pages[c]
	return ok
}

// Render renders v, a document of contract c (a value or a pointer to one), as its view, front matter first. A
// plan has two views: Render gives plan.md, and Tasks gives tasks.md.
func Render(c schema.Contract, v any, f Front) ([]byte, error) {
	p, ok := pages[c]
	if !ok {
		return nil, fmt.Errorf("%s has no view", c)
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer && !rv.IsNil() {
		rv = rv.Elem()
	}
	if !rv.IsValid() || rv.Type() != p.typ {
		return nil, fmt.Errorf("a view of %s renders a %s, not a %T", c, p.typ, v)
	}
	return execute(p.file, f, rv.Interface())
}

// Tasks renders the plan's tasks.md: one section for each task, in the plan's order.
func Tasks(p schema.Plan, f Front) ([]byte, error) {
	return execute("tasks.md.tmpl", f, p)
}

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"front":   front,
	"text":    text,
	"item":    mdsafe.Item,
	"line":    mdsafe.Line,
	"cell":    mdsafe.Cell,
	"fenced":  func(s string) string { return mdsafe.Fenced("", s) },
	"bullets": mdsafe.Bullets,
	"decided": decided,
	"closed":  mdsafe.CloseFences,
	"list":    list,
	"deps":    deps,
}).ParseFS(files, "*.md.tmpl"))

func execute(name string, f Front, doc any) ([]byte, error) {
	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name, struct {
		Front Front
		Doc   any
	}{f, doc}); err != nil {
		return nil, err
	}
	return append(bytes.TrimRight(b.Bytes(), "\n"), '\n'), nil
}

// date is a date as Odin gives one.
var date = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// front is the front matter the prompts asked for before the outputs were typed, so the docs folder reads as it
// did: title, status draft, date and related.
func front(f Front) (string, error) {
	if !date.MatchString(f.Date) {
		return "", fmt.Errorf("the date of a view is 2006-01-02, not %q", f.Date)
	}
	related := make([]string, len(f.Related))
	for i, r := range f.Related {
		related[i] = quote(r)
	}
	return "---\ntitle: " + quote(f.Title) + "\nstatus: draft\ndate: " + f.Date + "\nrelated: [" + strings.Join(related, ", ") + "]\n---\n", nil
}

// quote is s as a JSON string, which is a YAML double-quoted scalar: one value on one line, whatever s holds.
func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// text is a section's text, made safe, or None. when it holds nothing.
func text(s string) string {
	if strings.TrimSpace(s) == "" {
		return "None."
	}
	return mdsafe.Text(s)
}

// decided is one decision as the prompts wrote it, "**Decided <date>:** <decision> <reason>", with the owner
// decisions it cites, as a list item. Each field closes its own fences, so one cannot take in the next.
func decided(d schema.Decided) string {
	s := "**Decided " + mdsafe.Line(d.Date) + ":** " + mdsafe.CloseFences(d.Decision) + "\n" + mdsafe.CloseFences(d.Reason)
	if len(d.Cites) > 0 {
		s += "\n(Owner decisions: " + list(d.Cites) + ")"
	}
	return mdsafe.Item(s, 2)
}

// list is texts as one line, separated by commas.
func list(items []string) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = mdsafe.Line(it)
	}
	return strings.Join(out, ", ")
}

// deps is a task's dependencies as tasks.md wrote them: ids separated by commas, or nothing.
func deps(ids []string) string {
	if len(ids) == 0 {
		return "nothing"
	}
	return list(ids)
}
