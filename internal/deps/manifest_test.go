package deps

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, text string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

const goMod = `module example.invalid/app

go 1.26

require example.invalid/single v1.4.2

require (
	golang.org/x/text v0.41.0 // indirect
	example.invalid/dep v2.0.1
)

replace example.invalid/gone => ../gone
`

func TestFindReadsGoModInEveryFormOfRequire(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", goMod)
	for pkg, want := range map[string]string{"example.invalid/single": "1.4.2", "golang.org/x/text": "0.41.0", "example.invalid/dep": "2.0.1"} {
		m, err := Find(dir, pkg)
		if err != nil || m.Kind != Go || m.Current.String() != want || m.Path != filepath.Join(dir, "go.mod") {
			t.Errorf("Find(%s) = %+v, %v; want go %s", pkg, m, err, want)
		}
	}
	if cmd := mustFind(t, dir, "golang.org/x/text").Command("golang.org/x/text", Version{Major: 0, Minor: 42, Patch: 0}); strings.Join(cmd, " ") != "go get golang.org/x/text@v0.42.0" {
		t.Errorf("command %v", cmd)
	}
}

func mustFind(t *testing.T, dir, pkg string) Manifest {
	t.Helper()
	m, err := Find(dir, pkg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFindReadsPackageJSONAndKeepsTheTilde(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"dependencies":{"vite":"7.1.2","react":"~19.1.0"},"devDependencies":{"vitest":"3.2.4"}}`)
	to := Version{Major: 7, Minor: 1, Patch: 9}
	if m := mustFind(t, dir, "vite"); m.Kind != NPM || m.Tilde || strings.Join(m.Command("vite", to), " ") != "npm install --ignore-scripts --save-exact vite@7.1.9" {
		t.Errorf("vite: %+v %v", m, m.Command("vite", to))
	}
	if m := mustFind(t, dir, "react"); !m.Tilde || m.Current.String() != "19.1.0" || strings.Join(m.Command("react", to), " ") != "npm install --ignore-scripts --save-prefix=~ react@7.1.9" {
		t.Errorf("react: %+v %v", m, m.Command("react", to))
	}
	if m := mustFind(t, dir, "vitest"); m.Current.String() != "3.2.4" {
		t.Errorf("vitest: %+v", m)
	}
}

func TestFindRefusesWhatItWouldHaveToGuess(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module m\n\ngo 1.26\n\nrequire (\n\texample.invalid/pseudo v0.0.0-20261017195515-cb1dc0ee9cc6\n\texample.invalid/both v1.0.0\n)\n")
	write(t, dir, "package.json", `{"dependencies":{"caret":"^1.2.3","tag":"latest","url":"git+https://x/y.git","example.invalid/both":"1.0.0"}}`)
	for pkg, want := range map[string]string{
		"missing":                "requires no package",
		"example.invalid/pseudo": "not a tagged release",
		"caret":                  "pins x.y.z or ~x.y.z",
		"tag":                    "pins x.y.z or ~x.y.z",
		"url":                    "pins x.y.z or ~x.y.z",
		"example.invalid/both":   "both go.mod and package.json",
	} {
		if _, err := Find(dir, pkg); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Find(%s) = %v, want %q", pkg, err, want)
		}
	}
}
