package run

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

func TestRunIDs(t *testing.T) {
	id, err := NewID(time.Date(2026, 10, 15, 14, 22, 33, 0, time.UTC), bytes.NewReader([]byte{0xa1, 0xb2, 0xc3}))
	if err != nil || id != "20261015-142233-a1b2c3" {
		t.Errorf("NewID() = %q, %v", id, err)
	}
	if id, err := NewIDNow(); err != nil || !ValidID(id) {
		t.Errorf("NewIDNow() = %q, %v", id, err)
	}
	for _, bad := range []string{"", "../../etc", "20261015-142233-a1b2c3/..", "20261015-142233-A1B2C3", "x"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}

func TestStateSaveIsAtomicAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s := &State{ID: "20261015-142233-a1b2c3", Status: StatusStopped, Mode: gungnir.ModeKeyPoints,
		Position: 2, Attempts: map[string]int{"design": 1}, Pending: "design-gate",
		Usage: gungnir.Usage{InTok: 5, Turns: 1}}
	if err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pending != "design-gate" || got.Attempts["design"] != 1 || got.Usage.InTok != 5 || got.Schema != StateSchema {
		t.Errorf("LoadState() = %+v", got)
	}
	if got.Reasons == nil || got.Summaries == nil {
		t.Error("LoadState() left a map nil; the engine writes to it")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("Save() left %d files, want only run.json", len(entries))
	}
}

func TestLoadStateRefusesWhatOdinDidntWrite(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"an unknown field": `{"schema":"ygg.run/v1","id":"x","pwned":true}`,
		"another schema":   `{"schema":"ygg.run/v9","id":"x"}`,
		"not JSON":         `{`,
	} {
		os.WriteFile(filepath.Join(dir, "run.json"), []byte(body), 0o644)
		if _, err := LoadState(dir); err == nil {
			t.Errorf("%s: LoadState() = nil error", name)
		}
	}
}

func TestLockAllowsOneProcess(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); !errors.Is(err, ErrInUse) {
		t.Errorf("a second Lock() = %v, want ErrInUse", err)
	}
	if !Locked(dir) {
		t.Error("Locked() = false while held")
	}
	unlock()
	if Locked(dir) {
		t.Error("Locked() = true after unlock")
	}
	if !strings.Contains(ErrInUse.Error(), "another ygg process") {
		t.Error("ErrInUse doesn't say what holds the run")
	}
}
