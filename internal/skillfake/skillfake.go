// Package skillfake builds an in-memory skill library for tests of Odin.
package skillfake

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ouroboric-dev/gleipnir/skills"
	"github.com/ouroboric-dev/havamal"
)

// Libraries returns a pinned library with one vendored skill, name, whose text is body.
func Libraries(t *testing.T, name, body string) *skills.Libraries {
	t.Helper()
	text := "---\nname: " + name + "\ndescription: A skill for tests.\n---\n" + body + "\n"
	fsys := fstest.MapFS{
		"skills/.gitkeep":              {},
		"skills/" + name + "/SKILL.md": {Data: []byte(text)},
		"skills/" + name + "/LICENSE":  {Data: []byte("MIT\n")},
	}
	lock := &havamal.Lock{Skills: []havamal.Entry{{Name: name, Licence: "MIT",
		Upstream: &havamal.Upstream{Repo: "https://github.com/example/skills", Path: name, Commit: strings.Repeat("a", 40)},
		Files:    []havamal.File{{Path: "LICENSE", SHA256: havamal.Sum([]byte("MIT\n"))}, {Path: "SKILL.md", SHA256: havamal.Sum([]byte(text))}}}}}
	b, err := lock.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys["skills.lock"] = &fstest.MapFile{Data: b}
	lib, err := havamal.Open(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return &skills.Libraries{Pinned: lib}
}
