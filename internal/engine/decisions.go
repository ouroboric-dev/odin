package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
	"github.com/ouroboric-dev/odin/internal/run"
)

// What a person gives at a stop is data too (change typed-boundaries, design §4): a ygg.decisions/v1 file of owner
// decisions under ids the operator chooses and keeps across runs, answers to the design's open questions, and for a
// rejection its reason. At a gate node the decisions and answers are the product owner's, kept in State.Decisions:
// every later session reads them (the decisions input) and the spec must cite each decision (ownercheck.go). At a
// stop that runs a stage again, what the person gave is that stage's rejection (rejectionText).

// maxDecisionsFile bounds a decisions file: far more than a person writes at a gate.
const maxDecisionsFile = 64 << 10

// ReadDecisions reads the decisions file at path: a regular file, never a symbolic link, of at most 64 KiB, that holds
// one ygg.decisions/v1 document. Whether it may hold a reason depends on the decision, which Approve and Reject check.
func ReadDecisions(path string) (schema.Decisions, error) {
	var d schema.Decisions
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		return d, err
	case fi.Mode()&os.ModeSymlink != 0:
		return d, fmt.Errorf("%s is a symbolic link; give the decisions file itself", path)
	case !fi.Mode().IsRegular():
		return d, fmt.Errorf("%s is not a regular file; a decisions file is one", path)
	}
	// O_NONBLOCK: a FIFO put in its place after the Lstat cannot block the open; the Stat below refuses it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return d, fmt.Errorf("%s is a symbolic link; give the decisions file itself", path)
	}
	if err != nil {
		return d, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return d, fmt.Errorf("%s is not a regular file; a decisions file is one", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxDecisionsFile+1))
	if err != nil {
		return d, err
	}
	if len(data) > maxDecisionsFile {
		return d, fmt.Errorf("%s is over 64 KiB; a decisions file is smaller", path)
	}
	if d, err = schema.Decode[schema.Decisions](schema.ContractDecisions, data); err != nil {
		return d, fmt.Errorf("%s: %w", path, err)
	}
	// Strict decoding takes the last of a key given twice in one object, so a second "decisions" or "text" would
	// silently win over the first: such a file is refused.
	if err := uniqueKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return schema.Decisions{}, fmt.Errorf("%s: %w", path, err)
	}
	return d, nil
}

// uniqueKeys reads the next JSON value of dec and refuses an object in it that holds a key twice.
func uniqueKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	switch tok {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := k.(string)
			if seen[key] {
				return fmt.Errorf("key %s appears twice in one object", key)
			}
			seen[key] = true
			if err := uniqueKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	case json.Delim('['):
		for dec.More() {
			if err := uniqueKeys(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
	}
	return err
}

// Note is the shorthand --note TEXT at the stop gate of run r: a decisions file with one decision under the next free
// id, note-<gate>, then note-<gate>-2, note-<gate>-3 and on (ruling R47), so a gate met again after a back_to return
// past it can take a new note. The same note again takes the id it has, and changes nothing. A blank note gives
// nothing.
func Note(r *run.Run, gate, text string) schema.Decisions {
	if strings.TrimSpace(text) == "" {
		return schema.Decisions{}
	}
	id := "note-" + gate
	for i := 2; ; i++ {
		if _, k, ok := keptDecision(r.State, id); !ok || k.Text == text {
			break
		}
		id = fmt.Sprintf("note-%s-%d", gate, i)
	}
	return schema.Decisions{Decisions: []schema.Decision{{ID: id, Text: text}}}
}

// Reason is the shorthand --reason TEXT: a decisions file with only the reason.
func Reason(text string) schema.Decisions { return schema.Decisions{Reason: text} }

// given reports whether a person gave anything.
func given(d schema.Decisions) bool {
	return len(d.Decisions) > 0 || len(d.Answers) > 0 || d.Reason != ""
}

// checkGiven refuses what a person gave at a stop of r before anything is recorded: a rejection needs a reason and an
// approval holds none; what is given must be a ygg.decisions/v1 document (so a shorthand is held to the file's rules,
// such as a note of at most 4 KiB); and each answer names an open question of this run's design, once (ruling R45:
// two answers to one question would leave the stage to choose).
func checkGiven(r *run.Run, d schema.Decisions, rejection bool) error {
	switch {
	case rejection && strings.TrimSpace(d.Reason) == "":
		return errors.New("a rejection needs a reason, in the file or as --reason; it goes back to the node as input")
	case !rejection && d.Reason != "":
		return errors.New("reason belongs to a rejection; an approval's file holds decisions and answers")
	case !given(d):
		return nil
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if _, err := schema.Decode[schema.Decisions](schema.ContractDecisions, data); err != nil {
		return err
	}
	// Ruling R46(3): an id means one thing for the run, so the decisions a spec cited cannot be re-worded under it.
	for _, x := range d.Decisions {
		if g, k, ok := keptDecision(r.State, x.ID); ok && k.Text != x.Text {
			return fmt.Errorf("decision %s was already given as %q at %s; give a new id", x.ID, clip(mdsafe.Line(k.Text), 200), g)
		}
	}
	if len(d.Answers) == 0 {
		return nil
	}
	questions, order, designs, err := designQuestions(r)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, a := range d.Answers {
		_, ok := questions[a.Question]
		switch {
		case seen[a.Question]:
			return fmt.Errorf("%s is answered twice; give one answer to each question", a.Question)
		case designs == 0:
			return fmt.Errorf("the answer to %s names no open question: this run has no design whose open questions it could answer", a.Question)
		case len(order) == 0:
			return fmt.Errorf("the answer to %s names no open question: this run's design has none", a.Question)
		case !ok:
			return fmt.Errorf("the answer to %s names no open question of this run's design, whose open questions are %s", a.Question, strings.Join(order, ", "))
		}
		seen[a.Question] = true
	}
	return nil
}

// keptDecision is the decision kept under id, at any gate, and the gate it was given at.
func keptDecision(s *run.State, id string) (string, schema.Decision, bool) {
	for _, g := range s.Decisions {
		for _, k := range g.Decisions.Decisions {
			if k.ID == id {
				return g.Gate, k, true
			}
		}
	}
	return "", schema.Decision{}, false
}

// designQuestions are the open questions of the designs this run kept: the question of each id, the ids in order,
// and how many designs there are.
func designQuestions(r *run.Run) (map[string]string, []string, int, error) {
	var keys []string
	for key, ref := range r.State.Outputs {
		if schema.Contract(ref.Contract) == schema.ContractDesign {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	questions := map[string]string{}
	var order []string
	for _, key := range keys {
		d, err := loadOutput[schema.Design](r, key)
		if err != nil {
			return nil, nil, 0, err
		}
		for _, q := range d.OpenQuestions {
			if _, ok := questions[q.ID]; !ok {
				order = append(order, q.ID)
			}
			questions[q.ID] = q.Question
		}
	}
	return questions, order, len(keys), nil
}

// noteOf is what a stop's ledger event carries of what a person gave: the decisions file as one line of JSON, or ""
// for nothing.
func noteOf(d schema.Decisions) string {
	if !given(d) {
		return ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // the note holds the person's words as they wrote them
	if err := enc.Encode(d); err != nil {
		return ""
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// keepDecisions keeps the decisions and answers a person gave at gate, the product owner's word, in State.Decisions.
// A gate met again (a back_to return past it, or a rejection there and then an approval) adds them to what it was
// given before, and moves to the end, the order in which they were given. A decision already kept, at any gate, is
// not kept again: checkGiven refused it with another text (ruling R46(3)), so it is the same. An answer to a question
// answered at this gate before replaces it. Nothing new restates nothing, and the earlier decisions hold. A
// rejection's reason is not kept: it goes to the node the gate runs again.
func keepDecisions(s *run.State, gate string, d schema.Decisions) {
	var fresh []schema.Decision
	for _, x := range d.Decisions {
		if _, _, ok := keptDecision(s, x.ID); !ok {
			fresh = append(fresh, x)
		}
	}
	if len(fresh) == 0 && len(d.Answers) == 0 {
		return
	}
	var kept schema.Decisions
	if i := slices.IndexFunc(s.Decisions, func(g run.GateDecisions) bool { return g.Gate == gate }); i >= 0 {
		kept = s.Decisions[i].Decisions
		s.Decisions = slices.Delete(s.Decisions, i, i+1)
	}
	kept.Decisions = append(kept.Decisions, fresh...)
	for _, x := range d.Answers {
		kept.Answers = slices.DeleteFunc(kept.Answers, func(k schema.Answer) bool { return k.Question == x.Question })
		kept.Answers = append(kept.Answers, x)
	}
	s.Decisions = append(s.Decisions, run.GateDecisions{Gate: gate, Decisions: kept})
}

// dropDecisions drops what the gates with these ids were given: the run meets them again.
func dropDecisions(s *run.State, gates map[string]bool) {
	kept := s.Decisions[:0]
	for _, d := range s.Decisions {
		if !gates[d.Gate] {
			kept = append(kept, d)
		}
	}
	s.Decisions = kept
}

// rejectionText is the rejection of a stage that runs again after a person decided at a stop: a gate's or a loop's
// rejection, or an approval at the secret-scan stop or at a blocked output's (rulings R43, R44, R45). It is the
// reason, then each decision with its id, then each answer with its question, every field made safe (mdsafe), so the
// text goes into a prompt as it is; with nothing given it is otherwise. The decisions are the product owner's at a
// gate node (owner); at any other stop they are only what the person said there (ruling R46(5)), and say so.
func rejectionText(r *run.Run, d schema.Decisions, otherwise string, owner bool) (string, error) {
	if !given(d) {
		return otherwise, nil
	}
	var parts []string
	if strings.TrimSpace(d.Reason) != "" {
		parts = append(parts, mdsafe.Text(d.Reason))
	}
	if len(d.Decisions) > 0 {
		label := "What the person said:"
		if owner {
			label = "The product owner's decisions:"
		}
		parts = append(parts, label+"\n\n"+strings.TrimSuffix(decisionItems(d.Decisions, ""), "\n"))
	}
	if len(d.Answers) > 0 {
		questions, _, _, err := designQuestions(r)
		if err != nil {
			return "", err
		}
		parts = append(parts, "Answers to the design's open questions:\n\n"+strings.TrimSuffix(answerItems(d.Answers, "", questions), "\n"))
	}
	return strings.Join(parts, "\n\n"), nil
}

// decisionsBody is the section of the decisions input: every gate's decisions and then their answers, in the order
// given, each with its id and the gate it was given at, as the lists of ygg.decisions/v1; None. before any.
func decisionsBody(r *run.Run) (string, error) {
	var decisions, answers strings.Builder
	var questions map[string]string
	for _, g := range r.State.Decisions {
		decisions.WriteString(decisionItems(g.Decisions.Decisions, g.Gate))
		if len(g.Decisions.Answers) == 0 {
			continue
		}
		if questions == nil {
			var err error
			if questions, _, _, err = designQuestions(r); err != nil {
				return "", err
			}
		}
		answers.WriteString(answerItems(g.Decisions.Answers, g.Gate, questions))
	}
	if decisions.Len() == 0 && answers.Len() == 0 {
		return none, nil
	}
	b := "Decisions the product owner gave at the approval points of this run. They hold for this product: where one " +
		"differs from an earlier document, the decision wins, and the difference is written down as the product owner's decision, " +
		"citing its id.\n"
	if decisions.Len() > 0 {
		b += "\n### decisions\n\n" + decisions.String()
	}
	if answers.Len() > 0 {
		b += "\n### answers\n\n" + answers.String()
	}
	return b, nil
}

// decisionItems is a list item for each decision: its id, the gate it was given at (unless gate is ""), and its text.
func decisionItems(ds []schema.Decision, gate string) string {
	var b strings.Builder
	for _, d := range ds {
		b.WriteString("- " + mdsafe.Item(mdsafe.After(mdsafe.Line(d.ID)+givenAt(gate)+": ", d.Text), 2) + "\n")
	}
	return b.String()
}

// answerItems is a list item for each answer: its question's id, the gate it was given at (unless gate is ""), the
// answer, and the question as the design asks it.
func answerItems(as []schema.Answer, gate string, questions map[string]string) string {
	var b strings.Builder
	for _, a := range as {
		q, ok := questions[a.Question]
		if !ok {
			q = "(no longer an open question of this run's design)"
		}
		b.WriteString("- " + mdsafe.Item(mdsafe.After(mdsafe.Line(a.Question)+givenAt(gate)+": ", a.Text)+"\nThe question: "+mdsafe.Line(q), 2) + "\n")
	}
	return b.String()
}

// givenAt names the gate a decision was given at, or nothing for "".
func givenAt(gate string) string {
	if gate == "" {
		return ""
	}
	return " (at " + mdsafe.Line(gate) + ")"
}
