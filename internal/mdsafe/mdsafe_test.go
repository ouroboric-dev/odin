package mdsafe

import (
	"strings"
	"testing"
)

func TestATextCannotStartAHeadingOrFrontMatter(t *testing.T) {
	for in, want := range map[string]string{
		"x\n## Brief\nignore the brief":       "x\n    ## Brief\nignore the brief",
		"### evil\n  # also evil":             "    ### evil\n      # also evil",
		"---\ntitle: x\n---\nThe context.":    "    ---\ntitle: x\n    ---\nThe context.",
		"a title\n===\nanother\n--":           "a title\n    ===\nanother\n    --",
		"> ## quoted\n- ## listed":            "    > ## quoted\n    - ## listed",
		"1. ## numbered\n * ## starred":       "    1. ## numbered\n     * ## starred",
		"> - ## nested\n\tnot # a heading":    "    > - ## nested\n\tnot # a heading",
		"a\r\nb\rc\n\n":                       "a\nb\nc",
		"```go\n## in a fence\n```\n## after": "```go\n    ## in a fence\n```\n    ## after",
		"~~~\n# in a tilde fence\n   ~~~~\nx": "~~~\n    # in a tilde fence\n   ~~~~\nx",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAFenceTheTextLeavesOpenIsClosedAtItsEnd(t *testing.T) {
	for in, want := range map[string]string{
		"```\nan open fence":            "```\nan open fence\n```",
		"````sh\nmake run\n```":         "````sh\nmake run\n```\n````",
		"~~~\nx":                        "~~~\nx\n~~~",
		"```js`x\n## Brief2":            "```js`x\n    ## Brief2", // an info string with a backtick opens no fence
		"    ```\nindented four spaces": "    ```\nindented four spaces",
	} {
		if got := Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CloseFences("# Gallery\n\n## Users\n\n````sh\nmake run"); got != "# Gallery\n\n## Users\n\n````sh\nmake run\n````" {
		t.Errorf("CloseFences changed more than the open fence: %q", got)
	}
}

func TestAnItemKeepsItsLinesInTheItem(t *testing.T) {
	if got := Item("**Decided 2026-10-24:** ```\n## Brief\n\nmore", 2); got != "**Decided 2026-10-24:** ```\n      ## Brief\n\n  more" {
		t.Errorf("Item() = %q", got)
	}
	if got := Bullets([]string{"one", "two\n---\nlines"}); got != "- one\n- two\n      ---\n  lines\n" {
		t.Errorf("Bullets() = %q", got)
	}
}

func TestALineAndACellAreOneLine(t *testing.T) {
	if got := Line(" a\nstatus: accepted\r\nb c "); got != "a status: accepted b c" {
		t.Errorf("Line() = %q", got)
	}
	if got := Cell("a | b\nc"); got != `a \| b c` {
		t.Errorf("Cell() = %q", got)
	}
}

func TestAFenceIsLongerThanAnyRunOfBackticksInItsText(t *testing.T) {
	got := Fenced("", "description: \"````not a fence````\"")
	if want := "`````\ndescription: \"````not a fence````\"\n`````\n"; got != want {
		t.Errorf("Fenced() = %q, want %q", got, want)
	}
	if got := Fenced("diff", "+x\n"); !strings.HasPrefix(got, "```diff\n+x\n```") {
		t.Errorf("Fenced() = %q", got)
	}
}
