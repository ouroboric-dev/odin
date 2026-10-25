package run

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/havamal"
)

// ReadPrompt reads the prompt file of a session from the realm called realm, or from the graph pack for "", and
// keeps its hash in run.json if the run has none for it yet (ruling R55): a realm's prompt is known only when a
// task's loop expands. A prompt whose bytes do not hash as the run kept is refused with a *ChangedError (ruling
// R56). A file outside the pack or the realm is refused.
func (r *Run) ReadPrompt(realm, file string) ([]byte, error) {
	b, err := r.Setup.readPrompt(realm, file)
	if err != nil {
		return nil, err
	}
	if r.State.PromptSHA256 == nil {
		r.State.PromptSHA256 = map[string]string{}
	}
	if err := r.State.keep(r.State.PromptSHA256, "prompt", promptKey(realm, file), sum(b)); err != nil {
		return nil, err
	}
	return b, nil
}

// promptKey names a prompt in run.json: its path in the graph pack, or <realm>:<path> for a realm's.
func promptKey(realm, file string) string {
	if realm == "" {
		return file
	}
	return realm + ":" + file
}

// readPrompt reads a prompt file inside the graph pack or a realm.
func (s *Setup) readPrompt(realm, file string) ([]byte, error) {
	if p := filepath.FromSlash(file); !filepath.IsLocal(p) || filepath.Clean(p) != p {
		return nil, fmt.Errorf("the prompt %q is not inside its pack", file)
	}
	path, err := s.promptFile(realm, file)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// fingerprints are the hash of every prompt the graph pack gives a session (a node's own, where the pack supplies
// it, and every reviewer's) and of every skill the run may load, as the run reads them when it starts.
func fingerprints(setup *Setup) (prompts, skillHashes map[string]string, err error) {
	prompts = map[string]string{}
	add := func(file string) error {
		if file == "" || prompts[file] != "" {
			return nil
		}
		b, err := setup.readPrompt("", file)
		if err != nil {
			return err
		}
		prompts[file] = sum(b)
		return nil
	}
	for _, n := range setup.Graph.All() {
		if n.RealmFrom != schema.RealmLeaf {
			if err := add(n.Prompt); err != nil {
				return nil, nil, err
			}
		}
		for _, rv := range n.Reviewers {
			if err := add(rv.Prompt); err != nil {
				return nil, nil, err
			}
		}
	}
	skillHashes = map[string]string{}
	if len(setup.Skills.Skills) > 0 {
		loaded, err := havamal.Select(setup.Skills.Skills, setup.Libs.Pinned, setup.Libs.Private)
		if err != nil {
			return nil, nil, err
		}
		for _, l := range loaded {
			skillHashes[l.Name] = havamal.SetHash([]havamal.Entry{l.Entry})
		}
	}
	return prompts, skillHashes, nil
}

// ChangedError is a prompt or a skill whose bytes are not the ones the run kept the hash of (ruling R56): a run uses
// one set of instructions, or its numbers compare with nothing. The session that would read it does not start.
type ChangedError struct {
	Kind, Name    string // "prompt" or "skill", and its key in run.json
	Recorded, Now string // the sha256 the run kept, and the one read now
}

func (e *ChangedError) Error() string {
	return fmt.Sprintf("the %s %s changed during the run (recorded sha256 %.12s…, now %.12s…); start a new run", e.Kind, e.Name, e.Recorded, e.Now)
}

// LoadSkills is the skills section of a session's prompt, for the skills it names, read from the libraries as they
// are now: the operator's private skills are read again from their folder. A skill whose files do not hash as the
// run kept is refused with a *ChangedError; one the run had not hashed yet is kept now, and checked from then on.
// The pinned library is compiled into ygg and cannot change while it runs.
func (r *Run) LoadSkills(names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	libs := r.Setup.Libs
	if r.Setup.privateDir != "" {
		p, err := havamal.OpenPrivate(r.Setup.privateDir)
		if err != nil {
			return "", err
		}
		libs.Private = p
	}
	loaded, err := havamal.Select(names, libs.Pinned, libs.Private)
	if err != nil {
		return "", err
	}
	if r.State.SkillSHA256 == nil {
		r.State.SkillSHA256 = map[string]string{}
	}
	for _, l := range loaded {
		if err := r.State.keep(r.State.SkillSHA256, "skill", l.Name, havamal.SetHash([]havamal.Entry{l.Entry})); err != nil {
			return "", err
		}
	}
	return libs.Load(names)
}

// keep keeps hash under name in kept if there is none, and refuses one that differs from what is kept.
func (s *State) keep(kept map[string]string, kind, name, hash string) error {
	switch was := kept[name]; {
	case was == "":
		kept[name] = hash
	case was != hash:
		return &ChangedError{Kind: kind, Name: name, Recorded: was, Now: hash}
	}
	return nil
}
