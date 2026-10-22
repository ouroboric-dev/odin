package metrics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

// Now is where a run is, for the live view.
type Now struct {
	Node    string
	Attempt int
	Role    string
	Since   time.Time // when the node started
	Between bool      // no node is running: Node is the one that finished last
}

// Current reads the end of a run's ledger. The live view reads a ledger another process is writing, so it does
// not check the hash chain, ignores a last line that is still being written, skips a line it cannot read, and is
// not evidence: ygg metrics is. The strings it returns are cleaned, because the ledger may hold text an agent wrote.
func Current(dir string) (Now, error) {
	data, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		return Now{}, err
	}
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
		data = data[:i+1]
	} else {
		data = nil
	}
	var now Now
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e gungnir.Event
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		switch e.Name {
		case gungnir.EventNodeStarted:
			now = Now{Node: e.Node, Attempt: e.Attempt, Role: e.Role, Since: e.Time}
		case gungnir.EventNodeFinished, gungnir.EventNodeFailed:
			if e.Node == now.Node && e.Attempt == now.Attempt {
				now.Between = true
			}
		}
	}
	now.Node, now.Role = Clean(now.Node), Clean(now.Role)
	return now, nil
}
