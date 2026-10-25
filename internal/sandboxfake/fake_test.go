package sandboxfake

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ouroboric-dev/gleipnir/checks"
	"github.com/ouroboric-dev/gungnir"
)

// A typed check writes its result to the sandbox's own /tmp. The fake gives each sandbox a result of its own and
// never the host's /tmp, so tests that run at once can't read each other's results.
func TestATypedCheckGetsAResultOfItsSandboxsOwn(t *testing.T) {
	fake := &Sandbox{}
	ctx := context.Background()
	up := func(leaf string) gungnir.Runner {
		env, err := fake.Up(ctx, gungnir.SandboxSpec{Name: leaf, Run: "r", Leaf: filepath.Join(t.TempDir(), leaf)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(env.Workdir, 0o755); err != nil {
			t.Fatal(err)
		}
		return runner{fake, env}
	}
	a, b := up("a"), up("b")
	write := `printf '{"check":"unit","passed":true,"exit_code":0,"timed_out":false,"duration_ms":1,"passed_tests":["TestA"]}' > "$YGG_CHECK_RESULT"`
	res, err := checks.Run(ctx, a, checks.Spec{Name: "unit", Argv: []string{"sh", "-c", write}, Timeout: time.Minute, Typed: true}, filepath.Join(t.TempDir(), "a.log"))
	if err != nil || res.TypedErr != nil || res.Typed == nil || len(res.PassedTests()) != 1 {
		t.Fatalf("the typed check in a = %+v, %v; want its result read back", res, err)
	}
	if _, err := os.Stat(checks.ResultPath); err == nil {
		t.Errorf("the fake wrote the host's %s", checks.ResultPath)
	}
	res, err = checks.Run(ctx, b, checks.Spec{Name: "unit", Argv: []string{"true"}, Timeout: time.Minute, Typed: true}, filepath.Join(t.TempDir(), "b.log"))
	if err != nil || res.Typed != nil || res.TypedErr == nil {
		t.Errorf("the typed check in b = %+v, %v; it wrote no result, and must not read a's", res, err)
	}
	res, err = checks.Run(ctx, a, checks.Spec{Name: "unit", Argv: []string{"true"}, Timeout: time.Minute, Typed: true}, filepath.Join(t.TempDir(), "a2.log"))
	if err != nil || res.Typed != nil || res.TypedErr == nil {
		t.Errorf("the second typed check in a = %+v, %v; the result of the one before is removed first", res, err)
	}
}

type runner struct {
	s   *Sandbox
	env gungnir.Env
}

func (r runner) Run(ctx context.Context, c gungnir.Cmd) (gungnir.ExecResult, error) {
	return r.s.Exec(ctx, r.env, c)
}
