package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/mdsafe"
	"github.com/ouroboric-dev/odin/internal/run"
)

// The inputs of a session (change typed-boundaries, design §2): each input a node declares is one section of its
// prompt, "## Input: <name>", rendered from its typed value. Data is never prose the session could mistake for
// Odin's: every line of a field's text that could start a heading, front matter or a fence of its own is
// indented, and structure that prose would lose is a JSON block. Instructions (the role's prompt, its skills and
// the brief) pass as they are.

// none is the section of an input that may be empty and is.
const none = "None.\n"

// emptyAllowed are the built-in sources that may hold nothing, as on a first attempt: their section says so. Every
// other input must have a value, or the session does not start.
var emptyAllowed = []string{schema.SourceDecisions, schema.SourceOpenFindings, schema.SourceCheckResults, schema.SourceRejection, schema.SourceRefusal}

// missingInput is a declared input that nothing has provided: the session does not start.
func missingInput(who, name string) error {
	return &nodeFailure{fmt.Errorf("node %s needs %s, and nothing has provided it", who, name)}
}

// inputBody is the section of input name for session who of node n: a built-in source, a value the caller
// computed (extra), or the output an earlier node kept.
func inputBody(r *run.Run, n schema.Node, who, name string, extra map[string]string) (string, error) {
	s := r.State
	switch name {
	case schema.SourceBrief:
		if strings.TrimSpace(r.Setup.BriefText) == "" {
			return "", missingInput(who, name)
		}
		// The brief is an instruction: it passes as it is, but for a code fence it leaves open, which is closed.
		return withNewline(mdsafe.CloseFences(r.Setup.BriefText)), nil
	case schema.SourceDecisions:
		return renderDecisions(decisionsOf(s))
	case schema.SourceTask:
		block := taskInput(r, n)
		if block == "" {
			return "", missingInput(who, name)
		}
		return block, nil
	case schema.SourceRejection:
		return reasonBody("A person sent the last attempt back at an approval point, and this is why:", s.Reasons[n.ID]), nil
	case schema.SourceRefusal:
		return reasonBody("A later step of the factory refused the last attempt's work. Fix what it says; do not say it is fixed: the step looks again after your work and decides.", s.Refusals[n.ID]), nil
	case schema.SourceDate:
		return dateOf(r.Setup) + "\n", nil
	case schema.SourceRecord:
		return "", &nodeFailure{fmt.Errorf("node %s needs %s, and the record is not built yet", who, name)}
	case schema.SourceChange, schema.SourceOpenFindings, schema.SourceCheckResults:
		v, ok := extra[name]
		switch {
		case !ok, v == "" && !slices.Contains(emptyAllowed, name):
			return "", missingInput(who, name)
		case v == "":
			return none, nil
		}
		return withNewline(v), nil
	}
	c, v, err := keptInput(r, name)
	if err != nil {
		return "", &nodeFailure{fmt.Errorf("node %s needs %s: %w", who, name, err)}
	}
	if v == nil {
		return "", missingInput(who, name)
	}
	return renderContract(c, v)
}

// dateOf is the date of the date input: Odin's clock, in UTC.
func dateOf(s *run.Setup) string {
	now := time.Now
	if s.Clock != nil {
		now = s.Clock
	}
	return now().UTC().Format("2006-01-02")
}

// reasonBody is the section of a rejection or a refusal: what it is, then its words, or None. when there is none.
func reasonBody(what, text string) string {
	if strings.TrimSpace(text) == "" {
		return none
	}
	return what + "\n\n" + mdsafe.Text(text) + "\n"
}

// keptInput reads the output node kept in this run as the Go type of its contract, so the rules the schema can't
// say are checked again. v is nil when the run keeps none.
func keptInput(r *run.Run, node string) (schema.Contract, any, error) {
	ref, ok := r.State.Outputs[node]
	if !ok {
		return "", nil, nil
	}
	c := schema.Contract(ref.Contract)
	var v any
	var err error
	switch c {
	case schema.ContractDesign:
		v, err = loadOutput[schema.Design](r, node)
	case schema.ContractSpec:
		v, err = loadOutput[schema.Spec](r, node)
	case schema.ContractConformance:
		v, err = loadOutput[gungnir.Conformance](r, node)
	case schema.ContractPlan:
		v, err = loadOutput[schema.Plan](r, node)
	case schema.ContractReuseReview:
		v, err = loadOutput[schema.ReuseReview](r, node)
	case schema.ContractReview:
		v, err = loadOutput[schema.Review](r, node)
	case schema.ContractImplement:
		v, err = loadOutput[schema.Implement](r, node)
	case schema.ContractRetro:
		v, err = loadOutput[schema.Retro](r, node)
	case schema.ContractCheck:
		v, err = loadOutput[schema.CheckResult](r, node)
	case schema.ContractDecisions:
		v, err = loadOutput[schema.Decisions](r, node)
	case schema.ContractJudgement:
		v, err = loadOutput[schema.Judgement](r, node)
	default:
		v, err = loadOutput[json.RawMessage](r, node)
	}
	if err != nil {
		return "", nil, err
	}
	return c, v, nil
}

// renderContract renders v, a document of contract c, as the body of its section: each top-level field the
// document holds, in the order of the contract's schema, under "### <field>". Text is a field's text, made safe
// (mdsafe.Text); a list of strings is a bullet list; a number or a boolean is its value; an object, or a list that
// holds one, is a JSON block, which keeps the structure exactly and holds no line that could start a heading. A
// document with no field is None.
func renderContract(c schema.Contract, v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", fmt.Errorf("%s: a document is an object: %w", c, err)
	}
	order, err := propertyOrder(c)
	if err != nil {
		return "", err
	}
	var unlisted []string // a field the schema does not list comes last; the schema refuses one today
	for k := range fields {
		if !slices.Contains(order, k) {
			unlisted = append(unlisted, k)
		}
	}
	slices.Sort(unlisted)
	order = append(order, unlisted...)
	var b strings.Builder
	for _, name := range order {
		f, ok := fields[name]
		if !ok || string(f) == "null" {
			continue
		}
		body, err := renderField(f)
		if err != nil {
			return "", fmt.Errorf("%s: %s: %w", c, name, err)
		}
		if body == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### " + name + "\n\n" + body)
	}
	if b.Len() == 0 {
		return none, nil
	}
	return b.String(), nil
}

// renderField renders one field's value, ending in a newline; "" is an empty text, which is left out.
func renderField(raw json.RawMessage) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", err
	}
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return "", nil
		}
		return mdsafe.Text(x) + "\n", nil
	case json.Number, bool:
		return fmt.Sprint(x) + "\n", nil
	case []any:
		if len(x) == 0 {
			return none, nil
		}
		items := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return jsonBlock(x)
			}
			items = append(items, s)
		}
		return mdsafe.Bullets(items), nil
	}
	return jsonBlock(v)
}

// propertyOrder lists the top-level properties of contract c's schema, in the order the schema writes them.
func propertyOrder(c schema.Contract) ([]string, error) {
	name, ok := c.SchemaFile()
	if !ok {
		return nil, fmt.Errorf("contract %s has no embedded schema", c)
	}
	data, err := fs.ReadFile(schema.SchemaFS(), name)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Properties json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var order []string
	if len(doc.Properties) == 0 {
		return order, nil
	}
	dec := json.NewDecoder(bytes.NewReader(doc.Properties))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		order = append(order, key.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// jsonBlock is v as indented JSON in a code block. A JSON string holds no line break, and every line of the
// indented document starts with a space or a bracket, so no line of it can start a heading or close the fence.
func jsonBlock(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return "```json\n" + b.String() + "```\n", nil
}

func withNewline(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// decisionsOf is what the run holds of the operator's gate decisions, as a ygg.decisions/v1 document: each note an
// approval gave, in the order they were given, with its gate as its id.
func decisionsOf(s *run.State) schema.Decisions {
	var d schema.Decisions
	for _, x := range s.Decisions {
		d.Decisions = append(d.Decisions, schema.Decision{ID: x.Gate, Text: x.Note})
	}
	return d
}

// renderDecisions is the section of the decisions input: None. before any approval gave a note.
func renderDecisions(d schema.Decisions) (string, error) {
	if len(d.Decisions) == 0 && len(d.Answers) == 0 {
		return none, nil
	}
	body, err := renderContract(schema.ContractDecisions, d)
	if err != nil {
		return "", err
	}
	return "Decisions the product owner gave at the approval points of this run. They hold for this product: where one " +
		"differs from an earlier document, the decision wins, and the difference is written down as the product owner's decision.\n\n" + body, nil
}

// implementerFindings is the open_findings section of an implementer: the findings open against the work, the
// disputes of the last attempt that did not hold, and how the last session ended when it failed. "" is none.
func implementerFindings(in jormungandr.Input) (string, error) {
	var b strings.Builder
	if len(in.Open) > 0 {
		b.WriteString("The checks and reviewers found these problems in the work so far. Fix every one. Do not say any of them " +
			"is fixed: the check or reviewer that raised it looks again after your work and decides.\n\n")
		block, err := jsonBlock(in.Open)
		if err != nil {
			return "", err
		}
		b.WriteString(block)
	}
	if len(in.Rejected) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### Your disputes that did not hold\n\nYou said these findings are wrong. No check settled it, so each finding " +
			"stays open. Fix it, or dispute it again with a check of this loop whose result lists the test among the tests that passed.\n\n")
		block, err := jsonBlock(in.Rejected)
		if err != nil {
			return "", err
		}
		b.WriteString(block)
	}
	if strings.TrimSpace(in.LastFailure) != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("### The last attempt failed\n\nYour last session ended with:\n\n" + mdsafe.Text(in.LastFailure) +
			"\n\nThe repository is as the last attempt that finished left it.\n")
	}
	return b.String(), nil
}

// reviewerFindings is the open_findings section of a reviewer: the findings it reported before and still holds
// open. "" is none.
func reviewerFindings(open []gungnir.Finding) (string, error) {
	if len(open) == 0 {
		return "", nil
	}
	block, err := jsonBlock(open)
	if err != nil {
		return "", err
	}
	return "These are the findings you reported before and still hold open. Report each of these again, with the same id: " +
		"as open if the problem is still there, or as fixed with proof if it is gone. One you leave out is an error.\n\n" + block, nil
}

// checkView is how one check of the loop ended, as a session reads it: until the checks' results are typed
// (ygg.check/v1), its name and whether it passed.
type checkView struct {
	Check  string `json:"check"`
	Passed bool   `json:"passed"`
}

// checkResults is the check_results section: "" is none.
func checkResults(checks []checkView) (string, error) {
	if len(checks) == 0 {
		return "", nil
	}
	block, err := jsonBlock(checks)
	if err != nil {
		return "", err
	}
	return "How the checks of this loop ended when they last ran on the work:\n\n" + block, nil
}
