package ansi

import "testing"

func TestCompact_Passthrough(t *testing.T) {
	input := "hello world\nline two\n"
	got := Compact(input)
	if got != input {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestCompact_Empty(t *testing.T) {
	got := Compact("")
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestCompact_ControlChars(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"bell", "hello\x07world", "helloworld"},
		{"backspace", "abc\x08x", "abcx"},
		{"multiple controls", "a\x00b\x0ec\x7fd", "abcd"},
		{"preserve tab", "hello\tworld", "hello\tworld"},
		{"preserve newline", "hello\nworld", "hello\nworld"},
		{"cr is overwrite marker", "hello\rworld", "world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compact(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestCompact_CarriageReturnOverwrite(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple overwrite", "foo\rbar\r\n", "bar\n"},
		{"progress bar collapse", "0%\r1%\r2%\r3%\r\n", "3%\n"},
		{"multiple lines", "line1a\rline1b\nline2a\rline2b\n", "line1b\nline2b\n"},
		{"final line no newline", "progress...\rdone", "done"},
		{"no overwrite", "plain line\n", "plain line\n"},
		{"mixed overwrite and plain", "overwrite_a\roverwrite_b\nnormal line\n", "overwrite_b\nnormal line\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compact(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestCompact_TrailingWhitespace(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"trailing spaces", "hello   \n", "hello\n"},
		{"trailing tabs", "hello\t\t\n", "hello\n"},
		{"mixed trailing", "hello \t \n", "hello\n"},
		{"no trailing", "hello\n", "hello\n"},
		{"middle spaces preserved", "hello   world\n", "hello   world\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compact(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestCompact_BlankLineFolding(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"3 blanks to 1", "a\n\n\nb", "a\n\nb"},
		{"4 blanks to 1", "a\n\n\n\nb", "a\n\nb"},
		{"2 blanks preserved", "a\n\nb", "a\n\nb"},
		{"single blank preserved", "a\nb", "a\nb"},
		{"no blanks", "a\nb\nc", "a\nb\nc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compact(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestCompact_EdgeTrim(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"leading blanks", "\n\nhello\n", "hello\n"},
		{"trailing blanks", "hello\n\n\n", "hello"},
		{"both edges", "\n\nhello\n\n\n", "hello"},
		{"no edge blanks", "hello\nworld\n", "hello\nworld\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Compact(tt.input)
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}
