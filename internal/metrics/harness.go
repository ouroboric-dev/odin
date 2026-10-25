package metrics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// maxOutput is the most bytes of a kept output the record reads: the engine's own bound on an output.
const maxOutput = 1 << 20

// HarnessItem is a blocked reason or a note, and the session that wrote it.
type HarnessItem struct {
	Node  string `json:"node"` // the session key: a node's id, or a loop's and its reviewer's
	Role  string `json:"role"`
	Agent string `json:"agent"`
	Model string `json:"model"`
	Text  string `json:"text"`
}

// Harness is where the harness got in a model's way: outputs that broke their contract and went back for repair,
// the ones a repair mended, every blocked answer and every note, and the design's questions no person answered,
// which the spec then had to decide.
type Harness struct {
	Refused             map[string]int `json:"refused"`  // "role agent model": its outputs that broke their contract
	Repaired            map[string]int `json:"repaired"` // "role agent model": its refused outputs a repair session mended
	Blocked             []HarnessItem  `json:"blocked"`
	Notes               []HarnessItem  `json:"notes"`
	QuestionsLeftToSpec int            `json:"questions_left_to_spec"`
}

func newHarness() Harness {
	return Harness{Refused: map[string]int{}, Repaired: map[string]int{}, Blocked: []HarnessItem{}, Notes: []HarnessItem{}}
}

// harness counts the refused and repaired outputs from the ledger, and reads the blocked answers, the notes and
// the design's open questions from the outputs Odin kept, never from an agent's prose. A kept output that is not
// as Odin kept it is left out, with a warning.
func (r *Run) harness(dir string, state *record, events []gungnir.Event) {
	h := newHarness()
	who := map[string]gungnir.Event{} // session key: its last line that names a role
	for _, e := range events {
		key := e.Role + " " + e.Adapter + " " + e.Model
		switch e.Name {
		case gungnir.EventOutputRefused:
			h.Refused[key]++
		case gungnir.EventOutputRepaired:
			h.Repaired[key]++
		}
		if e.Node != "" && e.Role != "" {
			who[e.Node] = e
		}
	}
	answered := map[string]bool{}
	for _, g := range state.Decisions {
		for _, a := range g.Decisions.Answers {
			answered[a.Question] = true
		}
	}
	keys := make([]string, 0, len(state.Outputs))
	for k := range state.Outputs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		ref := state.Outputs[k]
		data, err := readKept(dir, k, ref.Path, ref.SHA256, schema.Contract(ref.Contract))
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("the kept output of %s is left out of the harness: %v", k, err))
			continue
		}
		var doc struct {
			Blocked *struct {
				Reason string `json:"reason"`
			} `json:"blocked"`
			Notes         []string `json:"notes"`
			OpenQuestions []struct {
				ID string `json:"id"`
			} `json:"open_questions"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("the kept output of %s is left out of the harness: %v", k, err))
			continue
		}
		w := who[k]
		item := func(text string) HarnessItem {
			return HarnessItem{Node: k, Role: w.Role, Agent: w.Adapter, Model: w.Model, Text: text}
		}
		if doc.Blocked != nil {
			h.Blocked = append(h.Blocked, item(doc.Blocked.Reason))
		}
		for _, n := range doc.Notes {
			h.Notes = append(h.Notes, item(n))
		}
		if schema.Contract(ref.Contract) == schema.ContractDesign {
			for _, q := range doc.OpenQuestions {
				if !answered[q.ID] {
					h.QuestionsLeftToSpec++
				}
			}
		}
	}
	r.Harness = h
}

// readKept reads the output kept for session key at rel in the run folder dir, as the engine's loadOutput does:
// a regular file of outputs/ named for the key, at most maxOutput bytes, whose hash is the one run.json recorded
// and which is a valid document of its contract.
func readKept(dir, key, rel, want string, c schema.Contract) ([]byte, error) {
	if key == "" || strings.ContainsAny(key, `/\`) || key == "." || key == ".." || rel != "outputs/"+key+".json" {
		return nil, fmt.Errorf("run.json keeps it at %q, which is not its file of outputs/", rel)
	}
	path := filepath.Join(dir, "outputs", key+".json")
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxOutput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxOutput {
		return nil, fmt.Errorf("%s is over %d bytes", path, maxOutput)
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s changed since Odin kept it", path)
	}
	if err := schema.Validate(c, data); err != nil {
		return nil, err
	}
	return data, nil
}
