// Package ledger writes and verifies a run's hash-chained ledger (spec §10.1). Odin is its only
// writer. Each line holds the SHA-256 of the line before it, so a changed line breaks the chain at
// the next one; run.json keeps an anchor (the last line's number and hash), so lines cut from the
// end are caught too.
package ledger

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
)

// Genesis is the prev hash of a ledger's first line.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Anchor is a ledger's last line number and that line's hash. Seq 0 means an empty ledger.
type Anchor struct {
	Seq  int64  `json:"seq"`
	Hash string `json:"hash"`
}

// Ledger appends events to one ledger.jsonl.
type Ledger struct {
	f    *os.File
	head Anchor
}

// Open opens path for appending, creating it if it doesn't exist. It refuses a ledger whose chain
// is broken, or that doesn't reach the anchor run.json recorded.
func Open(path string, want Anchor) (*Ledger, error) {
	head, err := Verify(path, want)
	if err != nil && !(errors.Is(err, os.ErrNotExist) && want.Seq == 0) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return &Ledger{f: f, head: head}, nil
}

// Head returns the anchor of the last line written.
func (l *Ledger) Head() Anchor { return l.head }

// Append sets e's schema, seq, prev and (if unset) time, checks e against the ledger schema, and
// writes it as one line, synced to disk before Append returns.
func (l *Ledger) Append(e gungnir.Event) error {
	e.Schema = gungnir.LedgerSchema
	e.Seq = l.head.Seq + 1
	e.Prev = l.head.Hash
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := schema.ValidateEvent(line); err != nil {
		return err
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.head = Anchor{Seq: e.Seq, Hash: hash(line)}
	return nil
}

// Close closes the file.
func (l *Ledger) Close() error { return l.f.Close() }

// Verify checks every line of path: the schema, the sequence, and the link to the line before. It
// also checks that the ledger reaches want and holds want's hash at want's line. It returns the
// ledger's head, or an error that names the first bad line.
func Verify(path string, want Anchor) (Anchor, error) {
	head := Anchor{Hash: Genesis}
	f, err := os.Open(path)
	if err != nil {
		return head, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return head, err
		}
		n := head.Seq + 1
		if !bytes.HasSuffix(line, []byte("\n")) {
			return head, fmt.Errorf("%s:%d: the line is incomplete", path, n)
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		if err := schema.ValidateEvent(line); err != nil {
			return head, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		var e struct {
			Seq  int64  `json:"seq"`
			Prev string `json:"prev"`
		}
		if err := json.Unmarshal(line, &e); err != nil {
			return head, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		if e.Seq != n {
			return head, fmt.Errorf("%s:%d: seq is %d", path, n, e.Seq)
		}
		if e.Prev != head.Hash {
			return head, fmt.Errorf("%s:%d: prev doesn't match line %d, so line %d was changed", path, n, n-1, n-1)
		}
		head = Anchor{Seq: n, Hash: hash(line)}
		if n == want.Seq && head.Hash != want.Hash {
			return head, fmt.Errorf("%s:%d: the line differs from the one run.json recorded", path, n)
		}
	}
	if head.Seq < want.Seq {
		return head, fmt.Errorf("%s: has %d lines, but run.json recorded %d: lines were removed", path, head.Seq, want.Seq)
	}
	if head.Seq == 0 {
		head.Hash = Genesis
	}
	return head, nil
}

func hash(line []byte) string {
	s := sha256.Sum256(line)
	return hex.EncodeToString(s[:])
}
