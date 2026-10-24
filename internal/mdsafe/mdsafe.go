// Package mdsafe writes the text of a typed field into Markdown that Odin builds, a prompt or a view, so that the
// field reads as its text and nothing else (change typed-boundaries, review focus 4): no line of it can start a
// heading, front matter or a heading's underline of its own, and a code fence it leaves open cannot swallow what
// follows it. It is the one rule for every prompt and every view.
package mdsafe

import (
	"regexp"
	"strings"
)

// Text is a field's text, written so that no line of it can start a heading or front matter of its own: a line
// that starts, after any spaces and any blockquote or list markers, with # or ---, or that is all = or all -,
// which would underline the line before it as a heading, is indented by four spaces, which makes it text of the
// block it is in. A code fence the text opens and does not close is closed at its end, so what follows it is not
// read as code. Line breaks are \n.
func Text(s string) string {
	return followFences(s, true)
}

// CloseFences is s with a code fence it opens and does not close closed at its end, and nothing else changed: for
// an instruction, such as the brief, which passes as it is.
func CloseFences(s string) string {
	return followFences(s, false)
}

// Item is a field's text as the content of a list item (Text), with every line after the first indented by
// indent spaces, the item's content column, so the text stays in the item. The caller writes the item's marker,
// and puts any words of its own before the field in s, so that the first line is read as the item reads it.
func Item(s string, indent int) string {
	lines := strings.Split(Text(s), "\n")
	pad := strings.Repeat(" ", indent)
	for i, l := range lines[1:] {
		if l != "" {
			lines[i+1] = pad + l
		}
	}
	return strings.Join(lines, "\n")
}

// Bullets is a bullet list of texts, one item each (Item), ending in a newline.
func Bullets(items []string) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString("- " + Item(it, 2) + "\n")
	}
	return b.String()
}

// Line is a field's text as one line, its line breaks made spaces: for a heading, a field of a list item and
// the front of a line, where a line break would end it.
func Line(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", " ", " ", " ", " ").Replace(s)
	return strings.TrimSpace(s)
}

// Cell is a field's text as one cell of a table: one line, with its pipes escaped, so it stays in its cell.
func Cell(s string) string {
	return strings.ReplaceAll(Line(s), "|", `\|`)
}

// Fenced is text in a code block whose fence is longer than any run of backticks in text, so text cannot close
// it, ending in a newline.
func Fenced(lang, text string) string {
	run, longest := 0, 0
	for _, c := range text {
		if c == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return fence + lang + "\n" + text + fence + "\n"
}

// followFences follows the code fences of s as CommonMark reads them, closes one left open at its end, and, with
// guard, indents each line outside the fence lines that could start a block of its own (startsBlock).
func followFences(s string, guard bool) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	open := "" // the fence the text has opened, while it is open
	for i, line := range lines {
		switch {
		case open == "" && fenceOpener(line) != "":
			open = fenceOpener(line)
			continue
		case open != "" && closesFence(line, open):
			open = ""
			continue
		}
		if guard && startsBlock(strings.TrimLeft(containerFree(line), " \t")) {
			lines[i] = "    " + line
		}
	}
	if open != "" {
		lines = append(lines, open)
	}
	return strings.Join(lines, "\n")
}

// fenceOpener is the fence line opens, as CommonMark reads one: three or more backticks or tildes after at most
// three spaces, where a backtick fence's info string holds no backtick; "" when it opens none.
func fenceOpener(line string) string {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return ""
	}
	run := fenceRun(t)
	if run == "" || (run[0] == '`' && strings.Contains(t[len(run):], "`")) {
		return ""
	}
	return run
}

// closesFence reports whether line closes the fence open: a run of its character at least as long, after at most
// three spaces, with nothing after it.
func closesFence(line, open string) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	run := fenceRun(t)
	return run != "" && run[0] == open[0] && len(run) >= len(open) && strings.TrimSpace(t[len(run):]) == ""
}

// fenceRun is the run of three or more backticks or tildes t starts with, or "".
func fenceRun(t string) string {
	if t == "" || (t[0] != '`' && t[0] != '~') {
		return ""
	}
	n := len(t) - len(strings.TrimLeft(t, t[:1]))
	if n < 3 {
		return ""
	}
	return t[:n]
}

// containerMarker is a blockquote or list marker at the start of a line: what follows it can be a heading too.
var containerMarker = regexp.MustCompile(`^[ \t]*(> ?|[-*+][ \t]+|[0-9]{1,9}[.)][ \t]+)`)

// containerFree is line without the blockquote and list markers it starts with.
func containerFree(line string) string {
	for {
		next := containerMarker.ReplaceAllString(line, "")
		if next == line {
			return line
		}
		line = next
	}
}

// startsBlock reports whether a line, without its leading spaces and markers, could be a heading, front matter
// or a heading's underline.
func startsBlock(t string) bool {
	if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "---") {
		return true
	}
	u := strings.TrimRight(t, " \t")
	return u != "" && (strings.Trim(u, "=") == "" || strings.Trim(u, "-") == "")
}
