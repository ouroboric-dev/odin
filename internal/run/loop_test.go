package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/odin/internal/skillfake"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

// loopRealm copies the toy loop realm into a folder of its own, and lets a test change a file of it.
func loopRealm(t *testing.T, edit map[string]func(string) string) string {
	t.Helper()
	src, err := filepath.Abs("../../testdata/toy-loop")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	for name, f := range edit {
		b, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), []byte(f(string(b))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// loopConfig is a factory.yaml whose implementer and reviewer roles are scripted with the toy loop's script.
func loopConfig(t *testing.T, roles string) string {
	t.Helper()
	script, _ := filepath.Abs("../../testdata/toy-loop/script.yaml")
	body := strings.ReplaceAll(roles, "SCRIPT", script) + fullCaps
	return writeConfig(t, t.TempDir(), body)
}

const loopRoles = "roles:\n  implementer: { agent: scripted, model: none, script: SCRIPT }\n  reviewer: { agent: scripted, model: none, script: SCRIPT }\n"

func TestPrepareAcceptsALoopAndBuildsTheRolesOfItsReviewers(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	s, err := Prepare(ctx, Options{Project: project, Config: loopConfig(t, loopRoles), Graph: loopRealm(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if s.Agents["implementer"] == nil || s.Agents["reviewer"] == nil {
		t.Errorf("agents = %v; a loop's reviewers run as roles too", s.Agents)
	}
	if got := s.Leaves(); len(got) != 1 || got[0] != "repos/app" || len(s.Create) != 0 {
		t.Errorf("leaves %v, create %v", got, s.Create)
	}
}

func TestPrepareRefusesALoopThatCouldNotRun(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cases := map[string]struct {
		roles string
		edit  map[string]func(string) string
		want  string
	}{
		"a reviewer role factory.yaml lacks": {"roles:\n  implementer: { agent: scripted, model: none, script: SCRIPT }\n", nil, "uses role reviewer"},
		"an implementer role it lacks":       {"roles:\n  reviewer: { agent: scripted, model: none, script: SCRIPT }\n", nil, "uses role implementer"},
		"a reviewer prompt that is missing": {loopRoles, map[string]func(string) string{"graph.yaml": func(s string) string {
			return strings.Replace(s, "prompts/review-quality.md", "prompts/nope.md", 1)
		}}, "nope.md"},
		"a check the pack lacks": {loopRoles, map[string]func(string) string{"graph.yaml": func(s string) string {
			return strings.Replace(s, "checks: [unit]\n    reviewers", "checks: [unit, lint]\n    reviewers", 1)
		}}, "runs check lint"},
		"a loop in a repo that is an umbrella": {loopRoles, map[string]func(string) string{"graph.yaml": func(s string) string {
			return strings.Replace(s, "\n    repo: repos/app\n", "\n    repo: .\n", 1)
		}}, "umbrella"},
	}
	for name, c := range cases {
		_, err := Prepare(ctx, Options{Project: project, Config: loopConfig(t, c.roles), Graph: loopRealm(t, c.edit)})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Prepare() = %v, want an error containing %q", name, err, c.want)
		}
	}
}

func TestTheSkillsOfALoopsReviewersAreInTheRecord(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	realm := loopRealm(t, map[string]func(string) string{"graph.yaml": func(s string) string {
		return strings.Replace(s, "prompts/review-quality.md, input: [change, open_findings, check_results, rejection], output: ygg.review/v1 }", "prompts/review-quality.md, input: [change, open_findings, check_results, rejection], output: ygg.review/v1, skills: [plain-language] }", 1)
	}})
	lib := skillfake.Libraries(t, "plain-language", "Write plainly.")
	s, err := Prepare(ctx, Options{Project: project, Config: loopConfig(t, loopRoles), Graph: realm, Hooks: Hooks{Libs: lib}})
	if err != nil || strings.Join(s.Skills.Skills, ",") != "plain-language" || len(s.Skills.Hash) != 64 {
		t.Fatalf("Prepare() = %+v, %v; the reviewer's skill belongs in the record", s.Skills, err)
	}
	if _, err := Prepare(ctx, Options{Project: project, Config: loopConfig(t, loopRoles), Graph: realm, Hooks: Hooks{Libs: &skills.Libraries{Pinned: skillfake.Libraries(t, "other", "x").Pinned}}}); err == nil || !strings.Contains(err.Error(), "skill") {
		t.Errorf("Prepare() = %v; a reviewer that names a skill nobody vendored never starts", err)
	}
}

func TestDiscardWorkPutsALeafBackToItsCheckpointAndSavesWhatItRemoved(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: loopConfig(t, loopRoles), Graph: loopRealm(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dir := r.RepoDir("repos/app")
	if err := os.WriteFile(filepath.Join(dir, "half.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.DiscardWork(ctx, "repos/app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "half.txt")); err == nil {
		t.Error("half.txt is still in the leaf")
	}
	if len(r.Saved) != 1 || !strings.Contains(r.Saved[0], "refs/ygg/"+r.State.ID+"/discarded/1") {
		t.Errorf("Saved = %v; what a reset removes is saved to a ref first", r.Saved)
	}
	// Nothing changed since: nothing to save.
	if err := r.DiscardWork(ctx, "repos/app"); err != nil || len(r.Saved) != 1 {
		t.Errorf("DiscardWork() of a clean leaf: %v, saved %v", err, r.Saved)
	}
}

func TestALoopsStateSurvivesInRunJSON(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	r, err := Start(ctx, Options{Project: project, Config: loopConfig(t, loopRoles), Graph: loopRealm(t, nil)})
	if err != nil {
		t.Fatal(err)
	}
	id := r.State.ID
	r.State.Loops["build"] = &LoopRecord{Escalation: &Escalation{Reason: "attempts-exhausted", Detail: "3 attempts ended with 1 blocking finding(s) open"}}
	r.State.Loops["build"].State.Attempt = 2
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	s, err := LoadState(Dir(project, id))
	if err != nil {
		t.Fatal(err)
	}
	if rec := s.Loops["build"]; rec == nil || rec.State.Attempt != 2 || rec.Escalation == nil || rec.Escalation.Reason != "attempts-exhausted" {
		t.Errorf("loops = %+v", s.Loops)
	}
	fresh, _ := LoadState(Dir(project, id))
	if fresh.Loops == nil || fresh.Warned == nil {
		t.Error("the maps a state loads with are nil; the engine writes to them")
	}
}

func TestTheLoopSmokeRealmPreparesWithBothRealAdapters(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("ygg refuses to prepare a sandbox as root: the sandbox's user is the operator's own")
	}
	smoke, err := filepath.Abs("../../testdata/smoke-loop")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(smoke, "factory.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(cfg, b, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Prepare(ctx, Options{Project: treetest.Project(t), Config: cfg, Graph: smoke})
	if err != nil {
		t.Fatal(err)
	}
	n := s.Graph.Nodes[0]
	if n.Kind != "loop" || s.Agents["implementer"].Name() != "claude-code" || s.Agents["reviewer"].Name() != "opencode" ||
		strings.Join(s.Sandbox.Hosts, ",") != "api.anthropic.com,openrouter.ai" || len(n.Reviewers) != 1 || n.Attempts != 2 {
		t.Errorf("node %+v, agents %v, scratch egress %v (want the adapters' model APIs only)", n, s.Agents, s.Sandbox.Hosts)
	}
}
