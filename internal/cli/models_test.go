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

func TestModelsImageTakesTheCUDAArchitectureLikeBuild(t *testing.T) {
	def := sandbox.ModelsImage() + "\n"
	for _, arch := range []string{"89", ""} {
		args := []string{"models", "image"}
		if arch != "" {
			args = append(args, "--cuda-arch", arch)
		}
		if code, out, _ := ygg(t, args...); code != ExitDone || out != def {
			t.Errorf("arch %q: exit %d, stdout %q, want %q", arch, code, out, def)
		}
	}
	want, err := sandbox.ModelsImageFor("86")
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := ygg(t, "models", "image", "--cuda-arch", "86")
	if code != ExitDone || out != want+"\n" || out == def {
		t.Errorf("exit %d, stdout %q, want %q and not the default %q", code, out, want, def)
	}
	code, out, errOut := ygg(t, "models", "image", "--cuda-arch", "x;y")
	if code != ExitRefused || out != "" || !strings.Contains(errOut, "CUDA architecture") {
		t.Errorf("a bad architecture: exit %d, stdout %q, stderr %q", code, out, errOut)
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

const modelsList = "testdata/models/models.yaml" // weights /abs/weights, state /abs/state

func TestModelsServerWithConfigPrintsEnginePaths(t *testing.T) {
	_, config := project(t, "script.yaml", sandboxExtra+"  mounts:\n    - { host: /abs, engine: /mnt/wsl/ygg-bind }\n")
	code, out, errOut := ygg(t, "models", "server", "--config", config, modelsList)
	want := "NAME=ygg-models\nNETWORK=ygg-models\nPORT=8089\nHOST_PORT=8088\nWEIGHTS=/abs/weights\nSTATE=/abs/state\n" +
		"ENGINE_WEIGHTS=/mnt/wsl/ygg-bind/weights\nENGINE_STATE=/mnt/wsl/ygg-bind/state\n"
	if code != ExitDone || out != want {
		t.Errorf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}

func TestModelsServerWithConfigRefusesAPathNoMountCovers(t *testing.T) {
	_, config := project(t, "script.yaml", sandboxExtra+"  mounts:\n    - { host: /elsewhere, engine: /mnt/e }\n")
	code, out, errOut := ygg(t, "models", "server", "--config", config, modelsList)
	if code != ExitRefused || out != "" || !strings.Contains(errOut, "weights") || !strings.Contains(errOut, "sandbox.mounts") || !strings.Contains(errOut, "/elsewhere") {
		t.Errorf("exit %d, stdout %q, stderr %q; want a refusal naming the key, sandbox.mounts and the host sides", code, out, errOut)
	}
}

func TestModelsServerWithConfigRefusesNoSandboxSection(t *testing.T) {
	_, config := project(t, "script.yaml", "")
	code, out, errOut := ygg(t, "models", "server", "--config", config, modelsList)
	if code != ExitRefused || out != "" || !strings.Contains(errOut, "sandbox") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestModelsServerWithConfigRefusesAnEnginePathAShellCouldMisread(t *testing.T) {
	_, config := project(t, "script.yaml", sandboxExtra+"  mounts:\n    - { host: /abs, engine: '/mnt/my bind' }\n")
	code, out, errOut := ygg(t, "models", "server", "--config", config, modelsList)
	if code != ExitRefused || out != "" || !strings.Contains(errOut, "weights") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestModelsSourcesPrintsOneLinePerFile(t *testing.T) {
	list, err := os.ReadFile("testdata/models/models.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// qwen3.8-27b sorts after mtp-27b: the one with no download prints nothing, the other has no revision.
	body := strings.Replace(string(list), "    file: model.gguf\n", "    file: model.gguf\n    download:\n      repo: acme/qwen-gguf\n      revision: abc123\n      files: [model.gguf, sub/extra.bin]\n", 1)
	body = strings.Replace(body, "    file: mtp.gguf\n", "    file: mtp.gguf\n    download:\n      repo: acme/mtp\n      files: [mtp.gguf]\n", 1)
	f := t.TempDir() + "/models.yaml"
	os.WriteFile(f, []byte(body), 0o644)
	code, out, errOut := ygg(t, "models", "sources", f)
	want := "mtp-27b acme/mtp main mtp.gguf\nqwen3.8-27b acme/qwen-gguf abc123 model.gguf\nqwen3.8-27b acme/qwen-gguf abc123 sub/extra.bin\n"
	if code != ExitDone || out != want {
		t.Errorf("exit %d, stderr %q, stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
	if code, out, _ := ygg(t, "models", "sources", "testdata/models/models.yaml"); code != ExitDone || out != "" {
		t.Errorf("no download: exit %d, stdout %q", code, out)
	}
}

func TestModelsSourcesRefusesABadList(t *testing.T) {
	bad := t.TempDir() + "/models.yaml"
	os.WriteFile(bad, []byte("server: {}\n"), 0o644)
	if code, out, _ := ygg(t, "models", "sources", bad); code != ExitRefused || out != "" {
		t.Errorf("bad list: exit %d, stdout %q", code, out)
	}
	if code, out, _ := ygg(t, "models", "sources"); code != ExitRefused || out != "" {
		t.Errorf("no file: exit %d, stdout %q", code, out)
	}
}
