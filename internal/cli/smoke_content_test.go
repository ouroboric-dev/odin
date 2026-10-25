package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/scripted"
	"github.com/ouroboric-dev/gungnir/schema"
	"go.yaml.in/yaml/v3"
)

// The content smoke run (v0-H15) reads these files on a real engine; this test keeps them loadable, and every
// scripted answer a valid document of its session's contract.
func TestTheContentSmokeFilesLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "smoke-content")
	if _, err := scripted.Load(filepath.Join(dir, "script.yaml")); err != nil {
		t.Errorf("script.yaml does not load: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "script.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var sc scripted.Script
	if err := yaml.Unmarshal(raw, &sc); err != nil {
		t.Fatal(err)
	}
	// Every scripted session answers with a document of the contract its node declares in the software graph
	// (change typed-boundaries): the whole final answer, which Odin validates as the real run would.
	for key, steps := range sc.Steps {
		c := smokeContentContract(key)
		if c == "" {
			t.Errorf("script.yaml answers for %s, which is no session of the software graph", key)
			continue
		}
		for i, st := range steps {
			if st.Result == nil || st.Result.Output == "" || st.Result.Report != nil {
				t.Errorf("%s, attempt %d: no output; a session answers with its %s document", key, i+1, c)
				continue
			}
			if _, err := schema.Decode[json.RawMessage](c, []byte(st.Result.Output)); err != nil {
				t.Errorf("%s, attempt %d: %v", key, i+1, err)
			}
		}
	}
	// The spec's leaves pin the runtime realm the real helheim-go needs, and the realms at the versions their packs
	// have; the plan is ready for those leaves, and each task has its text and its contract.
	spec, err := schema.Decode[schema.Spec](schema.ContractSpec, []byte(smokeContentOutput(t, sc, "spec")))
	if err != nil {
		t.Fatalf("the spec: %v", err)
	}
	if r := spec.Leaves.Runtime; r == nil || r.Realm != "niflheim-local" || r.RealmVersion != "0.1.0" {
		t.Errorf("the spec's leaves pin the runtime %+v, want niflheim-local 0.1.0", r)
	}
	for leaf, realm := range map[string]string{"repos/api": "helheim-go", "repos/web": "midgard-react"} {
		if l := spec.Leaves.Leaves[leaf]; l.Realm != realm || l.RealmVersion != "0.1.0" {
			t.Errorf("the spec's leaf %s is %+v, want %s 0.1.0", leaf, l, realm)
		}
	}
	plan, err := schema.Decode[schema.Plan](schema.ContractPlan, []byte(smokeContentOutput(t, sc, "plan")))
	if err != nil {
		t.Fatalf("the plan: %v", err)
	}
	if err := schema.CheckReady(schema.FeaturesFromPlan(plan), []string{"repos/api", "repos/web"}); err != nil {
		t.Errorf("the plan is not ready: %v", err)
	}
	for _, task := range plan.Tasks {
		if task.Text == "" || task.Contract == "" {
			t.Errorf("task %s has no text or no contract", task.ID)
		}
		key := strings.ToLower(task.ID) + "-implement"
		for _, s := range []string{key, key + "-spec-compliance", key + "-simplicity", key + "-quality"} {
			if len(sc.Steps[s]) == 0 {
				t.Errorf("script.yaml has no answer for the session %s of task %s", s, task.ID)
			}
		}
	}
	brief, err := os.ReadFile(filepath.Join(dir, "brief.md"))
	if err != nil || len(strings.TrimSpace(string(brief))) == 0 {
		t.Errorf("brief.md: %q, %v", brief, err)
	}
	example, err := os.ReadFile(filepath.Join(dir, "factory.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "implementer", "reviewer"} {
		if !strings.Contains(string(example), role+": { agent: scripted, model: none, script: ") {
			t.Errorf("factory.example.yaml gives the %s no script", role)
		}
	}
	if !strings.Contains(string(example), "graphs:\n  dir: /home/OPERATOR/yggdrasil/voluspa") {
		t.Errorf("factory.example.yaml has no graphs: dir for the graph pack of Voluspa")
	}
}

// smokeContentContract is the contract of the session key in the software graph of Voluspa: its stages by node id,
// and a task's implementer and the loop's three reviewers by their keys (<task>-implement[-<reviewer>]).
func smokeContentContract(key string) schema.Contract {
	stages := map[string]schema.Contract{"design": schema.ContractDesign, "spec": schema.ContractSpec, "conformance": schema.ContractConformance,
		"plan": schema.ContractPlan, "reuse-review": schema.ContractReuseReview}
	if c, ok := stages[key]; ok {
		return c
	}
	for _, rv := range []string{"spec-compliance", "simplicity", "quality"} {
		if strings.HasSuffix(key, "-implement-"+rv) {
			return schema.ContractReview
		}
	}
	if strings.HasSuffix(key, "-implement") {
		return schema.ContractImplement
	}
	return ""
}

// smokeContentOutput is the output of the first attempt of the session key.
func smokeContentOutput(t *testing.T, sc scripted.Script, key string) string {
	t.Helper()
	if len(sc.Steps[key]) == 0 || sc.Steps[key][0].Result == nil {
		t.Fatalf("script.yaml has no answer for %s", key)
	}
	return sc.Steps[key][0].Result.Output
}
