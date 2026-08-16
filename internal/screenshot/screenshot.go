package screenshot

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

const (
	glyphWidth  = 5
	glyphHeight = 7
	cellWidth   = 6
	cellHeight  = 10
	defaultCols = 80
)

// Palette holds the colors used for one theme.
type Palette struct {
	Background color.Color
	Foreground color.Color
	Prompt     color.Color
}

var (
	dark = Palette{
		Background: color.RGBA{0x1e, 0x1e, 0x1e, 0xff},
		Foreground: color.RGBA{0xe0, 0xe0, 0xe0, 0xff},
		Prompt:     color.RGBA{0x4e, 0xc9, 0xb0, 0xff},
	}
	light = Palette{
		Background: color.RGBA{0xff, 0xff, 0xff, 0xff},
		Foreground: color.RGBA{0x1e, 0x1e, 0x1e, 0xff},
		Prompt:     color.RGBA{0x0f, 0x6f, 0x5c, 0xff},
	}
)

// Options controls screenshot layout.
type Options struct {
	Start int    // first display line to include (0-based)
	Lines int    // number of display lines to render; 0 = all
	Cols  int    // terminal width in columns
	Theme string // "dark" (default) or "light"
}

// Render draws persisted messages as a terminal-style PNG and returns the bytes.
func Render(msgs []api.Message, opts Options) ([]byte, error) {
	cols := opts.Cols
	if cols < 20 {
		cols = defaultCols
	}
	pal := dark
	if opts.Theme == "light" {
		pal = light
	}
	display := flatten(msgs, cols)

	start := opts.Start
	if start < 0 {
		start = 0
	}
	if start > len(display) {
		start = len(display)
	}
	end := len(display)
	if opts.Lines > 0 && start+opts.Lines < end {
		end = start + opts.Lines
	}
	sel := display[start:end]

	const pad = 10
	width := pad*2 + cols*cellWidth
	height := pad*2 + max(1, len(sel))*cellHeight
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(img, pal.Background)
	for row, line := range sel {
		drawRow(img, pad, pad+row*cellHeight, line, pal, cols)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type displayRow struct {
	prompt bool
	text   string
}

func drawRow(img *image.RGBA, x, y int, row displayRow, pal Palette, cols int) {
	if row.prompt {
		x = drawString(img, x, y, "$ ", pal.Prompt, cols)
		drawString(img, x, y, row.text, pal.Foreground, cols-2)
		return
	}
	drawString(img, x, y, row.text, pal.Foreground, cols)
}

func drawString(img *image.RGBA, x, y int, s string, fg color.Color, cols int) int {
	if cols <= 0 {
		return x
	}
	used := 0
	for _, r := range s {
		if used >= cols {
			break
		}
		glyph, ok := font5x7[r]
		if !ok {
			glyph = font5x7['?']
		}
		drawGlyph(img, x+used*cellWidth, y, glyph, fg)
		used++
	}
	return x + used*cellWidth
}

func drawGlyph(img *image.RGBA, x, y int, glyph [glyphHeight]string, fg color.Color) {
	for row, bits := range glyph {
		for col, bit := range bits {
			if bit == '#' {
				img.Set(x+col, y+row, fg)
			}
		}
	}
}

func fill(img *image.RGBA, c color.Color) {
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			img.Set(x, y, c)
		}
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func flatten(msgs []api.Message, cols int) []displayRow {
	var rows []displayRow
	for _, m := range msgs {
		content := ansi.Strip(m.Content)
		if m.Type == api.MsgInput {
			content = strings.TrimSpace(content)
			for _, wrapped := range wrap(content, cols-2) {
				rows = append(rows, displayRow{prompt: true, text: wrapped})
			}
		} else if m.Type == api.MsgOutput {
			content = strings.TrimRight(content, "\r\n")
			for _, line := range strings.Split(content, "\n") {
				for _, wrapped := range wrap(line, cols) {
					rows = append(rows, displayRow{text: wrapped})
				}
			}
		}
	}
	return rows
}

func wrap(s string, maxRunes int) []string {
	if maxRunes <= 0 {
		return nil
	}
	if s == "" {
		return []string{""}
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		runes := []rune(line)
		for len(runes) > maxRunes {
			out = append(out, string(runes[:maxRunes]))
			runes = runes[maxRunes:]
		}
		out = append(out, string(runes))
	}
	return out
}

var font5x7 = map[rune][glyphHeight]string{
	' ':  {"     ", "     ", "     ", "     ", "     ", "     ", "     "},
	'?':  {" ### ", "#   #", "    #", "   # ", "  #  ", "     ", "  #  "},
	'.':  {"     ", "     ", "     ", "     ", "     ", " ### ", " ### "},
	',':  {"     ", "     ", "     ", "     ", "     ", " ##  ", "#    "},
	':':  {"     ", " ##  ", " ##  ", "     ", " ##  ", " ##  ", "     "},
	';':  {"     ", " ##  ", " ##  ", "     ", " ##  ", " ##  ", "#    "},
	'-':  {"     ", "     ", "     ", " ### ", "     ", "     ", "     "},
	'_':  {"     ", "     ", "     ", "     ", "     ", "     ", "#####"},
	'/':  {"    #", "   # ", "   # ", "  #  ", " #   ", " #   ", "#    "},
	'\\': {"#    ", " #   ", " #   ", "  #  ", "   # ", "   # ", "    #"},
	'|':  {"  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "  #  "},
	'=':  {"     ", " ### ", "     ", " ### ", "     ", "     ", "     "},
	'+':  {"     ", "  #  ", "  #  ", "#####", "  #  ", "  #  ", "     "},
	'0':  {" ### ", "#   #", "#  ##", "# # #", "##  #", "#   #", " ### "},
	'1':  {"  #  ", " ##  ", "# #  ", "  #  ", "  #  ", "  #  ", "#####"},
	'2':  {" ### ", "#   #", "    #", "   # ", "  #  ", " #   ", "#####"},
	'3':  {" ### ", "#   #", "    #", " ### ", "    #", "#   #", " ### "},
	'4':  {"   # ", "  ## ", " # # ", "#  # ", "#####", "   # ", "   # "},
	'5':  {"#####", "#    ", "#    ", "#### ", "    #", "#   #", " ### "},
	'6':  {" ### ", "#   #", "#    ", "#### ", "#   #", "#   #", " ### "},
	'7':  {"#####", "    #", "   # ", "  #  ", " #   ", " #   ", " #   "},
	'8':  {" ### ", "#   #", "#   #", " ### ", "#   #", "#   #", " ### "},
	'9':  {" ### ", "#   #", "#   #", " ####", "    #", "#   #", " ### "},
}

func init() {
	addLetters()
}

func addLetters() {
	patterns := map[rune][glyphHeight]string{
		'A': {" ### ", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"},
		'B': {"#### ", "#   #", "#   #", "#### ", "#   #", "#   #", "#### "},
		'C': {" ### ", "#   #", "#    ", "#    ", "#    ", "#   #", " ### "},
		'D': {"#### ", "#   #", "#   #", "#   #", "#   #", "#   #", "#### "},
		'E': {"#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#####"},
		'F': {"#####", "#    ", "#    ", "#### ", "#    ", "#    ", "#    "},
		'G': {" ### ", "#   #", "#    ", "# ###", "#   #", "#   #", " ### "},
		'H': {"#   #", "#   #", "#   #", "#####", "#   #", "#   #", "#   #"},
		'I': {"#####", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "#####"},
		'J': {"  ###", "   # ", "   # ", "   # ", "#  # ", "#  # ", " ##  "},
		'K': {"#   #", "#  # ", "# #  ", "##   ", "# #  ", "#  # ", "#   #"},
		'L': {"#    ", "#    ", "#    ", "#    ", "#    ", "#    ", "#####"},
		'M': {"#   #", "## ##", "# # #", "#   #", "#   #", "#   #", "#   #"},
		'N': {"#   #", "##  #", "##  #", "# # #", "#  ##", "#  ##", "#   #"},
		'O': {" ### ", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "},
		'P': {"#### ", "#   #", "#   #", "#### ", "#    ", "#    ", "#    "},
		'Q': {" ### ", "#   #", "#   #", "#   #", "# # #", "#  # ", " ## #"},
		'R': {"#### ", "#   #", "#   #", "#### ", "# #  ", "#  # ", "#   #"},
		'S': {" ####", "#    ", "#    ", " ### ", "    #", "    #", "#### "},
		'T': {"#####", "  #  ", "  #  ", "  #  ", "  #  ", "  #  ", "  #  "},
		'U': {"#   #", "#   #", "#   #", "#   #", "#   #", "#   #", " ### "},
		'V': {"#   #", "#   #", "#   #", "#   #", "#   #", " # # ", "  #  "},
		'W': {"#   #", "#   #", "#   #", "# # #", "# # #", "## ##", "#   #"},
		'X': {"#   #", "#   #", " # # ", "  #  ", " # # ", "#   #", "#   #"},
		'Y': {"#   #", "#   #", " # # ", "  #  ", "  #  ", "  #  ", "  #  "},
		'Z': {"#####", "    #", "   # ", "  #  ", " #   ", "#    ", "#####"},
	}
	for r, glyph := range patterns {
		font5x7[r] = glyph
		font5x7[r+'a'-'A'] = glyph
	}
}
