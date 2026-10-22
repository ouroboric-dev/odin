package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/ouroboric-dev/gungnir"
)

func TestCurrentNamesTheNodeThatIsRunningAndItsAttempt(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "plan", Attempt: 1})
	f.log(5, gungnir.Event{Name: gungnir.EventNodeFinished, Node: "plan", Attempt: 1})
	f.log(6, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "g-t1-implement", Attempt: 2, Role: "implementer"})
	now, err := Current(f.done())
	if err != nil {
		t.Fatal(err)
	}
	if now.Node != "g-t1-implement" || now.Attempt != 2 || now.Role != "implementer" || !now.Since.Equal(t0.Add(6*time.Minute)) || now.Between {
		t.Errorf("Current() = %+v", now)
	}
}

func TestCurrentBetweenNodesSaysWhatFinishedLast(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted})
	f.log(1, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "plan", Attempt: 1})
	f.log(5, gungnir.Event{Name: gungnir.EventNodeFinished, Node: "plan", Attempt: 1})
	now, err := Current(f.done())
	if err != nil || !now.Between || now.Node != "plan" || now.Attempt != 1 {
		t.Errorf("Current() = %+v, %v; want between nodes, after plan", now, err)
	}
	empty := newFake(t)
	if now, err := Current(empty.done()); err != nil || now.Node != "" {
		t.Errorf("a run with no line: Current() = %+v, %v", now, err)
	}
}

// The live view reads a ledger another process is writing: a half-written last line is not an error.
func TestCurrentIgnoresALastLineThatIsStillBeingWritten(t *testing.T) {
	f := newFake(t)
	f.log(0, gungnir.Event{Name: gungnir.EventRunStarted}).log(1, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "plan", Attempt: 1})
	dir := f.done()
	appendRaw(t, dir, `{"schema":"ygg.ledger/v1","seq":3,"ti`)
	if now, err := Current(dir); err != nil || now.Node != "plan" {
		t.Errorf("Current() = %+v, %v", now, err)
	}
}

// writeLedger writes ledger lines as they are, with no hash chain: the live view does not verify it.
func writeLedger(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	text := ""
	for _, l := range lines {
		text += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "ledger.jsonl"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ledgerLine(t *testing.T, e gungnir.Event) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The node name comes from the ledger, which a process may have tampered with: the live view prints it cleaned.
func TestCurrentCleansTheNodeItNames(t *testing.T) {
	dir := writeLedger(t, ledgerLine(t, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "a\x1b[2J\nb‮", Attempt: 1, Time: t0}))
	now, err := Current(dir)
	if err != nil {
		t.Fatal(err)
	}
	if now.Node == "" {
		t.Fatal("Current() named no node")
	}
	for _, r := range now.Node {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Errorf("Current().Node = %q holds control or format rune %U", now.Node, r)
		}
	}
	if strings.ContainsAny(now.Node, "\n\r") {
		t.Errorf("Current().Node = %q holds a newline", now.Node)
	}
}

// One line this reader cannot parse is not evidence: the live view skips it and names the last node that started.
func TestCurrentSkipsALineItCannotRead(t *testing.T) {
	dir := writeLedger(t,
		ledgerLine(t, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "plan", Attempt: 1, Time: t0}),
		`{not a ledger line`,
		ledgerLine(t, gungnir.Event{Name: gungnir.EventNodeStarted, Node: "g-t1-implement", Attempt: 2, Time: t0.Add(time.Minute)}))
	now, err := Current(dir)
	if err != nil || now.Node != "g-t1-implement" || now.Attempt != 2 {
		t.Errorf("Current() = %+v, %v; want the last good node_started, g-t1-implement attempt 2", now, err)
	}
}
