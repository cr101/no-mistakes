// Package fakegfm is the test fakes' stand-in for GitHub's Markdown renderer
// (POST /markdown, mode gfm), used by the fake gh CLIs
// (internal/pipeline/fakecli and cmd/fakeagent). It renders only the subset
// the PR step's closing-reference handling relies on, each case matching the
// real renderer:
//   - an HTML comment is removed: one opened at the start of a line (after up
//     to three spaces) runs through the line holding `-->`, across lines, or
//     to the end of the document, and an inline one closed on its own line is
//     removed too;
//   - a stray inline `<!--` with no closing `-->` is literal text and hides
//     nothing after it;
//   - fenced code renders inside <pre><code>, an inline code span inside
//     <code>, and raw <pre>/<code> elements stay as written.
//
// Everything else passes through unchanged.
package fakegfm

import (
	"html"
	"regexp"
	"strings"
)

var (
	codeSpanPattern      = regexp.MustCompile("`([^`]+)`")
	inlineCommentPattern = regexp.MustCompile(`<!--.*?-->`)
)

// Render returns text rendered to HTML as GitHub would for the subset above.
func Render(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []string
	fence := ""
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimLeft(line, " ")
		blockStart := len(line)-len(trimmed) <= 3
		switch {
		case fence != "":
			if blockStart && strings.HasPrefix(trimmed, fence) {
				out = append(out, "</code></pre>")
				fence = ""
			} else {
				out = append(out, html.EscapeString(line))
			}
		case blockStart && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")):
			fence = trimmed[:3]
			out = append(out, "<pre><code>")
		case blockStart && strings.HasPrefix(trimmed, "<!--"):
			for i < len(lines) && !strings.Contains(lines[i], "-->") {
				i++
			}
		default:
			out = append(out, renderInline(line))
		}
	}
	if fence != "" {
		out = append(out, "</code></pre>")
	}
	return strings.Join(out, "\n")
}

func renderInline(line string) string {
	line = codeSpanPattern.ReplaceAllStringFunc(line, func(span string) string {
		return "<code>" + html.EscapeString(span[1:len(span)-1]) + "</code>"
	})
	line = inlineCommentPattern.ReplaceAllString(line, "")
	return strings.ReplaceAll(line, "<!--", "&lt;!--")
}
