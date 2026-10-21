package deps

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ouroboric-dev/gungnir"
)

// Unfinished is a bump whose ledger ends in a line with no verdict: the tool was about to run in Product,
// and nothing says it ended, so the product may still be changed.
type Unfinished struct {
	ID      string // the bump's folder under .factory/deps
	Package string
	Product string // the product's path from the project; empty when the line does not name it
	Ledger  string
}

// FindUnfinished lists the bumps under project/.factory/deps whose last deps_bump line has no verdict.
// It reads the lines as they are and does not check the chain. A ledger that can't be read is an error
// in the result, and the other bumps are still listed. The caller says whether a ygg still runs the bump.
func FindUnfinished(project string) ([]Unfinished, error) {
	root := filepath.Join(project, ".factory", "deps")
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Unfinished
	var errs []error
	for _, e := range entries {
		path := filepath.Join(root, e.Name(), "ledger.jsonl")
		last, err := lastBumpLine(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			errs = append(errs, err)
		case last != nil && last.Verdict == "":
			out = append(out, Unfinished{ID: e.Name(), Package: last.Package, Product: strings.TrimSuffix(last.Note, startedNote), Ledger: path})
		}
	}
	return out, errors.Join(errs...)
}

func lastBumpLine(path string) (*gungnir.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var last *gungnir.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for n := 1; sc.Scan(); n++ {
		var e gungnir.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if e.Name == gungnir.EventDepsBump {
			last = &e
		}
	}
	return last, sc.Err()
}
