package deps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/odin/internal/ledger"
)

type failingOpener struct{}

func (failingOpener) Open(context.Context, Product) (gungnir.Runner, func(context.Context) error, error) {
	return nil, nil, errors.New("the sandbox did not start")
}

// A bump that stops after its started line leaves a ledger that FindUnfinished reads back: the contract
// between the line Bump writes and the line FindUnfinished parses.
func TestAnUnfinishedBumpsLedgerIsFoundAgain(t *testing.T) {
	w := newWorld(t, map[string]string{"api": "0.41.0"})
	project := t.TempDir()
	const id = "20261021-090000-b0b0b0"
	if err := os.MkdirAll(filepath.Join(project, ".factory", "deps", id), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Open(filepath.Join(project, ".factory", "deps", id, "ledger.jsonl"), ledger.Anchor{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	b := w.bumper()
	b.Open = failingOpener{}
	b.Log = func(e gungnir.Event) error { e.Run = id; return l.Append(e) }
	if _, err := b.Bump(context.Background(), "golang.org/x/text", "0.42.0", w.product("api"), nil); err == nil {
		t.Fatal("the bump did not stop")
	}
	got, err := FindUnfinished(project)
	if err != nil || len(got) != 0 {
		t.Fatalf("found %+v, error %v; the bump stopped with a closing line", got, err)
	}
}

// A ledger whose last deps_bump line has no verdict is found, with its product, behind a host line or not.
func TestALedgerEndingInAStartedLineIsFoundWithItsProduct(t *testing.T) {
	for _, host := range []bool{false, true} {
		project := t.TempDir()
		const id = "20261021-090000-b0b0b0"
		if err := os.MkdirAll(filepath.Join(project, ".factory", "deps", id), 0o755); err != nil {
			t.Fatal(err)
		}
		l, err := ledger.Open(filepath.Join(project, ".factory", "deps", id, "ledger.jsonl"), ledger.Anchor{})
		if err != nil {
			t.Fatal(err)
		}
		if host {
			if err := l.Append(gungnir.Event{Name: gungnir.EventDepsBump, Run: id, Note: HostNote}); err != nil {
				t.Fatal(err)
			}
		}
		if err := l.Append(gungnir.Event{Name: gungnir.EventDepsBump, Run: id, Package: "golang.org/x/text", From: "0.41.0", To: "0.42.0",
			Stage: "canary", Note: "api" + startedNote}); err != nil {
			t.Fatal(err)
		}
		l.Close()
		got, err := FindUnfinished(project)
		if err != nil || len(got) != 1 || got[0].ID != id || got[0].Package != "golang.org/x/text" || got[0].Product != "api" {
			t.Errorf("host %v: found %+v, error %v", host, got, err)
		}
	}
}

// A bump that stops after its started line is found when the line that closes it is missing, and not found
// once Bump writes it.
func TestABumpThatStoppedAtItsStartedLineIsFoundOnlyWithoutTheClosingLine(t *testing.T) {
	w := newWorld(t, map[string]string{"api": "0.41.0"})
	b := w.bumper()
	b.Open = failingOpener{}
	var lines []gungnir.Event
	b.Log = func(e gungnir.Event) error { lines = append(lines, e); return nil }
	if _, err := b.Bump(context.Background(), "golang.org/x/text", "0.42.0", w.product("api"), nil); err == nil {
		t.Fatal("the bump did not stop")
	}
	if len(lines) != 2 || lines[1].Verdict != "fail" || lines[1].Package != "golang.org/x/text" || lines[1].Stage != "canary" {
		t.Errorf("lines %+v", lines)
	}
}
