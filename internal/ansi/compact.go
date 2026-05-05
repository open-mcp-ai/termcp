package ansi

import (
	"regexp"
	"strings"
)

var blankLineRe = regexp.MustCompile(`\n{3,}`)

// Compact reduces terminal output noise for LLM consumption. It removes control
// characters (except \r, \n, \t), normalizes CRLF, collapses \r-overwrite
// sequences (progress bars), strips trailing whitespace, folds excess blank
// lines, and trims leading/trailing blanks.
func Compact(text string) string {
	if text == "" {
		return ""
	}

	// Fast path: return early if input looks already clean
	if !needsCompact(text) {
		return text
	}

	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c < 0x20 && c != '\r' && c != '\n' && c != '\t' {
			continue
		}
		if c == 0x7F {
			continue
		}
		b.WriteByte(c)
	}
	cleaned := b.String()

	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")

	lines := strings.Split(cleaned, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		line = strings.TrimRight(line, " \t")
		result = append(result, line)
	}
	cleaned = strings.Join(result, "\n")

	cleaned = blankLineRe.ReplaceAllString(cleaned, "\n\n")

	cleaned = strings.TrimLeft(cleaned, "\n")
	if strings.HasSuffix(cleaned, "\n\n") {
		cleaned = strings.TrimRight(cleaned, "\n")
	}

	return cleaned
}

func needsCompact(text string) bool {
	hasControl := false
	hasCr := false
	hasTrailingSpace := false
	blankRun := 0
	maxBlankRun := 0

	for i := 0; i < len(text); i++ {
		c := text[i]
		if c < 0x20 && c != '\r' && c != '\n' && c != '\t' {
			hasControl = true
		}
		if c == 0x7F {
			hasControl = true
		}
		if c == '\r' {
			hasCr = true
		}
		if c == ' ' || c == '\t' {
			if i+1 >= len(text) || text[i+1] == '\n' {
				hasTrailingSpace = true
			}
		}
		if c == '\n' {
			blankRun++
			if blankRun > maxBlankRun {
				maxBlankRun = blankRun
			}
		} else {
			blankRun = 0
		}
	}

	if text != "" && text[0] == '\n' {
		return true
	}

	return hasControl || hasCr || hasTrailingSpace || maxBlankRun >= 3
}
