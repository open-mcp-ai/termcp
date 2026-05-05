package ansi

import "testing"

func TestStrip_CSI(t *testing.T) {
	input := "\x1b[31mRed\x1b[0m text"
	got := Strip(input)
	if got != "Red text" {
		t.Fatalf("expected 'Red text', got %q", got)
	}
}

func TestStrip_OSC(t *testing.T) {
	input := "\x1b]0;window-title\x07prompt"
	got := Strip(input)
	if got != "prompt" {
		t.Fatalf("expected 'prompt', got %q", got)
	}
}

func TestStrip_Mixed(t *testing.T) {
	input := "\x1b[1;32m\x1b]0;title\x07OK\x1b[0m"
	got := Strip(input)
	if got != "OK" {
		t.Fatalf("expected 'OK', got %q", got)
	}
}

func TestStrip_CleanPassthrough(t *testing.T) {
	input := "clean text, no escape codes"
	got := Strip(input)
	if got != input {
		t.Fatalf("expected passthrough, got %q", got)
	}
}

func TestStrip_Empty(t *testing.T) {
	got := Strip("")
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestStrip_CursorMovement(t *testing.T) {
	input := "abc\x1b[2Dxy"
	got := Strip(input)
	if got != "abcxy" {
		t.Fatalf("expected 'abcxy', got %q", got)
	}
}

func TestStrip_OSCWithBEL(t *testing.T) {
	input := "\x1b]2;some-title\x07rest"
	got := Strip(input)
	if got != "rest" {
		t.Fatalf("expected 'rest', got %q", got)
	}
}

func TestStrip_OSCWithST(t *testing.T) {
	input := "\x1b]2;some-title\x1b\\rest"
	got := Strip(input)
	if got != "rest" {
		t.Fatalf("expected 'rest', got %q", got)
	}
}
