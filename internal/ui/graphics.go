package ui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/nkapila6/jupytui/internal/notebook"
	"golang.org/x/sys/unix"
)

// Real images for terminals that can show them. Kitty graphics (kitty,
// Ghostty) use unicode placeholders: the image is sent once and the
// frame just contains placeholder characters, so it scrolls and clips
// like text. Sixel (WezTerm, iTerm2, foot) has no such thing, so the
// frame reserves blank cells and the image is drawn over them after the
// frame lands. Everything else, and anything inside nvim's terminal or
// tmux (neither passes graphics through), gets half blocks.

type gfxMode int

const (
	gfxBlocks gfxMode = iota
	gfxKitty
	gfxSixel
)

func (g gfxMode) String() string {
	return [...]string{"blocks", "kitty", "sixel"}[g]
}

func parseGfx(s string) (gfxMode, bool) {
	switch strings.ToLower(s) {
	case "kitty":
		return gfxKitty, true
	case "sixel":
		return gfxSixel, true
	case "blocks", "halfblocks", "none":
		return gfxBlocks, true
	}
	return gfxBlocks, false
}

func detectGraphics() gfxMode {
	if g, ok := parseGfx(os.Getenv("JUPYTUI_IMAGES")); ok {
		return g
	}
	if os.Getenv("NVIM") != "" || os.Getenv("TMUX") != "" || strings.HasPrefix(os.Getenv("TERM"), "screen") {
		return gfxBlocks
	}
	term, prog := os.Getenv("TERM"), os.Getenv("TERM_PROGRAM")
	switch {
	case os.Getenv("KITTY_WINDOW_ID") != "", term == "xterm-kitty",
		strings.EqualFold(prog, "ghostty"), os.Getenv("GHOSTTY_RESOURCES_DIR") != "":
		return gfxKitty
	// WezTerm speaks kitty graphics too, but not the placeholder part
	case prog == "WezTerm", prog == "iTerm.app", strings.HasPrefix(term, "foot"), strings.Contains(term, "mlterm"):
		return gfxSixel
	}
	return gfxBlocks
}

// cellPixels is the size of one character cell in pixels, from the
// terminal; 8x16 when it doesn't say.
func cellPixels() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 || ws.Xpixel == 0 || ws.Ypixel == 0 {
		return 8, 16
	}
	return int(ws.Xpixel) / int(ws.Col), int(ws.Ypixel) / int(ws.Row)
}

// gfxImage is a decoded output image and where it's been sent.
type gfxImage struct {
	id         int
	img        image.Image
	png        []byte
	cols, rows int // current size in cells
	sent       bool
}

// imgSize picks the cell size for an image: its natural size, no wider
// than the output area and no taller than most of the screen.
func (m *Model) imgSize(img image.Image, width int) (int, int) {
	b := img.Bounds()
	cw, ch := m.cellW, m.cellH
	cols := min(width, max((b.Dx()+cw-1)/cw, 1))
	rows := max((cols*cw*b.Dy()+b.Dx()*ch-1)/(b.Dx()*ch), 1)
	if maxRows := max(m.bodyHeight()-3, 4); rows > maxRows {
		rows = maxRows
		cols = max(min(rows*ch*b.Dx()/(b.Dy()*cw), width), 1)
	}
	return cols, rows
}

// gfxFor returns (and remembers) the decoded image for an output.
func (m *Model) gfxFor(o *notebook.Output) *gfxImage {
	if g, ok := m.gfx[o]; ok {
		return g
	}
	mt, data, ok := imageData(o)
	if !ok || mt == "image/svg+xml" {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	if mt != "image/png" {
		var buf bytes.Buffer
		if png.Encode(&buf, img) != nil {
			return nil
		}
		data = buf.Bytes()
	}
	g := &gfxImage{img: img, png: data}
	m.assignID(g)
	m.gfx[o] = g
	return g
}

// Image ids ride in the placeholder's 256-colour foreground (38;5;id).
// Truecolor ids would be cheaper on space but the renderer downsamples
// them on terminals it thinks are 256-colour, which changes the id; and
// 0-15 can come out as basic SGR colours. So ids are 16..255, recycled
// when a notebook has more images than that.
const firstGfxID, gfxIDs = 16, 240

func (m *Model) assignID(g *gfxImage) {
	id := firstGfxID + m.nextGfxID%gfxIDs
	m.nextGfxID++
	for _, other := range m.gfx {
		if other.id == id {
			// the old owner gets a new id (and is re-sent) if it's shown again
			other.id, other.sent = 0, false
		}
	}
	g.id = id
}

// syncKitty sends any image that isn't in the terminal yet, or whose
// size changed, for every image output in the notebook.
func (m *Model) syncKitty() tea.Cmd {
	if m.gfxMode != gfxKitty || m.width == 0 {
		return nil
	}
	width := m.boxWidth() - 2
	var b strings.Builder
	for _, c := range m.nb.Cells {
		for _, o := range c.Outputs {
			g := m.gfxFor(o)
			if g == nil {
				continue
			}
			if g.id == 0 {
				m.assignID(g)
			}
			cols, rows := m.imgSize(g.img, width)
			switch {
			case !g.sent:
				b.WriteString(kittyTransmit(g.id, g.png, cols, rows))
			case cols != g.cols || rows != g.rows:
				fmt.Fprintf(&b, "\x1b_Ga=p,U=1,i=%d,p=1,c=%d,r=%d,q=2\x1b\\", g.id, cols, rows)
			default:
				continue
			}
			g.sent, g.cols, g.rows = true, cols, rows
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return tea.Raw(b.String())
}

// kittyTransmit sends a PNG and makes a virtual placement (U=1) that
// placeholder characters in the frame point at.
func kittyTransmit(id int, data []byte, cols, rows int) string {
	enc := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for i := 0; i < len(enc); i += kitty.MaxChunkSize {
		chunk := enc[i:min(i+kitty.MaxChunkSize, len(enc))]
		more := 1
		if i+kitty.MaxChunkSize >= len(enc) {
			more = 0
		}
		if i == 0 {
			fmt.Fprintf(&b, "\x1b_Ga=T,f=100,t=d,i=%d,p=1,U=1,c=%d,r=%d,q=2,m=%d;%s\x1b\\", id, cols, rows, more, chunk)
		} else {
			fmt.Fprintf(&b, "\x1b_Gm=%d;%s\x1b\\", more, chunk)
		}
	}
	return b.String()
}

// kittyLines is the placeholder text for an image. Every cell carries
// its own row/column diacritics so a popup drawn over part of the image
// can't confuse the rest of the row.
func kittyLines(g *gfxImage) []string {
	// the foreground colour carries the image id
	fg := fmt.Sprintf("\x1b[38;5;%dm", g.id)
	lines := make([]string, g.rows)
	for r := range g.rows {
		var b strings.Builder
		b.WriteString(fg)
		for c := range g.cols {
			b.WriteRune(kitty.Placeholder)
			b.WriteRune(kitty.Diacritic(r))
			b.WriteRune(kitty.Diacritic(c))
		}
		b.WriteString("\x1b[39m")
		lines[r] = b.String()
	}
	return lines
}

func hasPlaceholder(s string) bool {
	return strings.ContainsRune(s, kitty.Placeholder)
}

func (m *Model) kittyClear() string {
	if m.gfxMode != gfxKitty {
		return ""
	}
	// d=A drops every image and frees its data
	return "\x1b_Ga=d,d=A,q=2\x1b\\"
}

// ---- sixel ----

// sixelPlace is one image's visible part on screen.
type sixelPlace struct {
	id                  int
	x, y                int // screen cell of the top-left visible corner
	cols, rows, cropTop int
}

type sixelTickMsg struct{}

type sixelDrawMsg struct{ places []sixelPlace }

func sixelTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg { return sixelTickMsg{} })
}

func samePlaces(a, b []sixelPlace) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// handleSixelTick redraws images when what's on screen moved: wipe the
// old pixels with a full repaint, then draw once that frame is out.
func (m *Model) handleSixelTick() tea.Cmd {
	if m.gfxMode != gfxSixel {
		return nil
	}
	want := m.sixelWant
	if samePlaces(want, m.sixelDrawn) {
		return sixelTick()
	}
	m.sixelDrawn = want
	draw := tea.Tick(50*time.Millisecond, func(time.Time) tea.Msg { return sixelDrawMsg{want} })
	return tea.Batch(tea.ClearScreen, draw, sixelTick())
}

func (m *Model) handleSixelDraw(msg sixelDrawMsg) tea.Cmd {
	// stale: the screen changed again while we waited
	if !samePlaces(msg.places, m.sixelWant) || len(msg.places) == 0 {
		return nil
	}
	var b strings.Builder
	for _, p := range msg.places {
		s := m.sixelData(p)
		if s == "" {
			continue
		}
		b.WriteString("\x1b7")
		b.WriteString(ansi.CursorPosition(p.x+1, p.y+1))
		b.WriteString(s)
		b.WriteString("\x1b8")
	}
	if b.Len() == 0 {
		return nil
	}
	return tea.Raw(b.String())
}

// sixelData encodes the visible slice of an image at exactly the cell
// area's pixel size, cached since encoding is slow.
func (m *Model) sixelData(p sixelPlace) string {
	var g *gfxImage
	for _, gi := range m.gfx {
		if gi.id == p.id {
			g = gi
			break
		}
	}
	if g == nil {
		return ""
	}
	key := fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d", p.id, g.cols, g.rows, p.cropTop, p.rows, m.cellW, m.cellH)
	if s, ok := m.sixelCache[key]; ok {
		return s
	}
	cw, ch := m.cellW, m.cellH
	full := image.NewRGBA(image.Rect(0, 0, g.cols*cw, g.rows*ch))
	px := boxResize(g.img, g.cols*cw, g.rows*ch, m.imageBG())
	for i, c := range px {
		full.Pix[i*4], full.Pix[i*4+1], full.Pix[i*4+2], full.Pix[i*4+3] = c.R, c.G, c.B, 0xff
	}
	crop := image.NewRGBA(image.Rect(0, 0, g.cols*cw, p.rows*ch))
	draw.Draw(crop, crop.Bounds(), full, image.Pt(0, p.cropTop*ch), draw.Src)
	s := ansi.SixelGraphics(0, 1, 0, encodeSixel(crop))
	if len(m.sixelCache) > 50 {
		m.sixelCache = map[string]string{}
	}
	m.sixelCache[key] = s
	return s
}
