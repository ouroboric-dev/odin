package tracker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"go.yaml.in/yaml/v3"
)

// DefaultProjectsFile is where the operator's list of projects is: local configuration on the operator's
// machine, never in a tree (change project-is-its-own-tree).
func DefaultProjectsFile() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "ouroboric", "projects.yaml")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "ouroboric", "projects.yaml")
}

// LoadProjects reads the list: a YAML file with a projects key holding absolute paths. Every path must be a
// project umbrella (an umbrella.yaml that sets root: true), listed once. One bad path refuses the whole list,
// and no board is shown from part of it.
func LoadProjects(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the list of projects can't be read: %w (it holds `projects:` and a list of absolute paths)", err)
	}
	var list struct {
		Projects []string `yaml:"projects"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(list.Projects) == 0 {
		return nil, fmt.Errorf("%s lists no project", path)
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range list.Projects {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%s: %q is not an absolute path (a ~ is not expanded)", path, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			return nil, fmt.Errorf("%s: %s is listed twice", path, p)
		}
		seen[p] = true
		u, err := schema.LoadUmbrella(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %s is not a project umbrella: %w", path, p, err)
		}
		if !u.Root {
			return nil, fmt.Errorf("%s: %s is not a project umbrella: its umbrella.yaml doesn't set root: true", path, p)
		}
		out = append(out, p)
	}
	return out, nil
}

// WriteAll writes one board after another, each under its project.
func WriteAll(w io.Writer, boards []gungnir.Board) {
	for i, b := range boards {
		if i > 0 {
			fmt.Fprintln(w, strings.Repeat("-", 40))
		}
		WriteBoard(w, b)
	}
}
