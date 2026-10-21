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
	if err != nil || len(got) != 1 {
		t.Fatalf("found %+v, error %v", got, err)
	}
	if got[0].ID != id || got[0].Package != "golang.org/x/text" || got[0].Product != "api" {
		t.Errorf("found %+v", got[0])
	}
}
