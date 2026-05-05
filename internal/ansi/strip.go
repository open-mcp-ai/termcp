package ansi

import (
	"regexp"
)

var ansiRe = regexp.MustCompile(
	`\x1b(?:` +
		`[\[(][0-?]*[ -/]*[@-~]` + // CSI sequences
		`|\].*?(?:\x1b\\|\x07)` + // OSC sequences
		`|[()][Bb0UK]` + // Character set
		`|[ -/]*[0-~]` + // 2-byte sequences
		`)`)

// Strip removes ANSI escape codes from text.
func Strip(text string) string {
	return ansiRe.ReplaceAllString(text, "")
}
