package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/ouroboric-dev/gleipnir/sandbox"
)

func TestModelsServerPrintsShellLines(t *testing.T) {
	code, out, errOut := ygg(t, "models", "server", "testdata/models/models.yaml")
	want := "NAME=ygg-models\nNETWORK=ygg-models\nPORT=8089\nHOST_PORT=8088\nWEIGHTS=/abs/weights\nSTATE=/abs/state\n"
	if code != ExitDone || out != want {
		t.Errorf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}

func TestModelsPresetPrintsTheIni(t *testing.T) {
	want, err := os.ReadFile("testdata/models/preset.ini")
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := ygg(t, "models", "preset", "testdata/models/models.yaml")
	if code != ExitDone || out != string(want) {
		t.Errorf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}

func TestModelsRefusesABadList(t *testing.T) {
	bad := t.TempDir() + "/models.yaml"
	os.WriteFile(bad, []byte("server: {}\n"), 0o644)
	for _, sub := range []string{"preset", "server"} {
		if code, out, _ := ygg(t, "models", sub, bad); code != ExitRefused || out != "" {
			t.Errorf("models %s on a bad list: exit %d, stdout %q", sub, code, out)
		}
	}
	if code, _, _ := ygg(t, "models", "server"); code != ExitRefused {
		t.Errorf("models server without a file: exit %d", code)
	}
}

func TestModelsImagePrintsTheTagAlone(t *testing.T) {
	code, out, _ := ygg(t, "models", "image")
	if code != ExitDone || out != sandbox.ModelsImage()+"\n" {
		t.Errorf("exit %d, stdout %q", code, out)
	}
}

func TestModelsBuildPrintsTheTag(t *testing.T) {
	log := stubEngine(t)
	_, config := project(t, "script.yaml", sandboxExtra)
	code, out, errOut := ygg(t, "models", "build", "--config", config, "--cuda-arch", "89")
	if code != ExitDone || strings.TrimSpace(out) != sandbox.ModelsImage() {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--build-arg CUDA_ARCH=89") {
		t.Errorf("the build did not carry the architecture:\n%s", b)
	}
	if code, _, _ := ygg(t, "models", "build", "--config", config, "--cuda-arch", "x;y"); code != ExitRefused {
		t.Errorf("a bad architecture: exit %d", code)
	}
}

// A shell reads the lines unquoted, so a value that is not a plain path is refused with nothing on stdout.
func TestModelsServerRefusesAPathAShellCouldMisread(t *testing.T) {
	list, err := os.ReadFile("testdata/models/models.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"weights", "state"} {
		for name, bad := range map[string]string{
			"newline":  `/abs/x\nNAME=evil`,
			"space":    `/abs/my weights`,
			"subshell": `/abs/$(id)`,
			"backtick": "/abs/`id`",
		} {
			body := strings.Replace(string(list), key+": /abs/"+map[string]string{"weights": "weights", "state": "state"}[key], key+": \""+bad+"\"", 1)
			if body == string(list) {
				t.Fatal("the fixture changed")
			}
			f := t.TempDir() + "/models.yaml"
			os.WriteFile(f, []byte(body), 0o644)
			code, out, errOut := ygg(t, "models", "server", f)
			if code != ExitRefused || out != "" || !strings.Contains(errOut, key) {
				t.Errorf("%s with a %s: exit %d, stdout %q, stderr %q; want a refusal naming the key", key, name, code, out, errOut)
			}
		}
	}
	good := strings.Replace(string(list), "weights: /abs/weights", "weights: /srv/ygg_models/w-1.0@x+y", 1)
	f := t.TempDir() + "/models.yaml"
	os.WriteFile(f, []byte(good), 0o644)
	if code, out, _ := ygg(t, "models", "server", f); code != ExitDone || !strings.Contains(out, "WEIGHTS=/srv/ygg_models/w-1.0@x+y\n") {
		t.Errorf("a plain path: exit %d, stdout %q", code, out)
	}
}
