package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ouroboric-dev/gleipnir/sandbox"
	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/havamal"
	"github.com/ouroboric-dev/odin/internal/tree/treetest"
)

const sandboxSection = "sandbox:\n  engine: unix:///mnt/wsl/podman-sockets/podman-machine-default/podman-user.sock\n  mounts: [ { host: /home/ana/ygg, engine: /mnt/wsl/ygg-bind } ]\n"

func toySandbox(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/toy-sandbox")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// sandboxConfig writes a factory.yaml with a sandbox section, a scripted planner and the given
// implementer line.
func sandboxConfig(t *testing.T, implementer, extra string) string {
	t.Helper()
	script := filepath.Join(toySandbox(t), "script.yaml")
	body := "roles:\n  planner: { agent: scripted, model: none, script: " + script + " }\n  implementer: " + implementer + "\n" + fullCaps + sandboxSection + extra
	p := filepath.Join(t.TempDir(), "factory.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrepareSetsUpTheSandboxFromTheConfigAndThePack(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := sandboxConfig(t, "{ agent: claude-code, model: sonnet }", "")
	s, err := Prepare(ctx, Options{Project: project, Config: cfg, Realm: toySandbox(t)})
	if err != nil {
		t.Fatal(err)
	}
	if s.Sandbox == nil || s.Sandbox.Engine != "unix:///mnt/wsl/podman-sockets/podman-machine-default/podman-user.sock" ||
		s.Sandbox.Config.Images != sandbox.ImagesFor(s.Pack.Sandbox.Stack) || s.Sandbox.Config.Mounts[0].Engine != "/mnt/wsl/ygg-bind" {
		t.Errorf("Sandbox = %+v", s.Sandbox)
	}
	// The scripted planner needs no host; the claude-code implementer needs api.anthropic.com. The
	// realm's registries come with them, once each, sorted.
	if got := strings.Join(s.Sandbox.Egress, ","); got != "api.anthropic.com,proxy.golang.org,sum.golang.org" {
		t.Errorf("egress = %s", got)
	}
	if s.Agents["implementer"].Name() != "claude-code" {
		t.Errorf("the implementer is %s", s.Agents["implementer"].Name())
	}
}

func TestPrepareWithoutASandboxSectionLeavesTheHost(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	s, err := Prepare(ctx, Options{Project: project, Config: writeConfig(t, t.TempDir(), scriptedRoles+fullCaps), Realm: toy(t)})
	if err != nil || s.Sandbox != nil {
		t.Errorf("Prepare() = %v, sandbox %v; a scripted-only config runs on the host", err, s.Sandbox)
	}
}

// variant copies the sandboxed toy realm and rewrites one of its files.
func variant(t *testing.T, file string, change func(string) string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(toySandbox(t))); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(change(string(b))), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPrepareRefusesWhatWouldRunOutsideTheSandbox(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	script := "{ agent: scripted, model: none, script: " + filepath.Join(toySandbox(t), "script.yaml") + " }"
	cfg := sandboxConfig(t, script, "")
	// Acceptance checks run in the project umbrella, over several leaves: no sandbox holds them.
	acceptance := variant(t, "graph.yaml", func(g string) string {
		return strings.Replace(g, "kind: pointer, repo: repos/app, checks: [unit] }", "kind: pointer, repo: repos/app, checks: [unit], acceptance: [unit] }", 1)
	})
	if _, err := Prepare(ctx, Options{Project: project, Config: cfg, Realm: acceptance}); err == nil || !strings.Contains(err.Error(), "runs acceptance checks") {
		t.Errorf("a realm with acceptance checks and a sandbox: %v", err)
	}
	// A pack with no stack can't build an image.
	noStack := variant(t, "pack.yaml", func(p string) string { return strings.SplitN(p, "sandbox:", 2)[0] })
	if _, err := Prepare(ctx, Options{Project: project, Config: cfg, Realm: noStack}); err == nil || !strings.Contains(err.Error(), "names no sandbox stack") {
		t.Errorf("a pack without a stack: %v", err)
	}
	// privacy: strict with a model the adapter can't tighten stops the run before it starts.
	strict := sandboxConfig(t, "{ agent: opencode, model: anthropic/claude }", "privacy: strict\n")
	b, _ := os.ReadFile(strict)
	paid := strings.NewReplacer("turns: 100 }", "turns: 100, usd: 5 }", "turns: 200 }", "turns: 200, usd: 9 }").Replace(string(b))
	os.WriteFile(strict, []byte(paid), 0o644)
	if _, err := Prepare(ctx, Options{Project: project, Config: strict, Realm: toySandbox(t)}); err == nil || !strings.Contains(err.Error(), "openrouter") {
		t.Errorf("strict privacy on a model the adapter can't tighten: %v; want the adapter's refusal", err)
	}
}

func skillLib(t *testing.T) *skills.Libraries {
	t.Helper()
	text := "---\nname: plain-language\ndescription: Write plainly.\n---\nUse short words.\n"
	fsys := fstest.MapFS{"skills/.gitkeep": {}, "skills/plain-language/SKILL.md": {Data: []byte(text)}, "skills/plain-language/LICENSE": {Data: []byte("MIT\n")}}
	lock := &havamal.Lock{Skills: []havamal.Entry{{Name: "plain-language", Licence: "MIT",
		Upstream: &havamal.Upstream{Repo: "https://github.com/example/w", Path: "p", Commit: strings.Repeat("a", 40)},
		Files:    []havamal.File{{Path: "LICENSE", SHA256: havamal.Sum([]byte("MIT\n"))}, {Path: "SKILL.md", SHA256: havamal.Sum([]byte(text))}}}}}
	b, _ := lock.Marshal()
	fsys["skills.lock"] = &fstest.MapFile{Data: b}
	lib, err := havamal.Open(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return &skills.Libraries{Pinned: lib}
}

// realmWithSkills copies the toy realm and names skills on its design node.
func realmWithSkills(t *testing.T, names string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(toy(t))); err != nil {
		t.Fatal(err)
	}
	g, _ := os.ReadFile(filepath.Join(dir, "graph.yaml"))
	g2 := strings.Replace(string(g), "prompt: prompts/design.md,", "prompt: prompts/design.md, skills: ["+names+"],", 1)
	if err := os.WriteFile(filepath.Join(dir, "graph.yaml"), []byte(g2), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAGraphNamingASkillNobodyVendoredNeverStarts(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	if _, err := Start(ctx, Options{Project: project, Config: cfg, Realm: realmWithSkills(t, "no-such-skill")}); err == nil || !strings.Contains(err.Error(), "skill") {
		t.Errorf("Start() = %v, want a refusal naming the missing skill", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".factory")); err == nil {
		t.Error("a refused start created .factory/")
	}
}

func TestRunJSONRecordsTheSkillSetAndAResumeNeedsTheSameSkills(t *testing.T) {
	project := treetest.Project(t, "repos/app")
	cfg := writeConfig(t, t.TempDir(), scriptedRoles+fullCaps)
	realm := realmWithSkills(t, "plain-language")
	r, err := Start(ctx, Options{Project: project, Config: cfg, Realm: realm, Hooks: Hooks{Libs: skillLib(t)}})
	if err != nil {
		t.Fatal(err)
	}
	id, hash := r.State.ID, r.State.Skills.Hash
	r.Close()
	s, err := LoadState(Dir(project, id))
	if err != nil || len(hash) != 64 || s.Skills.Hash != hash || strings.Join(s.Skills.Skills, ",") != "plain-language" {
		t.Errorf("run.json skills = %+v, %v", s.Skills, err)
	}
	// A resume with a library that lacks the skill (a different Havamal version) is refused.
	if _, err := Open(ctx, project, id, ""); err == nil || !strings.Contains(err.Error(), "skill") {
		t.Errorf("Open() with the skill gone = %v, want a refusal", err)
	}
}
