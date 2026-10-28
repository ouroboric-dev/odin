package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/git"
	"github.com/ouroboric-dev/odin/internal/run"
)

// maxInlineInput is the most bytes one input's section takes in a prompt. A larger one goes to a file in the
// session's workspace, which the section names: it is never cut, and never silently left out.
const maxInlineInput = 96 << 10

// Where the files of the inputs over the bound go: inputs/ in a scratch workspace; in a leaf or an umbrella, a
// folder that no checkpoint commits (ruling R36), which the leaf's git ignores by a rule in its git dir, Odin's,
// and which Odin removes after the session.
const (
	scratchInputs = "inputs"
	repoInputs    = ".ygg-inputs"
)

// promptFor is the prompt of a session of node n, or of its reviewer rv when rv is set: the role's prompt file
// and its skills, as they are; for a scratch node, what its workspace holds; then one section for each input the
// session declares, in the declared order, each rendered from its typed value (inputs.go); then how to hand back
// the answer, a document of the session's contract, through the submit tool when the role's adapter has it. extra
// holds the values only the caller has: the task's change, the open findings and the check results. A declared
// input with no value is a *nodeFailure, before any session starts. An input over maxInlineInput is written to
// the session's workspace, and its section names the file. The session adds its turn budget after the answer, when
// it knows the turns it is given (withTurnBudget).
func promptFor(r *run.Run, n schema.Node, rv *schema.Reviewer, extra map[string]string) (_ string, err error) {
	who, role, realm, file, skillNames, output := n.ID, n.Role, n.Realm, n.Prompt, n.Skills, n.Output
	if rv != nil {
		// A reviewer's prompt comes from the graph pack.
		who, role, realm, file, skillNames, output = n.SessionKey(*rv), rv.Role, "", rv.Prompt, rv.Skills, rv.Output
	}
	inputs := sessionInputs(n, rv)
	b, err := r.ReadPrompt(realm, file) // its hash is kept in run.json, for the record (ruling R55)
	if err != nil {
		return "", changed(err)
	}
	text := strings.TrimRight(string(b), "\n")
	// Skills load explicitly, the ones the session names, and no others (spec §5.1).
	skillText, err := r.LoadSkills(skillNames)
	if err != nil {
		return "", changed(err)
	}
	if skillText != "" {
		text = strings.TrimRight(text+"\n\n"+skillText, "\n")
	}
	if n.Scratch {
		note := workspaceNote()
		if rv == nil && slices.Contains(inputs, schema.SourceRecord) {
			// The retrospective reads the ledger and the transcripts beside the record (change typed-boundaries §5).
			more, err := seedRecordFiles(r, n)
			if err != nil {
				return "", &nodeFailure{err}
			}
			note += more
		}
		text = strings.TrimRight(text+"\n\n"+note, "\n")
	}
	files, err := inputFiles(r, n)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil { // a prompt that could not be built leaves no input file behind
			err = errors.Join(err, dropInputs(r, n))
		}
	}()
	for _, name := range inputs {
		body, err := inputBody(r, n, who, name, extra)
		if err != nil {
			return "", err
		}
		if len(body) > maxInlineInput {
			rel, err := files.write(name, body)
			if err != nil {
				return "", &nodeFailure{fmt.Errorf("the input %s of %s: %w", name, who, err)}
			}
			body = fmt.Sprintf("Your input %s is in %s: read it whole.\n", name, rel)
		}
		text = strings.TrimRight(text+"\n\n## Input: "+name+"\n\n"+body, "\n")
	}
	viaSubmit := submits(r, role)
	if rv != nil {
		// The review request holds the answer's instructions, and the rules of a finding.
		request, err := jormungandr.ReviewRequest(rv.ID, nil, nil, viaSubmit)
		if err != nil {
			return "", err
		}
		return text + "\n\n" + request, nil
	}
	answer, err := jormungandr.AnswerInstructions(schema.Contract(output), viaSubmit)
	if err != nil {
		return "", err
	}
	return text + "\n\n## Your answer\n\n" + answer + "\n", nil
}

// withTurnBudget is text, a session's whole prompt, with the line that tells it its turn budget at its end, after
// its answer section (jormungandr.TurnBudget). turns is the cap the session is given: its node's max_turns, node,
// bounded by the turns the caps still allow (turnsFor). A node with no max_turns of its own runs to its adapter's
// limit, and gets no line, even when the caps bound it.
func withTurnBudget(text string, node, turns int) string {
	line := jormungandr.TurnBudget(turns)
	if node <= 0 || line == "" {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n\n" + line + "\n"
}

// sessionInputs lists the inputs of a session of node n, or of its reviewer rv when rv is set, in order: what it
// declares, then what the node reviews when it does not list it (ruling R12). A loop's sessions declare the loop's
// own sources (change, open_findings, check_results) as any other (ruling R37).
func sessionInputs(n schema.Node, rv *schema.Reviewer) []string {
	inputs := n.Input
	if rv != nil {
		inputs = rv.Input
	} else if n.Reviews != "" && !slices.Contains(inputs, n.Reviews) {
		inputs = append(slices.Clone(inputs), n.Reviews)
	}
	return inputs
}

// workspaceNote tells a scratch agent what its workspace holds. Its documents are its answer: Odin publishes the
// views it renders from the typed output (change typed-boundaries), so out/ holds drafts only.
func workspaceNote() string {
	return "## Your workspace\n\nYou work in the folder you start in. It is not a repository, and nothing in it is committed by you.\n" +
		"- Your inputs are the sections below. An input too large for this prompt is a file under " + scratchInputs + "/, which its section names.\n" +
		"- out/ is for drafts: you may build a large answer there as a JSON file, check it as you go, and then hand it back as your answer says. " +
		"Nothing in out/ leaves this workspace: Odin publishes the documents it renders from your answer.\n"
}

// inputFolder is where the input files of one session go: base is the session's workspace, folder the folder in it.
type inputFolder struct {
	base, folder string
	exclude      func() error // makes the folder one git ignores, before the first file; nil for a workspace
}

// inputFiles is the input folder of node n's sessions, emptied: in a scratch node's workspace, or in the repo it
// works in. The folder of a repo is removed whole, a link included, never followed.
func inputFiles(r *run.Run, n schema.Node) (*inputFolder, error) {
	if n.Scratch {
		f := &inputFolder{base: r.ScratchDir(n.ID), folder: scratchInputs}
		return f, os.RemoveAll(filepath.Join(f.base, f.folder))
	}
	repo := r.Setup.Repos[n.Repo]
	f := &inputFolder{base: repo.Dir, folder: repoInputs}
	if repo.GitDir != "" {
		// A leaf's git dir, or an umbrella's: either way Odin's, and outside the working copy.
		f.exclude = func() error { return git.Repo{GitDir: repo.GitDir, WorkTree: repo.Dir}.Exclude("/" + repoInputs + "/") }
	}
	return f, dropInputs(r, n)
}

// dropInputs removes the input files of node n's sessions from the repo it works in, after a session and before
// its checkpoint: nothing of them is ever committed (ruling R36). A link in their place is removed, not followed.
func dropInputs(r *run.Run, n schema.Node) error {
	if n.Scratch {
		return nil
	}
	return os.RemoveAll(filepath.Join(r.RepoDir(n.Repo), repoInputs))
}

func (f *inputFolder) write(name, body string) (string, error) {
	if f.exclude != nil {
		if err := f.exclude(); err != nil {
			return "", err
		}
		f.exclude = nil
	}
	return writeInputFile(f.base, f.folder, name, body)
}

// inputName is the name of an input: a node id or a built-in source. It names one file, and never .git or
// .factory, nor anything outside the folder.
var inputName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// writeInputFile writes body to <folder>/<name>.md under base, and returns that path, relative to base. The folder
// is made when it is not there, and must be a folder of its own, not a link; the file is new, and is never
// written through a link.
func writeInputFile(base, folder, name, body string) (string, error) {
	if !inputName.MatchString(name) {
		return "", fmt.Errorf("%q can't name an input file", name)
	}
	dir := filepath.Join(base, folder)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if st, err := os.Lstat(dir); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("%s is %s, not a folder of the workspace's own", dir, st.Mode().Type())
	}
	p := filepath.Join(dir, name+".md")
	out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := out.WriteString(body); err != nil {
		out.Close()
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(folder, name+".md")), nil
}

// changed makes a prompt or a skill that changed during the run a failure of the node, before its session starts
// (ruling R56): a resume reads it again, and fails again until the file is back or a new run starts.
func changed(err error) error {
	var ce *run.ChangedError
	if errors.As(err, &ce) {
		return &nodeFailure{err}
	}
	return err
}
