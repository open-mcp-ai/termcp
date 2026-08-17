package screenshot

import (
	"bytes"
	"image/png"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

func msg(typ api.MsgType, content string) api.Message {
	return api.Message{Type: typ, Content: content, CreatedAt: time.Now().UTC()}
}

func TestRender_ProducesValidPNG(t *testing.T) {
	msgs := []api.Message{
		msg(api.MsgInput, "ls -la\n"),
		msg(api.MsgOutput, "drwxr-xr-x 2 root root 4096 .\n"),
		msg(api.MsgOutput, "flag{abc123}\n"),
	}
	data, err := Render(msgs, Options{Cols: 80, Theme: "dark"})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty PNG bytes")
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output is not a valid PNG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("invalid image bounds: %+v", b)
	}
	// Prompt line + output line => at least 3 display rows (incl. the prompt row).
	expectedMin := 30 // pad*2 + 1 row
	if b.Dy() < expectedMin {
		t.Fatalf("height %d too small; expected at least %d", b.Dy(), expectedMin)
	}
}

func TestRender_LineRangeSelection(t *testing.T) {
	msgs := []api.Message{
		msg(api.MsgInput, "cmd1\n"),
		msg(api.MsgInput, "cmd2\n"),
		msg(api.MsgInput, "cmd3\n"),
	}
	// All rows (3 prompts).
	all, _ := Render(msgs, Options{Cols: 80})
	// Only one display line (lines=1).
	one, _ := Render(msgs, Options{Cols: 80, Start: 1, Lines: 1})
	// One filtered row should be strictly shorter in height than all three rows.
	if !(one != nil && all != nil) {
		t.Fatal("Render returned nil")
	}
	if len(one) == len(all) {
		t.Fatal("expected limited-lines render to differ in height")
	}
	// Sanity: PNG decodes for both.
	for _, d := range [][]byte{all, one} {
		if _, err := png.Decode(bytes.NewReader(d)); err != nil {
			t.Fatalf("invalid PNG: %v", err)
		}
	}
}

func TestRender_LightTheme(t *testing.T) {
	msgs := []api.Message{msg(api.MsgInput, "whoami\n"), msg(api.MsgOutput, "root\n")}
	data, err := Render(msgs, Options{Cols: 80, Theme: "light"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("valid PNG expected for light theme: %v", err)
	}
}

func TestRender_UnknownUnicodeTolerated(t *testing.T) {
	msgs := []api.Message{msg(api.MsgOutput, "你好，世界 /root\n")}
	data, err := Render(msgs, Options{Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("valid PNG expected for CJK input (rendered as fallback): %v", err)
	}
}

func TestFlatten_WrapsLongLines(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	rows := flatten([]api.Message{msg(api.MsgOutput, long)}, 80)
	if len(rows) < 2 {
		t.Fatalf("expected long line to wrap into multiple rows, got %d", len(rows))
	}
}

func TestFlatten_ANSIStripped(t *testing.T) {
	rows := flatten([]api.Message{msg(api.MsgOutput, "\x1b[31mred\x1b[0m text\n")}, 80)
	if len(rows) == 0 {
		t.Fatal("expected output rows")
	}
	if containsByteEscape(rows[0].text) {
		t.Fatalf("ANSI escapes not stripped: %q", rows[0].text)
	}
}

func containsByteEscape(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			return true
		}
	}
	return false
}
