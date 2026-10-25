package models

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ouroboric-dev/gungnir/schema"
)

// fake is a llama-server in router mode. Its /models body holds a raw newline
// in status.preset, like the real one.
type fake struct {
	mu       sync.Mutex
	status   string
	failed   bool
	loads    int
	loadBody string
	loadCode int
	onLoad   func()
	polls    int
	loadedAt int // polls before the status turns loaded after a load; 0 never
}

func (f *fake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.loads > 0 {
			f.polls++
			if f.loadedAt > 0 && f.polls >= f.loadedAt {
				f.status = "loaded"
			}
		}
		extra := ""
		if f.failed {
			extra = `,"failed":true,"exit_code":3`
		}
		w.Write([]byte(`{"data":[{"id":"other","status":{"value":"unloaded","preset":"[other]\nx = 1\n"}},` +
			`{"id":"qwen-a","aliases":[],"status":{"value":"` + f.status + `","args":["a"],"preset":"[qwen-a]\nctx = 1\n"` + extra + `}}]}`))
	})
	mux.HandleFunc("/models/load", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.loads++
		cb := f.onLoad
		code := f.loadCode
		body := f.loadBody
		f.mu.Unlock()
		if cb != nil {
			cb()
		}
		if code != 0 {
			w.WriteHeader(code)
		}
		if body == "" {
			body = `{"success":true}`
		}
		w.Write([]byte(body))
	})
	return mux
}

func setup(t *testing.T, f *fake) (*server, string) {
	t.Helper()
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	return newServer(t, ts.URL), ts.URL
}

func newServer(t *testing.T, rawURL string) *server {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	list := &schema.ModelList{
		Server: schema.ModelServerConfig{HostPort: port, State: t.TempDir()},
		Models: map[string]schema.LocalModelConfig{"qwen-a": {}},
	}
	s := New(list, http.DefaultClient).(*server)
	s.interval = 5 * time.Millisecond
	s.timeout = 2 * time.Second
	return s
}

func TestReadyRefusesAServerThatIsDown(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	u := ts.URL
	ts.Close()
	s := newServer(t, u)
	err := s.Ready(context.Background(), "qwen-a")
	if err == nil || !strings.Contains(err.Error(), "qwen-a") || !strings.Contains(err.Error(), "scripts/ygg-models.sh start") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyRefusesAMissingModel(t *testing.T) {
	f := &fake{status: "unloaded"}
	s, _ := setup(t, f)
	err := s.Ready(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	if f.loads != 0 {
		t.Fatal("load requested")
	}
	if _, err := os.Stat(filepath.Join(s.list.Server.State, "allowed")); err == nil {
		t.Fatal("allow file written for a missing model")
	}
}

func TestReadyWritesTheAllowFileFirst(t *testing.T) {
	f := &fake{status: "unloaded", loadedAt: 2}
	s, _ := setup(t, f)
	allow := filepath.Join(s.list.Server.State, "allowed")
	var got string
	f.onLoad = func() {
		b, err := os.ReadFile(allow)
		if err != nil {
			got = "ERR " + err.Error()
			return
		}
		got = string(b)
	}
	if err := s.Ready(context.Background(), "qwen-a"); err != nil {
		t.Fatal(err)
	}
	if got != "qwen-a\n" {
		t.Fatalf("allow file at load time = %q", got)
	}
	if f.loads != 1 {
		t.Fatalf("loads = %d", f.loads)
	}
	left, _ := filepath.Glob(filepath.Join(s.list.Server.State, "*"))
	if len(left) != 1 {
		t.Fatalf("state folder = %v", left)
	}
}

func TestReadySkipsALoadedModel(t *testing.T) {
	f := &fake{status: "loaded"}
	s, _ := setup(t, f)
	if err := s.Ready(context.Background(), "qwen-a"); err != nil {
		t.Fatal(err)
	}
	if f.loads != 0 {
		t.Fatalf("loads = %d", f.loads)
	}
	b, err := os.ReadFile(filepath.Join(s.list.Server.State, "allowed"))
	if err != nil || string(b) != "qwen-a\n" {
		t.Fatalf("allow file = %q, %v", b, err)
	}
}

func TestReadyReturnsACancelledContext(t *testing.T) {
	f := &fake{status: "loading"}
	s, _ := setup(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	err := s.Ready(ctx, "qwen-a")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyRefusesANameWithControlCharacters(t *testing.T) {
	f := &fake{status: "loaded"}
	s, _ := setup(t, f)
	if err := s.Ready(context.Background(), "qwen-a\nevil"); err == nil {
		t.Fatal("accepted")
	}
}

func TestReadyTimesOut(t *testing.T) {
	f := &fake{status: "loading"}
	s, _ := setup(t, f)
	s.timeout = 100 * time.Millisecond
	err := s.Ready(context.Background(), "qwen-a")
	if err == nil || !strings.Contains(err.Error(), "qwen-a") || !strings.Contains(err.Error(), "loaded") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyTreatsAlreadyRunningAsSuccess(t *testing.T) {
	f := &fake{status: "unloaded", loadedAt: 1, loadCode: 400,
		loadBody: `{"error":{"code":400,"message":"model is already running","type":"invalid_request_error"}}`}
	s, _ := setup(t, f)
	if err := s.Ready(context.Background(), "qwen-a"); err != nil {
		t.Fatal(err)
	}
}

func TestReadyRefusesAFailedLoad(t *testing.T) {
	f := &fake{status: "unloaded", failed: true}
	s, _ := setup(t, f)
	err := s.Ready(context.Background(), "qwen-a")
	if err == nil || !strings.Contains(err.Error(), "exit_code") || !strings.Contains(err.Error(), "3") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadyReportsARejectedLoad(t *testing.T) {
	f := &fake{status: "unloaded", loadCode: 500, loadBody: `{"error":"boom"}`}
	s, _ := setup(t, f)
	err := s.Ready(context.Background(), "qwen-a")
	if err == nil || !strings.Contains(err.Error(), "qwen-a") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}

func TestEscapeControlsOnlyInsideStrings(t *testing.T) {
	in := "{\n\"a\": \"x\ny\\\"\tz\"}"
	got := string(escapeControls([]byte(in)))
	if got != "{\n\"a\": \"x\\ny\\\"\\tz\"}" {
		t.Fatalf("got %q", got)
	}
}
