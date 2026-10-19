package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func goodIndex(t *testing.T) []byte {
	t.Helper()
	b, err := fixtureImpact(t).JSON()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheImpactIndexIsReadWithABoundAndNeverThroughALink(t *testing.T) {
	good := goodIndex(t)
	t.Run("oversize", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".ygg"), 0o755); err != nil {
			t.Fatal(err)
		}
		big := append(bytes.Clone(good), bytes.Repeat([]byte(" "), maxImpactBytes)...)
		if err := os.WriteFile(filepath.Join(dir, ImpactFile), big, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadImpact(dir); err == nil || !strings.Contains(err.Error(), "bytes") {
			t.Errorf("an oversize index was read: %v", err)
		}
	})
	t.Run("symlinked file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".ygg"), 0o755); err != nil {
			t.Fatal(err)
		}
		real := filepath.Join(t.TempDir(), "real.json")
		if err := os.WriteFile(real, good, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, filepath.Join(dir, ImpactFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadImpact(dir); err == nil {
			t.Error("a symlinked index was read")
		}
	})
	t.Run("symlinked folder", func(t *testing.T) {
		dir := t.TempDir()
		other := t.TempDir()
		if err := os.WriteFile(filepath.Join(other, "impact.json"), good, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, filepath.Join(dir, ".ygg")); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadImpact(dir); err == nil {
			t.Error("an index under a symlinked .ygg was read")
		}
	})
}

func TestTheImpactIndexIsWrittenWholeAndNeverThroughALink(t *testing.T) {
	imp := fixtureImpact(t)
	t.Run("a failed write leaves the old index", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteImpact(dir, imp); err != nil {
			t.Fatal(err)
		}
		old, _ := os.ReadFile(filepath.Join(dir, ImpactFile))
		orig := renameFile
		renameFile = func(string, string) error { return errors.New("injected") }
		defer func() { renameFile = orig }()
		changed := *imp
		changed.Edges = 99
		if err := WriteImpact(dir, &changed); err == nil {
			t.Fatal("the failure was swallowed")
		}
		now, _ := os.ReadFile(filepath.Join(dir, ImpactFile))
		if !bytes.Equal(old, now) {
			t.Error("a failed write damaged the old index")
		}
		ents, _ := os.ReadDir(filepath.Join(dir, ".ygg"))
		if len(ents) != 1 {
			t.Errorf("a temp file was left: %v", ents)
		}
	})
	t.Run("symlinked destination", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".ygg"), 0o755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "victim")
		if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, ImpactFile)); err != nil {
			t.Fatal(err)
		}
		if err := WriteImpact(dir, imp); err == nil {
			t.Error("wrote over a symlink")
		}
		if b, _ := os.ReadFile(target); string(b) != "keep" {
			t.Errorf("the link target changed: %q", b)
		}
	})
	t.Run("symlinked folder", func(t *testing.T) {
		dir := t.TempDir()
		other := t.TempDir()
		if err := os.Symlink(other, filepath.Join(dir, ".ygg")); err != nil {
			t.Fatal(err)
		}
		if err := WriteImpact(dir, imp); err == nil {
			t.Error("wrote through a symlinked .ygg")
		}
		if ents, _ := os.ReadDir(other); len(ents) != 0 {
			t.Errorf("files appeared behind the link: %v", ents)
		}
	})
}

func TestTheImpactIndexParserIsAnExactSchema(t *testing.T) {
	good := string(goodIndex(t))
	if _, err := ParseImpact([]byte(good)); err != nil {
		t.Fatalf("the good index is refused: %v", err)
	}
	cases := map[string]func(string) string{
		"unknown field":    func(s string) string { return strings.Replace(s, `"bound"`, `"extra": 1, "bound"`, 1) },
		"other schema":     func(s string) string { return strings.Replace(s, "ygg.impact/v1", "ygg.impact/v2", 1) },
		"other bound":      func(s string) string { return strings.Replace(s, `"lower"`, `"upper"`, 1) },
		"trailing data":    func(s string) string { return s + "{}" },
		"trailing garbage": func(s string) string { return s + "x" },
		"edges count":      func(s string) string { return strings.Replace(s, `"edges": 24`, `"edges": 25`, 1) },
		"inferred count":   func(s string) string { return strings.Replace(s, `"inferred": 2,`, `"inferred": 1,`, 1) },
		"extracted tag":    func(s string) string { return strings.Replace(s, `"tag": "inferred"`, `"tag": "extracted"`, 1) },
		"both lists": func(s string) string {
			return strings.Replace(s, `"api": {
      "extracted": [
        "web",
        "worker"
      ],
      "inferred": []`, `"api": {
      "extracted": [
        "web",
        "worker"
      ],
      "inferred": ["web"]`, 1)
		},
		"null reach list": func(s string) string {
			return strings.Replace(s, `"api": {
      "extracted": [
        "web",
        "worker"
      ],
      "inferred": []`, `"api": {
      "extracted": [
        "web",
        "worker"
      ],
      "inferred": null`, 1)
		},
	}
	for _, k := range []string{"contracts", "reach", "inferred_edges"} {
		cases["null "+k] = func(s string) string { return withKey(t, s, k, json.RawMessage("null")) }
		cases["missing "+k] = func(s string) string { return withKey(t, s, k, nil) }
	}
	for name, mutate := range cases {
		m := mutate(good)
		if m == good {
			t.Errorf("%s: the mutation did not change the index", name)
			continue
		}
		if _, err := ParseImpact([]byte(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// withKey returns the index with a top-level key set to v, or removed when v is nil.
func withKey(t *testing.T, s, k string, v json.RawMessage) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		delete(m, k)
	} else {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
