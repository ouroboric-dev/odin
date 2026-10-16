package budget

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir"
)

func journal(t *testing.T, now time.Time) *Journal {
	t.Helper()
	return &Journal{Dir: filepath.Join(t.TempDir(), "spend"), Now: func() time.Time { return now }, Loc: time.UTC}
}

var noon = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func entry(run string, in, turns int64) Entry {
	return Entry{Run: run, Project: "/p", Node: "build", Usage: gungnir.Usage{InTok: in, OutTok: 1, Turns: turns, CostUSD: 0.5}}
}

func TestTheDaySumsEverySessionOfEveryRun(t *testing.T) {
	j := journal(t, noon)
	for _, e := range []Entry{entry("20261016-090000-aaaaaa", 100, 2), entry("20261016-100000-bbbbbb", 50, 1)} {
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := j.Day(noon)
	if err != nil || got.InTok != 150 || got.OutTok != 2 || got.Turns != 3 || got.CostUSD != 1 {
		t.Errorf("Day() = %+v, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(j.Dir, "2026-10-16.jsonl")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the journal file = %v, %v; want 0600", fi, err)
	}
	if got, err := j.Day(noon.Add(24 * time.Hour)); err != nil || got != (gungnir.Usage{}) {
		t.Errorf("tomorrow = %+v, %v; a new day starts at zero", got, err)
	}
}

func TestADayEndsAtTheOperatorsMidnight(t *testing.T) {
	zone := time.FixedZone("UTC-3", -3*3600)
	j := &Journal{Dir: t.TempDir(), Loc: zone}
	late := time.Date(2026, 10, 16, 23, 30, 0, 0, zone) // 02:30 UTC on the 17th
	early := time.Date(2026, 10, 17, 0, 30, 0, 0, zone) // 03:30 UTC on the 17th
	for _, tm := range []time.Time{late, early} {
		e := entry("20261016-090000-aaaaaa", 10, 1)
		e.Time = tm
		if err := j.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := j.Day(late); got.InTok != 10 {
		t.Errorf("the 16th = %+v; 23:30 local belongs to the 16th although it is the 17th in UTC", got)
	}
	if got, _ := j.Day(early); got.InTok != 10 {
		t.Errorf("the 17th = %+v", got)
	}
}

func TestRunsAppendingAtOnceNeverMixTheirLines(t *testing.T) {
	j := journal(t, noon)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 25; k++ {
				if err := j.Append(entry("20261016-090000-aaaaaa", 1, 1)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if got, err := j.Day(noon); err != nil || got.InTok != 500 {
		t.Errorf("Day() = %+v, %v; want 500 tokens from 500 whole lines", got, err)
	}
}

func TestACutShortLastLineIsIgnoredAndAnyOtherBadLineStopsTheCount(t *testing.T) {
	j := journal(t, noon)
	j.Append(entry("20261016-090000-aaaaaa", 100, 1))
	f, _ := os.OpenFile(j.File(noon), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"time":"2026-10-16T12:00:00Z","run":"20261016-0900`) // a crash in the middle of a write
	f.Close()
	if got, err := j.Day(noon); err != nil || got.InTok != 100 {
		t.Fatalf("Day() = %+v, %v; the torn last line is ignored", got, err)
	}
	f, _ = os.OpenFile(j.File(noon), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("\n" + `{"usage":{"in_tok":-5}}` + "\n")
	f.Close()
	if _, err := j.Day(noon); err == nil || !strings.Contains(err.Error(), "2026-10-16.jsonl:2") {
		t.Errorf("Day() = %v; a bad line in the middle must be an error that names it", err)
	}
	// An entry with a field Odin never writes is not Odin's.
	k := journal(t, noon)
	os.MkdirAll(k.Dir, 0o700)
	os.WriteFile(k.File(noon), []byte(`{"time":"2026-10-16T12:00:00Z","run":"r","project":"/p","node":"n","usage":{},"free":"text"}`+"\n"), 0o600)
	if _, err := k.Day(noon); err == nil {
		t.Error("an entry with an unknown field was counted")
	}
}

func TestTheDefaultFolderFollowsXDGThenHome(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	home := func() (string, error) { return "/home/ana", nil }
	if got, err := DefaultDir(env(map[string]string{"XDG_STATE_HOME": "/state"}), home); err != nil || got != "/state/ygg/spend" {
		t.Errorf("with XDG_STATE_HOME: %q, %v", got, err)
	}
	if got, err := DefaultDir(env(nil), home); err != nil || got != "/home/ana/.local/state/ygg/spend" {
		t.Errorf("without it: %q, %v", got, err)
	}
	if got, err := DefaultDir(env(map[string]string{"XDG_STATE_HOME": "relative"}), home); err != nil || got != "/home/ana/.local/state/ygg/spend" {
		t.Errorf("with a relative XDG_STATE_HOME: %q, %v; the XDG rule ignores it", got, err)
	}
	if _, err := DefaultDir(env(nil), func() (string, error) { return "", os.ErrNotExist }); err == nil {
		t.Error("no home and no XDG_STATE_HOME: DefaultDir() = nil error")
	}
}
