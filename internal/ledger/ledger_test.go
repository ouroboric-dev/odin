package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
)

const run = "20261015-090000-a1b2c3"

func write(t *testing.T, n int) (string, Anchor) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path, Anchor{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i := 0; i < n; i++ {
		if err := l.Append(gungnir.Event{Run: run, Node: "design", Name: gungnir.EventNodeStarted, Attempt: i + 1}); err != nil {
			t.Fatal(err)
		}
	}
	return path, l.Head()
}

func lines(t *testing.T, path string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

func TestLedgerChainsEveryLine(t *testing.T) {
	path, head := write(t, 3)
	ls := lines(t, path)
	sum := sha256.Sum256(ls[0])
	if !bytes.Contains(ls[1], []byte(`"prev":"`+hex.EncodeToString(sum[:])+`"`)) {
		t.Errorf("line 2 doesn't hold line 1's hash:\n%s", ls[1])
	}
	if !bytes.Contains(ls[0], []byte(`"prev":"`+Genesis+`"`)) {
		t.Errorf("line 1 doesn't start from the genesis hash:\n%s", ls[0])
	}
	got, err := Verify(path, head)
	if err != nil || got != head || got.Seq != 3 {
		t.Errorf("Verify() = %+v, %v; want %+v", got, err, head)
	}
}

func TestVerifyNamesTheChangedLine(t *testing.T) {
	path, head := write(t, 3)
	b, _ := os.ReadFile(path)
	changed := bytes.Replace(b, []byte(`"ygg.attempt":2`), []byte(`"ygg.attempt":7`), 1)
	if err := os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(path, head)
	if err == nil || !strings.Contains(err.Error(), "line 2 was changed") {
		t.Errorf("Verify() = %v, want an error that names line 2", err)
	}
}

func TestVerifyCatchesLinesCutFromTheEnd(t *testing.T) {
	path, head := write(t, 3)
	ls := lines(t, path)
	if err := os.WriteFile(path, append(bytes.Join(ls[:2], []byte("\n")), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(path, Anchor{}); err != nil {
		t.Fatalf("without an anchor the shorter chain is valid, but Verify() = %v", err)
	}
	if _, err := Verify(path, head); err == nil || !strings.Contains(err.Error(), "lines were removed") {
		t.Errorf("Verify() with the anchor = %v, want lines were removed", err)
	}
}

func TestVerifyRefusesATornLastLine(t *testing.T) {
	path, _ := write(t, 2)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"schema":"ygg.ledger/v1","seq":3`)
	f.Close()
	if _, err := Verify(path, Anchor{}); err == nil || !strings.Contains(err.Error(), ":3: the line is incomplete") {
		t.Errorf("Verify() = %v, want line 3 incomplete", err)
	}
	if _, err := Open(path, Anchor{}); err == nil {
		t.Error("Open() appends to a ledger with a torn line")
	}
}

func TestOpenContinuesTheChain(t *testing.T) {
	path, head := write(t, 2)
	l, err := Open(path, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(gungnir.Event{Run: run, Name: gungnir.EventRunResumed}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if got, err := Verify(path, l.Head()); err != nil || got.Seq != 3 {
		t.Errorf("Verify() = %+v, %v; want 3 good lines", got, err)
	}
}

func TestAppendRefusesAnEventTheSchemaRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := Open(path, Anchor{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Append(gungnir.Event{Run: "../../etc", Name: gungnir.EventRunStarted}); err == nil {
		t.Error("Append() wrote an event with a path as its run id")
	}
	if l.Head().Seq != 0 {
		t.Errorf("a refused event moved the head to %d", l.Head().Seq)
	}
}
