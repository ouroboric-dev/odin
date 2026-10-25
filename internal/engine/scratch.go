package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ouroboric-dev/gungnir"
	"github.com/ouroboric-dev/gungnir/schema"
	"github.com/ouroboric-dev/odin/internal/run"
)

// runScratch runs a scratch agent node (spec §3.3 rule 1): a planner gets a workspace of its own, not an
// umbrella and not a leaf. The workspace holds an empty out/ folder, for the agent's drafts, and the files of its
// inputs over the bound: what it reads is in its prompt, which promptFor builds from its declared inputs. What it
// produces is its typed output; Odin publishes that and its view into the project later (a publish node), after
// the approval point, and nothing of out/. A workspace is made again for every attempt and every retry, so a
// half-written one never reaches the next.
func runScratch(ctx context.Context, r *run.Run, n schema.Node) error {
	s := r.State
	dir := r.ScratchDir(n.ID)
	if err := seedScratch(r, n); err != nil {
		return &nodeFailure{err}
	}
	text, err := promptFor(r, n, nil, nil)
	if err != nil {
		return err
	}
	// A try that failed starts again from a fresh workspace, with the same input files.
	reseed := func() error {
		if err := seedScratch(r, n); err != nil {
			return err
		}
		_, err := promptFor(r, n, nil, nil)
		return err
	}
	// The session hands back one document of the node's contract, through the output channel: an output that
	// does not validate is asked for once more, with the errors, and a second one fails the node. A gate reads a
	// design-conformance verdict from there (spec §4.6).
	sp := sessionSpec{key: n.ID, role: n.Role, text: text, output: schema.Contract(n.Output), maxTurns: n.MaxTurns, maxTokens: n.MaxTokens,
		timeout: time.Duration(n.Timeout), scratch: dir, reseed: reseed}
	res, ev, err := sessionForOutput(ctx, r, sp)
	if err != nil {
		return nodeError(err)
	}
	ev.Name, ev.Result = gungnir.EventNodeFinished, res.Summary
	if err := r.Log(ev); err != nil {
		return err
	}
	s.Summaries[n.ID] = res.Summary // its rejection and refusal go once its output passes the owner check (advance)
	return nil
}

// seedScratch makes the workspace of node n anew: an empty out/. Nothing of the project is copied in, not even
// the output of a node it reviews: a node's inputs are typed, and reach its prompt (change typed-boundaries); the
// ones over the bound are written by promptFor, after this. Everything in Odin's own folder is replaced; nothing
// outside it is touched. The folder itself stays, emptied in place: the scratch sandbox mounts it and stays up
// across a retry and a refusal sent back, and a folder made again would not be the one its container sees
// (review I-3).
func seedScratch(r *run.Run, n schema.Node) error {
	dir := r.ScratchDir(n.ID)
	if err := emptyDir(dir); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(dir, "out"), 0o755)
}

// emptyDir removes everything in dir and keeps dir, so a mount of it stays valid. A dir that is not
// there is made; anything at its path that is not a folder, such as a symlink, is removed first.
func emptyDir(dir string) error {
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case !st.IsDir():
		if err := os.Remove(dir); err != nil {
			return err
		}
		return os.MkdirAll(dir, 0o755)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// The retrospective's copy of the transcripts stops at whichever bound it meets first.
const (
	maxRetroTranscripts     = 2000
	maxRetroTranscriptBytes = 64 << 20
)

// seedRecordFiles gives the workspace of a scratch node that reads the record what else the retrospective reads
// (change typed-boundaries §5): ledger.jsonl, a copy of the run's ledger whose grade lines keep only the totals, and
// transcripts/, a copy of the run's transcripts up to the bound. Every transcript was written by a session before
// the grade, and nothing of the grade's own folder is copied. It returns what the prompt says of them.
func seedRecordFiles(r *run.Run, n schema.Node) (string, error) {
	ws := r.ScratchDir(n.ID)
	var grades []string
	for _, m := range r.Setup.Graph.All() {
		if m.Kind == schema.KindGrade {
			grades = append(grades, m.ID)
		}
	}
	if err := copyLedger(filepath.Join(r.Dir, "ledger.jsonl"), filepath.Join(ws, "ledger.jsonl"), grades); err != nil {
		return "", fmt.Errorf("copying the ledger for %s: %w", n.ID, err)
	}
	c, err := copyTranscripts(filepath.Join(r.Dir, "transcripts"), filepath.Join(ws, "transcripts"), maxRetroTranscripts, maxRetroTranscriptBytes)
	if err != nil {
		return "", fmt.Errorf("copying the transcripts for %s: %w", n.ID, err)
	}
	return retroNote(c), nil
}

// transcriptCopy is how a copy of the transcripts went: the files it copied, the ones it skipped because each was
// larger than what was left of the bytes, the ones it left out once the file count was reached, and the files the run
// holds.
type transcriptCopy struct{ copied, skipped, leftOut, total int }

// retroNote is what the workspace note of the retrospective adds: where the ledger and the transcripts are, and,
// when the copy met its bound, how much of the transcripts it holds.
func retroNote(c transcriptCopy) string {
	note := "- ledger.jsonl is a copy of the run's ledger, one event a line, up to this session: ledger:SEQ names a line by its seq. " +
		"The lines of the grade keep its totals, and its own words are left out; the operator's judgements are left out.\n"
	switch {
	case c.total == 0:
		note += "- transcripts/ is empty: the run kept no transcript.\n"
	case c.copied < c.total:
		note += fmt.Sprintf("- transcripts/ holds %d of the run's %d transcript files, in name order: the copy holds at most %d files and %d MiB. "+
			"%d skipped, each larger than what was left of the bytes, and %d left out after the last file the count allowed; "+
			"they are not in this workspace. transcript:NAME names one.\n",
			c.copied, c.total, maxRetroTranscripts, maxRetroTranscriptBytes>>20, c.skipped, c.leftOut)
	default:
		note += fmt.Sprintf("- transcripts/ holds all %d of the run's transcript files, as its sessions wrote them: transcript:NAME names one.\n", c.total)
	}
	return note
}

// copyLedger copies the ledger at src to dst, a new file. The lines of the grade nodes (grades) lose their result
// and note, the grade's own words, such as a refusal that names the suite's scenarios; the grade_* events carry
// totals only and are copied as they are. The operator's judged lines, which hold a score, are left out. The copy is
// for reading: its hash chain does not hold.
func copyLedger(src, dst string, grades []string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	w := bufio.NewWriter(out)
	for sc.Scan() {
		line, err := ledgerLine(sc.Bytes(), grades)
		if err != nil {
			out.Close()
			return err
		}
		if line == nil {
			continue
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		out.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ledgerLine is one line of the ledger as the retrospective's copy holds it; nil leaves it out.
func ledgerLine(line []byte, grades []string) ([]byte, error) {
	var head struct {
		Node string            `json:"ygg.node.id"`
		Name gungnir.EventName `json:"event"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return nil, err
	}
	switch {
	case head.Name == gungnir.EventJudged:
		return nil, nil
	case !slices.Contains(grades, head.Node):
		return line, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return nil, err
	}
	delete(fields, "ygg.result")
	delete(fields, "ygg.note")
	return json.Marshal(fields)
}

// copyTranscripts copies the regular files of the folder src into dst, made new, in name order, until maxFiles are
// copied: a file larger than what is left of maxBytes is skipped, and the copy goes on with the next. A src that is a
// link, or not a folder, is refused; a link in it is never followed nor copied, and a file that grows while it is read
// is cut at the size it had. A src that is not there gives an empty dst.
func copyTranscripts(src, dst string, maxFiles int, maxBytes int64) (transcriptCopy, error) {
	var c transcriptCopy
	if err := os.Mkdir(dst, 0o755); err != nil {
		return c, err
	}
	st, err := os.Lstat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return c, nil
	case err != nil:
		return c, err
	case !st.IsDir():
		return c, fmt.Errorf("%s is %s, not a folder of the run's own", src, describeMode(st.Mode()))
	}
	dir, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return c, fmt.Errorf("%s is not a folder of the run's own: %w", src, err)
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return c, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	left := maxBytes
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		c.total++
		if c.copied >= maxFiles {
			c.leftOut++
			continue
		}
		info, err := e.Info()
		if err != nil {
			return c, err
		}
		if info.Size() > left {
			c.skipped++
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), info.Size()); err != nil {
			return c, err
		}
		c.copied++
		left -= info.Size()
	}
	return c, nil
}

// describeMode names what a path is that should have been a folder.
func describeMode(m fs.FileMode) string {
	if m&fs.ModeSymlink != 0 {
		return "a link"
	}
	return "a " + m.Type().String() + " file"
}

// copyFile copies at most n bytes of the regular file src to dst, a new file. Neither is opened through a link, and
// a src that is no longer a regular file is an error.
func copyFile(src, dst string, n int64) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if st, err := in.Stat(); err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(in, n)); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
