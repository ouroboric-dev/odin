// Package task reads a benchmark task for grading (spec §11, change benchmark-split): the public half
// from a checkout of mimir. It checks everything that can be
// checked before a container is built, and refuses with one of five reasons when a check fails. It
// reads files and starts nothing.
package task

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ouroboric-dev/gungnir"
)

// Task is a task of mimir. Every path is absolute when the folder given to Load is.
type Task struct {
	ID          string
	Version     string // task.version: the version of the brief, such as 4.0.0
	Suite       string // metadata.suite: the hidden suite that scores this version, such as v1
	BriefSHA256 string // metadata.brief_sha256, which Load has checked against instruction.md
	Dir         string // <tasks_dir>/<id>
	Environment string // <tasks_dir>/<id>/environment: the grader's image is built from it
	Timeout     time.Duration
	Env         map[string]string  // verifier.env, passed to the grader's container
	Weights     map[string]float64 // the categories of the reward, from SCORE_WEIGHTS; they sum to 1
}

// Refusal is why grading was refused before anything was built. Reason is the word the ledger records.
type Refusal struct {
	Reason gungnir.GradeReason
	Detail string
}

func (r *Refusal) Error() string { return fmt.Sprintf("grading refused (%s): %s", r.Reason, r.Detail) }

func refuse(reason gungnir.GradeReason, format string, a ...any) error {
	return &Refusal{Reason: reason, Detail: fmt.Sprintf(format, a...)}
}

// File size limits: a task's files are small text, and a larger one is not a task.
const (
	maxToml  = 64 << 10
	maxBrief = 1 << 20
)

// weightEpsilon is how far the weights may be from a sum of 1.
const weightEpsilon = 1e-9

var (
	taskID    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	suiteName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	category  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	envName   = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// taskFile is the part of task.toml that grading reads. The file holds more (Harbor's keys); they are
// not ours and not checked.
type taskFile struct {
	Artifacts []string `toml:"artifacts"`
	Task      struct {
		Name    string `toml:"name"`
		Version string `toml:"version"`
	} `toml:"task"`
	Metadata struct {
		BriefSHA256 string `toml:"brief_sha256"`
		Suite       string `toml:"suite"`
	} `toml:"metadata"`
	Verifier struct {
		TimeoutSec      float64           `toml:"timeout_sec"`
		EnvironmentMode string            `toml:"environment_mode"`
		Env             map[string]string `toml:"env"`
	} `toml:"verifier"`
}

// Load reads task id from tasksDir, and checks it in this order: the task file (bad_task), the retired
// mark (retired) and the brief's hash (brief_changed). Any failure is a *Refusal.
func Load(tasksDir, id string) (*Task, error) {
	if !taskID.MatchString(id) {
		return nil, refuse(gungnir.GradeBadTask, "%q is not a task id (lower case words joined by dashes)", id)
	}
	t := &Task{ID: id, Dir: filepath.Join(tasksDir, id)}
	t.Environment = filepath.Join(t.Dir, "environment")
	raw, err := readRegular(filepath.Join(t.Dir, "task.toml"), maxToml)
	if err != nil {
		return nil, refuse(gungnir.GradeBadTask, "task.toml: %v", err)
	}
	var f taskFile
	if _, err := toml.NewDecoder(bytes.NewReader(raw)).Decode(&f); err != nil {
		return nil, refuse(gungnir.GradeBadTask, "task.toml does not parse: %v", err)
	}
	if err := t.fromFile(f); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(t.Dir, "retired", t.Suite)); err == nil {
		return nil, refuse(gungnir.GradeRetired, "the hidden suite %s of task %s is retired; a new task version names a new suite", t.Suite, id)
	}
	brief, err := readRegular(filepath.Join(t.Dir, "instruction.md"), maxBrief)
	if err != nil {
		return nil, refuse(gungnir.GradeBadTask, "instruction.md: %v", err)
	}
	if sum := sha256.Sum256(brief); hex.EncodeToString(sum[:]) != t.BriefSHA256 {
		return nil, refuse(gungnir.GradeBriefChanged, "instruction.md hashes to %s, task.toml says %s; any change to a brief is a new task version", hex.EncodeToString(sum[:]), t.BriefSHA256)
	}
	return t, nil
}

// fromFile checks the fields grading uses, and sets them.
func (t *Task) fromFile(f taskFile) error {
	bad := func(format string, a ...any) error { return refuse(gungnir.GradeBadTask, format, a...) }
	if !slices.Equal(f.Artifacts, []string{"/app"}) {
		return bad("artifacts is %q, it must be [\"/app\"]: only /app is carried to the grader", f.Artifacts)
	}
	if name := f.Task.Name; name[strings.LastIndex(name, "/")+1:] != t.ID {
		return bad("task.name %q does not end in the task's folder name %s", name, t.ID)
	}
	if !gungnir.ValidVersion(f.Task.Version) {
		return bad("task.version %q is not of the form 1.2.3", f.Task.Version)
	}
	if !sha256Hex.MatchString(f.Metadata.BriefSHA256) {
		return bad("metadata.brief_sha256 %q is not a SHA-256 in hex", f.Metadata.BriefSHA256)
	}
	if !suiteName.MatchString(f.Metadata.Suite) {
		return bad("metadata.suite %q is not a suite version such as v1", f.Metadata.Suite)
	}
	if f.Verifier.EnvironmentMode != "separate" {
		return bad("verifier.environment_mode is %q, it must be \"separate\": the grader holds only the collected /app", f.Verifier.EnvironmentMode)
	}
	if f.Verifier.TimeoutSec <= 0 || f.Verifier.TimeoutSec > 24*3600 {
		return bad("verifier.timeout_sec %v must be above 0 and at most a day", f.Verifier.TimeoutSec)
	}
	for name := range f.Verifier.Env {
		if !envName.MatchString(name) {
			return bad("verifier.env: %q is not a variable name in capitals", name)
		}
	}
	w, err := parseWeights(f.Verifier.Env["SCORE_WEIGHTS"])
	if err != nil {
		return bad("verifier.env SCORE_WEIGHTS: %v", err)
	}
	t.Version, t.BriefSHA256, t.Suite = f.Task.Version, f.Metadata.BriefSHA256, f.Metadata.Suite
	t.Timeout, t.Env, t.Weights = time.Duration(f.Verifier.TimeoutSec*float64(time.Second)), f.Verifier.Env, w
	return nil
}

// parseWeights reads "cat=w,cat=w": categories once each, weights from 0 to 1, summing to 1.
func parseWeights(s string) (map[string]float64, error) {
	if s == "" {
		return nil, errors.New("it is not set")
	}
	w := map[string]float64{}
	var sum float64
	for _, part := range strings.Split(s, ",") {
		name, num, ok := strings.Cut(part, "=")
		v, err := strconv.ParseFloat(num, 64)
		switch {
		case !ok || !category.MatchString(name):
			return nil, fmt.Errorf("%q is not category=weight", part)
		case err != nil || math.IsNaN(v) || v < 0 || v > 1:
			return nil, fmt.Errorf("the weight of %s must be a number from 0 to 1", name)
		}
		if _, twice := w[name]; twice {
			return nil, fmt.Errorf("the category %s is named twice", name)
		}
		w[name] = v
		sum += v
	}
	if math.Abs(sum-1) > weightEpsilon {
		return nil, fmt.Errorf("the weights sum to %v, not 1", sum)
	}
	return w, nil
}

// readRegular reads a regular file of at most max bytes, and nothing that is a link: a task's files are
// read from a checkout, and a link could lead to a file outside it.
func readRegular(path string, max int64) ([]byte, error) {
	st, err := os.Lstat(path)
	switch {
	case err != nil:
		return nil, bare(err)
	case !st.Mode().IsRegular():
		return nil, errors.New("it is not a regular file")
	case st.Size() > max:
		return nil, fmt.Errorf("it is over %d bytes", max)
	}
	b, err := os.ReadFile(path)
	return b, bare(err)
}

// bare is err without the path, which the message names already.
func bare(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
