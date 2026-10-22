// Package bridge is Yggdrasil's side of the Harbor bridge (bench/harbor/ygg_harbor/agent.py): JSON lines over the
// factory's standard input and output. The adapter sends one start line; the factory then sends requests
// ({"id","op",...}) and the adapter answers each ({"id","ok",...}). Standard output carries requests and nothing
// else: a line that is not a request makes the adapter fail the run.
package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sync"

	"github.com/ouroboric-dev/odin/internal/metrics"
)

// maxLine bounds one line from the adapter. A reply to the requests used here is a few hundred bytes.
const maxLine = 1 << 20

// Start is the adapter's first line: the brief, where the product goes in the task environment, and the budget
// Harbor does not enforce.
type Start struct {
	Type        string  `json:"type"`
	Instruction string  `json:"instruction"`
	Workdir     string  `json:"workdir"`
	BudgetUSD   float64 `json:"budget_usd"`
}

// Check is the result of the adapter's self-check of the product in the task environment.
type Check struct {
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// RemoteError is a request the adapter refused or could not carry out; Msg is its own words, raw. Error prints them
// cleaned: the adapter's text may carry newlines or escape sequences.
type RemoteError struct{ Op, Msg string }

func (e *RemoteError) Error() string {
	return "the adapter did not carry out " + e.Op + ": " + metrics.Clean(e.Msg)
}

// Conn is the factory's end of the bridge. Its methods may be called from several goroutines: each request is
// sent and answered before the next one goes out. After a protocol error the lines are out of step, so every later
// call returns that error without writing or reading.
type Conn struct {
	in    io.Reader
	out   io.Writer
	mu    sync.Mutex // one request at a time
	next  int
	lines chan []byte   // the lines the reader goroutine took from the adapter, one at a time
	done  chan struct{} // closed when the bridge is dead: a protocol error, the end of the input or Close
	once  sync.Once

	errMu   sync.Mutex
	err     error // the first thing that killed the bridge
	readErr error // why the reader goroutine stopped
}

// NewConn makes the factory's end from the adapter's output (the factory's standard input) and the factory's
// standard output. A goroutine reads the input for as long as the bridge lives, so the end of the input is seen
// while no request is waiting: Dead reports it.
func NewConn(in io.Reader, out io.Writer) *Conn {
	c := &Conn{in: in, out: out, lines: make(chan []byte), done: make(chan struct{})}
	go c.read(bufio.NewReader(in))
	return c
}

// Dead is closed when the bridge is dead: after a protocol error, when the adapter's output ends, or after Close.
func (c *Conn) Dead() <-chan struct{} { return c.done }

// Err is why the bridge is dead, or nil while it lives.
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err != nil {
		return c.err
	}
	return c.readErr
}

// Close ends the bridge: a call that waits for the adapter returns, and later calls fail. It closes the input and
// the output when they can be closed, for a read or a write on a pipe that no one serves does not return otherwise.
func (c *Conn) Close() {
	c.errMu.Lock()
	if c.err == nil {
		c.err = errors.New("the bridge is closed")
	}
	c.errMu.Unlock()
	c.once.Do(func() { close(c.done) })
	if cl, ok := c.in.(io.Closer); ok {
		cl.Close()
	}
	if cl, ok := c.out.(io.Closer); ok {
		cl.Close()
	}
}

func (c *Conn) read(r *bufio.Reader) {
	for {
		line, err := readLine(r)
		if err != nil {
			c.errMu.Lock()
			c.readErr = err
			c.errMu.Unlock()
			c.once.Do(func() { close(c.done) })
			return
		}
		select {
		case c.lines <- line:
		case <-c.done:
			return
		}
	}
}

// ReadStart reads and checks the start line.
func (c *Conn) ReadStart() (Start, error) {
	line, err := c.readLine()
	if err != nil {
		return Start{}, fmt.Errorf("no start line from the adapter: %w", err)
	}
	var s Start
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Start{}, fmt.Errorf("the first line from the adapter is not a start line: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Start{}, errors.New("the start line has data after the JSON object")
	}
	switch {
	case s.Type != "start":
		return Start{}, fmt.Errorf("the first line from the adapter has the type %q, want start", s.Type)
	case s.Instruction == "":
		return Start{}, errors.New("the start line holds no instruction")
	case !path.IsAbs(s.Workdir):
		return Start{}, fmt.Errorf("the start line's workdir %q is not an absolute path", s.Workdir)
	case s.BudgetUSD <= 0:
		return Start{}, fmt.Errorf("the start line's budget_usd is %v, want more than 0", s.BudgetUSD)
	}
	return s, nil
}

// UploadDir asks the adapter to copy the host folder source into target of the task environment.
func (c *Conn) UploadDir(source, target string) error {
	_, err := c.call("upload_dir", map[string]any{"source": source, "target": target})
	return err
}

// Selfcheck asks the adapter to run its self-check on the product now.
func (c *Conn) Selfcheck() (Check, error) { return c.check("selfcheck") }

// Spend reports the run's total cost so far. The adapter stops the factory when it passes the budget.
func (c *Conn) Spend(usd float64) error {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return fmt.Errorf("the spend %v is not a finite amount of 0 or more", usd)
	}
	_, err := c.call("spend", map[string]any{"usd": usd})
	return err
}

// Done ends the run. The adapter runs its self-check once more and answers with it; the factory exits after.
func (c *Conn) Done() (Check, error) { return c.check("done") }

func (c *Conn) check(op string) (Check, error) {
	reply, err := c.call(op, nil)
	if err != nil {
		return Check{}, err
	}
	var ch Check
	if err := json.Unmarshal(reply, &ch); err != nil {
		return Check{}, fmt.Errorf("the reply to %s is not a check: %w", op, err)
	}
	return ch, nil
}

// call sends one request and returns the adapter's reply line once it answers that request.
func (c *Conn) call(op string, fields map[string]any) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil, c.Err()
	default:
	}
	id := c.next + 1
	req := map[string]any{"id": id, "op": op}
	for k, v := range fields {
		req[k] = v
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	c.next = id
	if _, err := c.out.Write(append(b, '\n')); err != nil {
		return nil, c.fail(fmt.Errorf("the bridge is closed: %w", err))
	}
	line, err := c.readLine()
	if err != nil {
		return nil, c.fail(fmt.Errorf("the adapter closed the bridge before it answered %s: %w", op, err))
	}
	var reply struct {
		ID    *int   `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(line, &reply); err != nil {
		return nil, c.fail(fmt.Errorf("the reply to %s is not JSON: %w", op, err))
	}
	if reply.ID == nil || *reply.ID != c.next {
		got := "no id"
		if reply.ID != nil {
			got = fmt.Sprintf("the id %d", *reply.ID)
		}
		msg := fmt.Sprintf("the reply to %s has %s, want %d", op, got, c.next)
		if said := metrics.Clean(reply.Error); said != "" {
			msg += ": the adapter said " + said
		}
		return nil, c.fail(errors.New(msg))
	}
	if !reply.OK {
		return nil, &RemoteError{Op: op, Msg: reply.Error}
	}
	return line, nil
}

// fail records a protocol error, after which the bridge refuses every call, and returns it.
func (c *Conn) fail(err error) error {
	c.errMu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.errMu.Unlock()
	c.once.Do(func() { close(c.done) })
	return err
}

// readLine waits for the next line from the adapter, or for the bridge to die.
func (c *Conn) readLine() ([]byte, error) {
	select {
	case line := <-c.lines:
		return line, nil
	case <-c.done:
		return nil, c.Err()
	}
}

func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, fmt.Errorf("a line from the adapter is longer than %d bytes", maxLine)
		}
		if !isPrefix {
			return line, nil
		}
	}
}
