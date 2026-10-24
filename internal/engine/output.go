package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

// maxKeptOutput is the most bytes of a kept output Odin reads back: the bound of a submitted draft (ygg-submit),
// far above a plan's 1 MiB.
const maxKeptOutput = 4 << 20

// The output channel (change typed-boundaries, design §3): every agent session hands back one document of its
// contract, through the submit tool or as its strict whole answer. Odin validates it, asks once more with the
// errors when it does not validate, and keeps the document. Nothing is read out of prose.

// readOutput reads the output of the session key, a document of contract c: the document it submitted, when
// its adapter submits (gungnir.Submitter) and it did, read with Decode; otherwise its whole final answer, read
// with DecodeAnswer, which takes one JSON document, bare or in one code fence, and nothing else. It returns the
// document as it came. Odin validates a submitted document again: the tool helps the model, and is never trusted.
func readOutput[T any](r *run.Run, key string, c schema.Contract, res gungnir.Result) (T, []byte, error) {
	if len(res.Submitted) > 0 && submits(r, key) {
		v, err := schema.Decode[T](c, res.Submitted)
		return v, res.Submitted, err
	}
	v, err := schema.DecodeAnswer[T](c, []byte(res.Output))
	if err != nil {
		return v, nil, err
	}
	return v, answerBody(res.Output), nil
}

// answerBody is the document of a whole answer that DecodeAnswer read: the answer, trimmed, without its code
// fence when it has one.
func answerBody(answer string) []byte {
	s := strings.TrimSpace(answer)
	if strings.HasPrefix(s, "```") {
		_, body, _ := strings.Cut(s, "\n")
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
	}
	return []byte(s)
}

// submits reports whether the adapter of the session key hands its output back through the submit tool. key is
// a node's id or the session key of a loop's reviewer.
func submits(r *run.Run, key string) bool {
	role, ok := sessionRole(r, key)
	if !ok {
		return false
	}
	s, ok := r.Setup.Agents[role].(gungnir.Submitter)
	return ok && s.Submits()
}

// sessionRole is the role whose sessions run under key.
func sessionRole(r *run.Run, key string) (string, bool) {
	for _, n := range r.Nodes() {
		if n.ID == key {
			return n.Role, true
		}
		for _, rv := range n.Reviewers {
			if n.SessionKey(rv) == key {
				return rv.Role, true
			}
		}
	}
	return "", false
}

// sessionWithOutput runs one session of sp, whose output is a document of sp.output, and reads it. An output
// that does not validate is refused: the session is logged as output_refused, with its usage, and one more
// session, a new one, gets the same prompt with the errors under "## Your answer could not be used". It works
// where the first did, in the same workspace or leaf: the work stays, and only the answer is asked for again.
// When it validates it is logged as output_repaired; when it does not, the node fails as an agent does. The
// output that validated is kept (keepOutput). It returns the result and the event of the last session, which
// the caller completes and logs, as it does for session.
func sessionWithOutput[T any](ctx context.Context, r *run.Run, sp sessionSpec) (T, gungnir.Result, gungnir.Event, error) {
	var zero T
	if _, ok := sp.output.SchemaFile(); !ok {
		// The graph check gives every agent session a contract; this is an error of Odin's own, before any session.
		return zero, gungnir.Result{}, gungnir.Event{}, fmt.Errorf("the session %s returns %q, which is not a contract", sp.key, sp.output)
	}
	res, ev, err := session(ctx, r, sp)
	if err != nil {
		return zero, res, ev, err
	}
	v, raw, rerr := readOutput[T](r, sp.key, sp.output, res)
	if rerr != nil {
		refused := ev
		refused.Name, refused.Result, refused.Note = gungnir.EventOutputRefused, fmt.Sprintf("the output is not a valid %s", sp.output), firstLine(rerr.Error())
		if err := r.Log(refused); err != nil {
			return zero, res, ev, err
		}
		repair := sp
		repair.text = sp.text + "\n\n" + repairSection(sp.output, rerr)
		if res, ev, err = session(ctx, r, repair); err != nil {
			return zero, res, ev, err
		}
		if v, raw, rerr = readOutput[T](r, sp.key, sp.output, res); rerr != nil {
			failure := &jormungandr.AgentFailure{Reason: fmt.Sprintf("the output of %s is not a valid %s, after being asked twice: %v", sp.key, sp.output, rerr)}
			ev.Name, ev.Result = gungnir.EventNodeFailed, failure.Error()
			if err := r.Log(ev); err != nil {
				return zero, res, ev, err
			}
			return zero, res, ev, failure
		}
		if err := r.Log(gungnir.Event{Name: gungnir.EventOutputRepaired, Node: sp.key, Attempt: ev.Attempt, Role: sp.role,
			Adapter: ev.Adapter, Model: ev.Model, Result: fmt.Sprintf("the output is a valid %s", sp.output)}); err != nil {
			return zero, res, ev, err
		}
	}
	if err := keepOutput(r, sp.key, sp.output, raw, ev.Attempt); err != nil {
		return zero, res, ev, err
	}
	return v, res, ev, nil
}

// sessionForOutput is sessionWithOutput for a node whose contract is known only when the graph is read: it reads
// the output as the Go type of sp.output, so the rules the schema cannot say (Check) run too.
func sessionForOutput(ctx context.Context, r *run.Run, sp sessionSpec) (gungnir.Result, gungnir.Event, error) {
	var res gungnir.Result
	var ev gungnir.Event
	var err error
	switch sp.output {
	case schema.ContractDesign:
		_, res, ev, err = sessionWithOutput[schema.Design](ctx, r, sp)
	case schema.ContractSpec:
		_, res, ev, err = sessionWithOutput[schema.Spec](ctx, r, sp)
	case schema.ContractConformance:
		_, res, ev, err = sessionWithOutput[gungnir.Conformance](ctx, r, sp)
	case schema.ContractPlan:
		_, res, ev, err = sessionWithOutput[schema.Plan](ctx, r, sp)
	case schema.ContractReuseReview:
		_, res, ev, err = sessionWithOutput[schema.ReuseReview](ctx, r, sp)
	case schema.ContractReview:
		_, res, ev, err = sessionWithOutput[schema.Review](ctx, r, sp)
	case schema.ContractImplement:
		_, res, ev, err = sessionWithOutput[schema.Implement](ctx, r, sp)
	case schema.ContractRetro:
		_, res, ev, err = sessionWithOutput[schema.Retro](ctx, r, sp)
	case schema.ContractCheck:
		_, res, ev, err = sessionWithOutput[schema.CheckResult](ctx, r, sp)
	case schema.ContractDecisions:
		_, res, ev, err = sessionWithOutput[schema.Decisions](ctx, r, sp)
	case schema.ContractJudgement:
		_, res, ev, err = sessionWithOutput[schema.Judgement](ctx, r, sp)
	default:
		// A contract with no Go type of its own (the run record): its schema is the whole rule.
		_, res, ev, err = sessionWithOutput[json.RawMessage](ctx, r, sp)
	}
	return res, ev, err
}

// repairSection is what the repair session is told: every error of the answer, and how to hand it back.
func repairSection(c schema.Contract, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Your answer could not be used\n\nYour answer is not a valid `%s` document:\n\n", c)
	var ve *schema.ValidationError
	if errors.As(err, &ve) {
		for _, e := range ve.Errors {
			b.WriteString("- " + e + "\n")
		}
		if len(ve.Errors) >= maxValidationErrors {
			fmt.Fprintf(&b, "\nThese are the first %d errors; there may be more.\n", maxValidationErrors)
		}
	} else {
		b.WriteString("- " + err.Error() + "\n")
	}
	fmt.Fprintf(&b, "\nFix every error and hand back the whole `%s` document again: through the tool `submit`, or, if you do not "+
		"have it, as your final answer, which is then exactly one JSON document, bare or in one code fence, and nothing else.\n", c)
	return b.String()
}

// maxValidationErrors is the most errors a schema.ValidationError lists.
const maxValidationErrors = 50

// keepOutput keeps raw, the output of session key, a document of contract c, as outputs/<key>.json in the run
// folder, written atomically, and records it in run.json with its hash and attempt.
func keepOutput(r *run.Run, node string, c schema.Contract, raw []byte, attempt int) error {
	rel, err := outputPath(node)
	if err != nil {
		return err
	}
	dir := filepath.Join(r.Dir, "outputs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeAtomic(dir, filepath.Base(rel), raw); err != nil {
		return fmt.Errorf("keeping the output of %s: %w", node, err)
	}
	sum := sha256.Sum256(raw)
	if r.State.Outputs == nil {
		r.State.Outputs = map[string]run.OutputRef{}
	}
	r.State.Outputs[node] = run.OutputRef{Contract: string(c), Path: rel, SHA256: hex.EncodeToString(sum[:]), Attempt: attempt}
	return r.Save()
}

// loadOutput reads back the output kept for node and decodes it as its contract. A file whose hash is not the
// one run.json recorded is refused: what a later stage reads is what Odin validated.
func loadOutput[T any](r *run.Run, node string) (T, error) {
	var zero T
	ref, ok := r.State.Outputs[node]
	if !ok {
		return zero, fmt.Errorf("%s has no output kept in this run", node)
	}
	rel, err := outputPath(node)
	if err != nil {
		return zero, err
	}
	if ref.Path != rel {
		return zero, fmt.Errorf("run.json keeps the output of %s at %q, not at %s", node, ref.Path, rel)
	}
	dir := filepath.Join(r.Dir, "outputs")
	st, err := os.Lstat(filepath.Join(dir, filepath.Base(rel)))
	if err != nil {
		return zero, fmt.Errorf("the output of %s: %w", node, err)
	}
	data, err := readRegular(dir, filepath.Base(rel), st, maxKeptOutput)
	if err != nil {
		return zero, fmt.Errorf("the output of %s: %w", node, err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != ref.SHA256 {
		return zero, fmt.Errorf("the output of %s changed since Odin kept it (%s)", node, rel)
	}
	v, err := schema.Decode[T](schema.Contract(ref.Contract), data)
	if err != nil {
		return zero, fmt.Errorf("the output of %s: %w", node, err)
	}
	return v, nil
}

// outputPath is where the output of session key is kept, relative to the run folder. A key is a node id or a
// reviewer's session key, so it names one file of outputs/ and nothing else.
func outputPath(key string) (string, error) {
	if key == "" || strings.ContainsAny(key, `/\`) || key == "." || key == ".." {
		return "", fmt.Errorf("%q can't name an output file", key)
	}
	return "outputs/" + key + ".json", nil
}

// writeAtomic writes data to dir/name through a synced temporary file renamed over it, then syncs dir.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
