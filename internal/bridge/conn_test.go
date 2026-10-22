package bridge

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// adapter plays the Harbor agent adapter (bench/harbor/ygg_harbor/agent.py) on the other end of the pipes: it
// sends the start line, then answers each request line with the handler of its op, one at a time.
type adapter struct {
	t        *testing.T
	toYgg    *io.PipeWriter
	fromYgg  *bufio.Reader
	mu       sync.Mutex
	requests []map[string]any
}

func newConn(t *testing.T, start string) (*Conn, *adapter) {
	t.Helper()
	inR, inW := io.Pipe()   // adapter to factory
	outR, outW := io.Pipe() // factory to adapter
	a := &adapter{t: t, toYgg: inW, fromYgg: bufio.NewReader(outR)}
	go func() {
		if start != "" {
			inW.Write([]byte(start + "\n")) //nolint:errcheck // the pipe closes when the test ends
		}
	}()
	t.Cleanup(func() { inW.Close(); outW.Close() })
	return NewConn(inR, outW), a
}

// answer reads one request and replies with reply(request). It returns the request.
func (a *adapter) answer(reply func(map[string]any) string) map[string]any {
	a.t.Helper()
	line, err := a.fromYgg.ReadBytes('\n')
	if err != nil {
		a.t.Fatalf("the factory wrote no request: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(line, &req); err != nil {
		a.t.Fatalf("the factory wrote a line that is not a request: %q", line)
	}
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	if _, err := a.toYgg.Write([]byte(reply(req) + "\n")); err != nil {
		a.t.Fatal(err)
	}
	return req
}

func ok(fields string) func(map[string]any) string {
	return func(req map[string]any) string {
		id, _ := json.Marshal(req["id"])
		return `{"id":` + string(id) + `,"ok":true` + fields + `}`
	}
}

const goodStart = `{"type":"start","instruction":"Build the thing.","workdir":"/app","budget_usd":100}`

func TestTheStartLineGivesTheBriefTheWorkdirAndTheBudget(t *testing.T) {
	c, _ := newConn(t, goodStart)
	s, err := c.ReadStart()
	if err != nil || s.Instruction != "Build the thing." || s.Workdir != "/app" || s.BudgetUSD != 100 {
		t.Errorf("ReadStart() = %+v, %v", s, err)
	}
}

func TestAStartLineThatIsNotAStartIsRefused(t *testing.T) {
	for name, line := range map[string]string{
		"not JSON":           `hello`,
		"another type":       `{"type":"go","instruction":"x","workdir":"/app","budget_usd":1}`,
		"no instruction":     `{"type":"start","instruction":"","workdir":"/app","budget_usd":1}`,
		"a relative workdir": `{"type":"start","instruction":"x","workdir":"app","budget_usd":1}`,
		"no budget":          `{"type":"start","instruction":"x","workdir":"/app","budget_usd":0}`,
		"a negative budget":  `{"type":"start","instruction":"x","workdir":"/app","budget_usd":-5}`,
		"an unknown field":   `{"type":"start","instruction":"x","workdir":"/app","budget_usd":1,"root":true}`,
	} {
		c, _ := newConn(t, line)
		if s, err := c.ReadStart(); err == nil {
			t.Errorf("%s: ReadStart() = %+v, nil; want a refusal", name, s)
		}
	}
	c, a := newConn(t, "")
	a.toYgg.Close()
	if _, err := c.ReadStart(); err == nil {
		t.Error("an adapter that sent nothing: ReadStart() = nil")
	}
}

func TestEachRequestCarriesTheNextIDAndTheReplyMustBeForIt(t *testing.T) {
	c, a := newConn(t, "")
	go func() {
		a.answer(ok(""))
		a.answer(func(req map[string]any) string { return `{"id":99,"ok":true}` }) // an answer to another request
	}()
	if err := c.Spend(0.5); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if err := c.Spend(0.7); err == nil || !strings.Contains(err.Error(), "id") {
		t.Errorf("a reply for another id: Spend() = %v; want an error that names the id", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.requests) != 2 || a.requests[0]["id"] != float64(1) || a.requests[1]["id"] != float64(2) || a.requests[0]["op"] != "spend" || a.requests[0]["usd"] != 0.5 {
		t.Errorf("requests = %+v", a.requests)
	}
}

func TestARefusedRequestIsARemoteErrorWithTheAdaptersWords(t *testing.T) {
	c, a := newConn(t, "")
	go a.answer(func(req map[string]any) string {
		id, _ := json.Marshal(req["id"])
		return `{"id":` + string(id) + `,"ok":false,"error":"refused: the value of OPENROUTER_API_KEY in key.txt"}`
	})
	err := c.UploadDir("/tmp/product", "/app")
	var re *RemoteError
	if !errors.As(err, &re) || re.Op != "upload_dir" || !strings.Contains(re.Msg, "OPENROUTER_API_KEY") {
		t.Errorf("UploadDir() = %v; want a *RemoteError that carries the adapter's words", err)
	}
}

func TestUploadSelfcheckAndDoneSendTheFieldsTheAdapterReads(t *testing.T) {
	c, a := newConn(t, "")
	go func() {
		a.answer(ok(""))
		a.answer(ok(`,"passed":true,"detail":"PASS port 3000 answered"`))
		a.answer(ok(`,"passed":false,"detail":"FAIL start.sh is missing"`))
	}()
	if err := c.UploadDir("/tmp/product", "/app"); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Selfcheck(); err != nil || !got.Passed || got.Detail != "PASS port 3000 answered" {
		t.Errorf("Selfcheck() = %+v, %v", got, err)
	}
	if got, err := c.Done(); err != nil || got.Passed || !strings.HasPrefix(got.Detail, "FAIL") {
		t.Errorf("Done() = %+v, %v; a failed self-check still ends the run and is reported, not hidden", got, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.requests[0]; r["op"] != "upload_dir" || r["source"] != "/tmp/product" || r["target"] != "/app" {
		t.Errorf("upload_dir request = %+v", r)
	}
	if a.requests[1]["op"] != "selfcheck" || a.requests[2]["op"] != "done" {
		t.Errorf("requests = %+v", a.requests)
	}
}

func TestAnAdapterThatHangsUpMidRequestIsAnErrorNotAHang(t *testing.T) {
	c, a := newConn(t, "")
	go func() {
		a.fromYgg.ReadBytes('\n') //nolint:errcheck // read the request, then hang up without a reply
		a.toYgg.Close()
	}()
	if err := c.Spend(1); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("Spend() = %v; want an error that says the bridge closed", err)
	}
}

// The reporter of spend runs beside the main flow; two requests must never interleave on the line.
func TestRequestsFromTwoGoroutinesAreSentOneAtATime(t *testing.T) {
	c, a := newConn(t, "")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Spend(1); err != nil {
				t.Errorf("Spend() = %v", err)
			}
		}()
		a.answer(ok(""))
	}
	wg.Wait()
	seen := map[float64]bool{}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.requests {
		seen[r["id"].(float64)] = true
	}
	if len(seen) != 20 {
		t.Errorf("%d different ids in 20 requests", len(seen))
	}
}

func TestAReplyLineOverTheBoundIsRefused(t *testing.T) {
	c, a := newConn(t, "")
	go a.answer(func(req map[string]any) string {
		return `{"id":1,"ok":true,"detail":"` + strings.Repeat("x", maxLine+10) + `"}`
	})
	if err := c.Spend(1); err == nil {
		t.Error("Spend() accepted a reply line over the bound")
	}
}
