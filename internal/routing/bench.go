package routing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// BenchSchema names the format of a routing bench.
const BenchSchema = "ygg.routing-bench/v1"

// The kinds of question a bench asks (spec §8.3). Each is answered by one lookup in an index.
const (
	QConsumers       = "consumers"        // about a leaf: the leaves that depend on it
	QContractOwner   = "contract-owner"   // about a contract file: the leaf that holds it
	QReachedBy       = "reached-by"       // about a leaf, contract or package: the leaves a change reaches
	QContractReaders = "contract-readers" // about a contract file: the leaves other than its owner that a change reaches
	QPin             = "pin"              // about a leaf, arg a package: the version it pins
	QServices        = "services"         // about a leaf: the services its realm gives it
)

// Question is one bench question with its known answer. An answer is a set: its order does not matter.
type Question struct {
	ID     string   `yaml:"id"`
	Kind   string   `yaml:"kind"`
	About  string   `yaml:"about"`
	Arg    string   `yaml:"arg,omitempty"`
	Answer []string `yaml:"answer"`
}

// Bench is a project's routing bench: fixed questions with known answers. It lives in the project
// umbrella, where no agent mounts, so an agent can't write the answers it is graded on.
type Bench struct {
	Schema    string     `yaml:"schema"`
	Questions []Question `yaml:"questions"`
}

var benchID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// LoadBench reads a bench file: one YAML document with no unknown field, 1 to 500 questions, each with
// a unique id and a known kind, from a regular file (not a link) of at most 1 MiB.
func LoadBench(path string) (*Bench, error) {
	li, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !li.Mode().IsRegular() || li.Size() > maxFile {
		return nil, fmt.Errorf("%s: the bench must be a regular file (not a link) of at most %d bytes", path, maxFile)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !os.SameFile(li, fi) {
		return nil, fmt.Errorf("%s: changed while it was opened (%v)", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("%s: over %d bytes", path, maxFile)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var b Bench
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: more than one YAML document", path)
	}
	if b.Schema != BenchSchema {
		return nil, fmt.Errorf("%s: schema %q, want %q", path, b.Schema, BenchSchema)
	}
	if len(b.Questions) == 0 || len(b.Questions) > 500 {
		return nil, fmt.Errorf("%s: %d questions, want 1 to 500", path, len(b.Questions))
	}
	seen := map[string]bool{}
	for _, q := range b.Questions {
		switch {
		case !benchID.MatchString(q.ID):
			return nil, fmt.Errorf("%s: question id %q must be lower case words joined by dashes", path, q.ID)
		case seen[q.ID]:
			return nil, fmt.Errorf("%s: question %s is there twice", path, q.ID)
		case !slices.Contains([]string{QConsumers, QContractOwner, QReachedBy, QContractReaders, QPin, QServices}, q.Kind):
			return nil, fmt.Errorf("%s: question %s has the unknown kind %q", path, q.ID, q.Kind)
		case q.Answer == nil:
			return nil, fmt.Errorf("%s: question %s has no answer list (write [] for none)", path, q.ID)
		case q.About == "":
			return nil, fmt.Errorf("%s: question %s asks about nothing", path, q.ID)
		case (q.Kind == QPin) != (q.Arg != ""):
			return nil, fmt.Errorf("%s: question %s: only a pin question has an arg, and it needs one", path, q.ID)
		}
		seen[q.ID] = true
	}
	return &b, nil
}

// LoadBenchIn reads the bench at rel under root and refuses a link at any step of the path, so a
// folder swapped for a link can't point the gate at a bench of someone else's making.
func LoadBenchIn(root, rel string) (*Bench, error) {
	if !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("bench path %q must be a path inside the project", rel)
	}
	p := root
	parts := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	for _, part := range parts[:len(parts)-1] {
		p = filepath.Join(p, part)
		if err := plainDir(p); err != nil {
			return nil, fmt.Errorf("bench path: %w", err)
		}
	}
	return LoadBench(filepath.Join(p, parts[len(parts)-1]))
}

// Answerer answers a question with a set. ok is false when it can't answer.
type Answerer interface {
	Answer(q Question) (answer []string, ok bool)
}

// Report is a bench run: the questions answered right first, out of all of them.
type Report struct {
	Right, Total int
	Score        float64
	Misses       []string // the ids of the questions answered wrong, in bench order
}

// Score asks every question once. An answer is right when it is the known set, no more, no less. There is
// no second try and no model: this is right-first.
func (b *Bench) Score(a Answerer) Report {
	r := Report{Total: len(b.Questions)}
	for _, q := range b.Questions {
		if got, ok := a.Answer(q); ok && sameSet(got, q.Answer) {
			r.Right++
		} else {
			r.Misses = append(r.Misses, q.ID)
		}
	}
	if r.Total > 0 {
		r.Score = float64(r.Right) / float64(r.Total)
	}
	return r
}

// Ships reports whether an index with this score ships: it scored at or above the pack's threshold.
// A bench with no question never ships an index.
func (r Report) Ships(threshold float64) bool {
	return r.Total > 0 && float64(r.Right)+1e-9 >= threshold*float64(r.Total)
}

func (r Report) String() string { return fmt.Sprintf("%d/%d right first", r.Right, r.Total) }

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

type indexAnswerer struct {
	res *Result
	imp *Impact
}

// IndexAnswerer answers from the two indexes alone: one lookup for each question, extracted edges only.
func IndexAnswerer(res *Result, imp *Impact) Answerer { return indexAnswerer{res, imp} }

func (a indexAnswerer) Answer(q Question) ([]string, bool) {
	out := []string{}
	switch q.Kind {
	case QConsumers:
		for _, e := range a.res.Edges {
			if e.Tag == Extracted && e.To == q.About && slices.Contains([]string{"consumes", "requires", "imports"}, e.Kind) && slices.Contains(a.res.Leaves, e.From) {
				out = append(out, e.From)
			}
		}
	case QContractOwner:
		for _, e := range a.res.Edges {
			if e.Tag == Extracted && e.Kind == "holds" && e.To == "contract:"+q.About {
				out = append(out, e.From)
			}
		}
	case QReachedBy:
		out = slices.Clone(a.imp.Reaches(q.About).Extracted)
		out = slices.DeleteFunc(out, func(s string) bool { return !slices.Contains(a.res.Leaves, s) })
	case QContractReaders:
		c, ok := a.imp.Contract(q.About)
		if !ok {
			return nil, false
		}
		out = c.Readers.Extracted
	case QPin:
		for _, e := range a.res.Edges {
			if e.Tag == Extracted && e.Kind == "pins" && e.From == q.About && strings.HasPrefix(e.To, q.Arg+"@") {
				out = append(out, strings.TrimPrefix(e.To, q.Arg+"@"))
			}
		}
	case QServices:
		for _, e := range a.res.Edges {
			if e.Tag == Extracted && e.Kind == "needs" && e.From == q.About {
				out = append(out, strings.TrimPrefix(e.To, "service:"))
			}
		}
	default:
		return nil, false
	}
	return out, true
}

type grepBaseline struct {
	w      *wiring
	leaves []string
}

// GrepBaseline is the scripted stand-in for an agent with no index: it searches the leaves' text for the
// name asked about. It can answer who mentions a name, and nothing else. It is the figure to beat.
func GrepBaseline(dir string, leaves []string) Answerer {
	return grepBaseline{w: &wiring{root: dir}, leaves: leaves}
}

func (g grepBaseline) Answer(q Question) ([]string, bool) {
	var term string
	switch q.Kind {
	case QConsumers, QReachedBy:
		term = strings.TrimPrefix(strings.TrimPrefix(q.About, "go:"), "npm:")
	case QContractOwner, QContractReaders:
		term = q.About[strings.LastIndex(q.About, "/")+1:]
	default:
		return nil, false
	}
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(term) + `\b`)
	out := []string{}
	for _, l := range g.leaves {
		if l == q.About {
			continue
		}
		for _, rel := range g.w.walk(l, func(string) bool { return true }) {
			if b := g.w.read(rel); b != nil && (re.Match(b) || re.MatchString(rel)) {
				out = append(out, l)
				break
			}
		}
	}
	return out, true
}
