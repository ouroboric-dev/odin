package cli

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localProject makes a project whose factory.yaml has a local role on model and a model list whose server
// answers on hostPort. network is what the stubbed podman says of the model network's Internal.
func localProject(t *testing.T, model string, hostPort string, network string) (dir, config string) {
	t.Helper()
	log := stubEngine(t)
	bin := filepath.Dir(log)
	// stubEngine's podman says nothing of a network; this one answers inspect and defers the rest.
	real, _ := os.ReadFile(filepath.Join(bin, "podman"))
	body := strings.Replace(string(real), "  network) [ \"$2\" = exists ] && exit 1 ;;",
		"  network) [ \"$2\" = exists ] && exit 1; [ \"$2\" = inspect ] && echo "+network+" ;;", 1)
	if body == string(real) {
		t.Fatal("the stub's network case changed")
	}
	os.WriteFile(filepath.Join(bin, "podman"), []byte(body), 0o755)
	list, _ := os.ReadFile("testdata/models/models.yaml")
	listFile := filepath.Join(t.TempDir(), "models.yaml")
	os.WriteFile(listFile, []byte(strings.Replace(string(list), "host_port: 8088", "host_port: "+hostPort, 1)), 0o644)
	dir, config = project(t, "script.yaml", sandboxExtra+"models:\n  file: "+listFile+"\n")
	b, _ := os.ReadFile(config)
	b = []byte(strings.Replace(string(b), "roles:\n", "roles:\n  local: { agent: opencode, model: "+model+" }\n", 1))
	os.WriteFile(config, b, 0o644)
	return dir, config
}

func serverPort(t *testing.T, s *httptest.Server) string {
	u, _ := url.Parse(s.URL)
	return u.Port()
}

func TestDoctorModelsWarnsWhenTheServerIsDown(t *testing.T) {
	dir, config := localProject(t, "local/qwen3.8-27b", "1", "true") // nothing listens on port 1
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if !strings.Contains(out, "WARN  models: ") || !strings.Contains(out, "scripts/ygg-models.sh start") {
		t.Errorf("doctor: exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "ERROR models") {
		t.Errorf("a server that is down is a warning:\n%s", out)
	}
}

func TestDoctorModelsFailsOnAMissingModel(t *testing.T) {
	dir, config := localProject(t, "local/nope", "1", "true")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitFailed || !strings.Contains(out, "ERROR models: ") || !strings.Contains(out, "nope") {
		t.Errorf("doctor: exit %d:\n%s", code, out)
	}
}

func TestDoctorModelsWarnsWhenTheNetworkIsNotInternal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[]}`)) }))
	defer srv.Close()
	dir, config := localProject(t, "local/qwen3.8-27b", serverPort(t, srv), "false")
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if !strings.Contains(out, "WARN  models: ") || !strings.Contains(out, "not internal") || strings.Contains(out, "does not answer") {
		t.Errorf("doctor:\n%s", out)
	}
}

func TestDoctorModelsIsOKWhenTheServerAnswersOnAnInternalNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"data":[]}`)) }))
	defer srv.Close()
	dir, config := localProject(t, "local/qwen3.8-27b", serverPort(t, srv), "true")
	code, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if code != ExitDone || !strings.Contains(out, "OK    models: ") || strings.Contains(out, "WARN  models") {
		t.Errorf("doctor: exit %d:\n%s", code, out)
	}
}

func TestDoctorModelsAddsNothingWithoutALocalRole(t *testing.T) {
	stubEngine(t)
	dir, config := project(t, "script.yaml", sandboxExtra)
	_, out, _ := ygg(t, "doctor", "--project", dir, "--config", config)
	if strings.Contains(out, "models") {
		t.Errorf("doctor:\n%s", out)
	}
}
