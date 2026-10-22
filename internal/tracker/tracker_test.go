package tracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/jormungandr"
	"github.com/ouroboric-dev/odin/internal/run"
)

const runID = "20261023-090000-a1b2c3"

func task(id, leaf string, status schema.TaskStatus, why string) schema.FeatureRecord {
	return schema.FeatureRecord{Feature: schema.Feature{ID: id, Title: "title of " + id, Layer: leaf}, Status: status, Why: why}
}

// project writes the two files the local tracker reads: .factory/features.json and the run it names.
func project(t *testing.T, tasks []schema.FeatureRecord, mutate func(*run.State)) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".factory", "runs", runID), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &schema.Features{Run: runID, Source: "docs/plan/tasks.md", SourceSHA256: strings.Repeat("a", 64), Tasks: tasks}
	if err := f.Save(filepath.Join(dir, ".factory", "features.json")); err != nil {
		t.Fatal(err)
	}
	st := &run.State{ID: runID, Status: run.StatusRunning, Mode: gungnir.ModeKeyPoints, Features: tasks, Loops: map[string]*run.LoopRecord{}}
	if mutate != nil {
		mutate(st)
	}
	if err := st.Save(filepath.Join(dir, ".factory", "runs", runID)); err != nil {
		t.Fatal(err)
	}
	return dir
}

func column(t *testing.T, b gungnir.Board, id string) gungnir.Card {
	t.Helper()
	for _, c := range b.Cards {
		if c.Task == id {
			return c
		}
	}
	t.Fatalf("task %s is not on the board: %+v", id, b.Cards)
	return gungnir.Card{}
}

func TestEveryTaskLandsInTheColumnItsStatusAndItsLoopSay(t *testing.T) {
	tasks := []schema.FeatureRecord{
		task("g-T1", "repos/api", schema.TaskDone, ""),
		task("g-T2", "repos/web", schema.TaskBlocked, "attempts-exhausted in implement"),
		task("g-T3", "repos/web", schema.TaskTodo, ""),
		task("g-T4", "repos/api", schema.TaskDoing, ""), // loop still implementing
		task("g-T5", "repos/api", schema.TaskDoing, ""), // loop in its ladder
		task("g-T6", "repos/api", schema.TaskDoing, ""), // loop passed, merge and pointer to go
		task("g-T7", "repos/api", schema.TaskDoing, ""), // waiting at its contract gate
	}
	dir := project(t, tasks, func(s *run.State) {
		s.Status, s.Pending, s.PendingGate = run.StatusStopped, "g-t7-contract-gate", gungnir.GatePointerContract
		s.Loops["g-t4-implement"] = &run.LoopRecord{State: jormungandr.State{Attempt: 1, Limit: 3, Phase: jormungandr.PhaseImplement}}
		s.Loops["g-t5-implement"] = &run.LoopRecord{State: jormungandr.State{Attempt: 2, Limit: 3, Phase: jormungandr.PhaseLadder}}
		s.Loops["g-t6-implement"] = &run.LoopRecord{State: jormungandr.State{Attempt: 1, Limit: 3, Phase: jormungandr.PhaseLadder, Passed: true}}
		s.Loops["g-t7-implement"] = &run.LoopRecord{State: jormungandr.State{Attempt: 1, Limit: 3, Phase: jormungandr.PhaseLadder, Passed: true}}
	})
	b, err := Local{Project: dir}.Board(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("the board is not valid: %v", err)
	}
	want := map[string]gungnir.Column{"g-T1": gungnir.ColumnDone, "g-T2": gungnir.ColumnBlocked, "g-T3": gungnir.ColumnReady,
		"g-T4": gungnir.ColumnInProgress, "g-T5": gungnir.ColumnVerifying, "g-T6": gungnir.ColumnVerifying, "g-T7": gungnir.ColumnNeedsYou}
	for id, col := range want {
		if got := column(t, b, id).Column; got != col {
			t.Errorf("%s is in %q, want %q", id, got, col)
		}
	}
	if c := column(t, b, "g-T2"); c.Why != "attempts-exhausted in implement" {
		t.Errorf("g-T2 Why = %q", c.Why)
	}
	if c := column(t, b, "g-T7"); !strings.Contains(c.Why, "pointer-contract") {
		t.Errorf("g-T7 Why = %q; want the gate it waits at", c.Why)
	}
	if got := b.Cards[0].Task; got != "g-T1" || b.Cards[6].Task != "g-T7" {
		t.Errorf("cards are not in the plan's order: %+v", b.Cards)
	}
}

func TestAFailedOrPausedRunPutsItsTaskInNeedsYouWithTheReason(t *testing.T) {
	dir := project(t, []schema.FeatureRecord{task("g-T1", "repos/api", schema.TaskDoing, "")}, func(s *run.State) {
		s.Status, s.Error = run.StatusFailed, "node g-t1-implement: the sandbox didn't start\nsecond line of the error"
		s.Loops["g-t1-implement"] = &run.LoopRecord{State: jormungandr.State{Attempt: 1, Limit: 3, Phase: jormungandr.PhaseImplement}}
	})
	b, _ := Local{Project: dir}.Board(context.Background())
	c := column(t, b, "g-T1")
	if c.Column != gungnir.ColumnNeedsYou || !strings.Contains(c.Why, "run failed") || strings.Contains(c.Why, "\n") {
		t.Errorf("card = %+v; want needs-you with a one-line 'run failed' reason", c)
	}
}

func TestAProjectWithNoPlanYetHasAnEmptyBoardNotAnError(t *testing.T) {
	b, err := Local{Project: t.TempDir()}.Board(context.Background())
	if err != nil || len(b.Cards) != 0 || b.Summary() != "no tasks" {
		t.Errorf("Board() = %+v, %v", b, err)
	}
}

func TestAFeaturesFileThatNamesAMissingRunStillGivesABoardFromItsStatuses(t *testing.T) {
	dir := project(t, []schema.FeatureRecord{task("g-T1", "repos/api", schema.TaskDoing, ""), task("g-T2", "repos/api", schema.TaskTodo, "")}, nil)
	if err := os.RemoveAll(filepath.Join(dir, ".factory", "runs")); err != nil {
		t.Fatal(err)
	}
	b, err := Local{Project: dir}.Board(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if column(t, b, "g-T1").Column != gungnir.ColumnInProgress || column(t, b, "g-T2").Column != gungnir.ColumnReady {
		t.Errorf("Board = %+v", b)
	}
}

func TestTheTextBoardListsEveryColumnWithItsCount(t *testing.T) {
	dir := project(t, []schema.FeatureRecord{task("g-T1", "repos/api", schema.TaskDone, ""), task("g-T2", "repos/web", schema.TaskBlocked, "attempts-exhausted")}, nil)
	b, _ := Local{Project: dir}.Board(context.Background())
	var w bytes.Buffer
	WriteBoard(&w, b)
	out := w.String()
	for _, want := range []string{"Ready (0)", "In progress (0)", "Verifying (0)", "Blocked (1)", "g-T2", "attempts-exhausted", "Needs you (0)", "Done (1)", "g-T1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the board lacks %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Ready") > strings.Index(out, "Done") {
		t.Error("the columns are not in reading order")
	}
}

func TestALongReasonIsCutOnTheBoardAndKeptOnTheCard(t *testing.T) {
	long := strings.Repeat("a long reason ", 30)
	dir := project(t, []schema.FeatureRecord{task("g-T1", "repos/api", schema.TaskBlocked, long)}, nil)
	b, _ := Local{Project: dir}.Board(context.Background())
	var w bytes.Buffer
	WriteBoard(&w, b)
	for _, l := range strings.Split(w.String(), "\n") {
		if strings.Contains(l, "g-T1") && len([]rune(l)) > 16+1+12+1+len("title of g-T1")+2+maxWhy+3+2 {
			t.Errorf("the board line is %d characters:\n%s", len([]rune(l)), l)
		}
	}
	if !strings.Contains(w.String(), "…]") || column(t, b, "g-T1").Why != strings.TrimSpace(long) {
		t.Errorf("the reason must be cut on the board with an ellipsis and whole on the card:\n%s", w.String())
	}
}
