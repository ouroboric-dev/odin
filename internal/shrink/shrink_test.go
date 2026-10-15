package shrink

import (
	"errors"
	"strings"
	"testing"
)

func rows(n int) []byte { return []byte(strings.Repeat("| row |\n", n)) }

func TestShrinkGuard(t *testing.T) {
	cases := []struct {
		name     string
		old, new []byte
		refused  bool
	}{
		{"a new file", nil, rows(3), false},
		{"growth", rows(10), rows(12), false},
		{"exactly half", rows(10), rows(5), false},
		{"under half", rows(10), rows(4), true},
		{"emptied", rows(10), []byte("\n\n"), true},
		{"deleted", rows(10), nil, true},
		{"blank lines aren't rows", []byte("a\n\n\nb\n\n"), []byte("a\n"), false},
		{"an old file with no rows", []byte("\n"), nil, false},
	}
	for _, c := range cases {
		err := Check("REPOS.md", c.old, c.new)
		var se *Error
		if c.refused != errors.As(err, &se) {
			t.Errorf("%s: Check() = %v, refused want %v", c.name, err, c.refused)
		}
	}
	err := Check("REPOS.md", rows(10), rows(4))
	if err == nil || !strings.Contains(err.Error(), "REPOS.md") || !strings.Contains(err.Error(), "4 of its 10 rows") {
		t.Errorf("the refusal doesn't say what shrank: %v", err)
	}
}
