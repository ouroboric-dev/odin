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
// task's loop expands. A hash the run kept stays what it was. A file outside the pack or the realm is refused.
func (r *Run) ReadPrompt(realm, file string) ([]byte, error) {
	b, err := r.Setup.readPrompt(realm, file)
	if err != nil {
		return nil, err
	}
	if r.State.PromptSHA256 == nil {
		r.State.PromptSHA256 = map[string]string{}
	}
	if key := promptKey(realm, file); r.State.PromptSHA256[key] == "" {
		r.State.PromptSHA256[key] = sum(b)
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
	path, err := s.PromptFile(realm, file)
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
