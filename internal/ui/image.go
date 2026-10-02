package ui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nkapila6/jupytui/internal/notebook"
)

// Images (matplotlib, seaborn, PIL) are drawn with "▀" half blocks: the
// foreground colours the top pixel, the background the bottom one. Low
// resolution, but it works in any truecolor terminal, nvim's included.
// gx opens the real thing in the system viewer.

const maxImageRows = 30

var imageMimes = []string{"image/png", "image/jpeg", "image/svg+xml"}

type imgKey struct {
	o     *notebook.Output
	width int
	dark  bool
}

// imageData returns the first image in an output's mime bundle.
func imageData(o *notebook.Output) (mime string, data []byte, ok bool) {
	for _, mt := range imageMimes {
		raw, has := o.Data[mt]
		if !has {
			continue
		}
		if mt == "image/svg+xml" {
			s, _ := o.DataText(mt)
			return mt, []byte(s), true
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			var parts []string
			if json.Unmarshal(raw, &parts) != nil {
				continue
			}
			s = strings.Join(parts, "")
		}
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			continue
		}
		return mt, b, true
	}
	return "", nil, false
}

// outImg is where an image sits in a cell's output lines, for sixel.
type outImg struct {
	line, id, cols, rows int
}

// renderImage returns the output's image lines, or false if it has no
// image: kitty placeholders, blank cells for sixel to draw over, or
// half blocks.
func (m *Model) renderImage(o *notebook.Output, width int) ([]string, *outImg, bool) {
	if o.OutputType != "display_data" && o.OutputType != "execute_result" {
		return nil, nil, false
	}
	mt, data, ok := imageData(o)
	if !ok {
		return nil, nil, false
	}
	caption := func(b image.Rectangle) string {
		return m.st.dim.Render(fmt.Sprintf("%s %d×%d · gx to open", mt, b.Dx(), b.Dy()))
	}
	switch m.gfxMode {
	case gfxKitty:
		// until the terminal has it, half blocks keep the space filled
		if g := m.gfxFor(o); g != nil && g.sent && g.id != 0 {
			return append(kittyLines(g), caption(g.img.Bounds())), nil, true
		}
	case gfxSixel:
		if g := m.gfxFor(o); g != nil {
			cols, rows := m.imgSize(g.img, width)
			g.cols, g.rows = cols, rows
			lines := make([]string, rows, rows+1)
			for i := range lines {
				lines[i] = strings.Repeat(" ", cols)
			}
			return append(lines, caption(g.img.Bounds())), &outImg{id: g.id, cols: cols, rows: rows}, true
		}
	}
	key := imgKey{o, width, m.dark}
	if lines, ok := m.imgCache[key]; ok {
		return lines, nil, true
	}
	var lines []string
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		lines = []string{m.st.dim.Render(fmt.Sprintf("[%s · gx to open]", mt))}
	} else {
		lines = m.halfBlocks(img, width)
		b := img.Bounds()
		lines = append(lines, m.st.dim.Render(fmt.Sprintf("%s %d×%d · gx to open", mt, b.Dx(), b.Dy())))
	}
	if len(m.imgCache) > 100 {
		m.imgCache = map[imgKey][]string{}
	}
	m.imgCache[key] = lines
	return lines, nil, true
}

// imageBG is what transparent pixels get blended onto.
func (m *Model) imageBG() color.RGBA {
	if m.dark {
		return color.RGBA{0x1a, 0x1b, 0x26, 0xff}
	}
	return color.RGBA{0xff, 0xff, 0xff, 0xff}
}

func (m *Model) halfBlocks(img image.Image, maxW int) []string {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return nil
	}
	// one column per pixel, two pixel rows per text row
	tw := min(maxW, w)
	th := h * tw / w
	if th > maxImageRows*2 {
		th = maxImageRows * 2
		tw = max(w*th/h, 1)
	}
	th = max(th+th%2, 2)

	px := boxResize(img, tw, th, m.imageBG())

	var lines []string
	for y := 0; y < th; y += 2 {
		var sb strings.Builder
		var lastFg, lastBg color.RGBA
		for x := range tw {
			fg, bgc := px[y*tw+x], px[(y+1)*tw+x]
			if x == 0 || fg != lastFg {
				fmt.Fprintf(&sb, "\x1b[38;2;%d;%d;%dm", fg.R, fg.G, fg.B)
			}
			if x == 0 || bgc != lastBg {
				fmt.Fprintf(&sb, "\x1b[48;2;%d;%d;%dm", bgc.R, bgc.G, bgc.B)
			}
			lastFg, lastBg = fg, bgc
			sb.WriteString("▀")
		}
		sb.WriteString("\x1b[0m")
		lines = append(lines, sb.String())
	}
	return lines
}

// boxResize averages every source pixel that lands in each target pixel,
// so thin plot lines fade instead of vanishing like with nearest
// neighbour. Transparent pixels are blended onto bg.
func boxResize(img image.Image, tw, th int, bg color.RGBA) []color.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([]color.RGBA, tw*th)
	for ty := range th {
		y0, y1 := ty*h/th, max((ty+1)*h/th, ty*h/th+1)
		for tx := range tw {
			x0, x1 := tx*w/tw, max((tx+1)*w/tw, tx*w/tw+1)
			var r, g, bl, n uint64
			for y := y0; y < y1 && y < h; y++ {
				for x := x0; x < x1 && x < w; x++ {
					cr, cg, cb, ca := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
					// premultiplied 16-bit: blend onto bg
					inv := 0xffff - ca
					r += uint64(cr + inv*uint32(bg.R)*0x101/0xffff)
					g += uint64(cg + inv*uint32(bg.G)*0x101/0xffff)
					bl += uint64(cb + inv*uint32(bg.B)*0x101/0xffff)
					n++
				}
			}
			if n == 0 {
				out[ty*tw+tx] = bg
				continue
			}
			out[ty*tw+tx] = color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 0xff}
		}
	}
	return out
}

// openImage writes the selected cell's first image to a temp file and
// hands it to the system viewer (gx).
func (m *Model) openImage() {
	for _, o := range m.cell().Outputs {
		mt, data, ok := imageData(o)
		if !ok {
			continue
		}
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/svg+xml": ".svg"}[mt]
		if m.imgDir == "" {
			dir, err := os.MkdirTemp("", "jupytui-img-")
			if err != nil {
				m.msg = "gx: " + err.Error()
				return
			}
			m.imgDir = dir
		}
		m.imgN++
		path := filepath.Join(m.imgDir, fmt.Sprintf("output-%d%s", m.imgN, ext))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			m.msg = "gx: " + err.Error()
			return
		}
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		if err := exec.Command(opener, path).Start(); err != nil {
			m.msg = "gx: " + err.Error()
			return
		}
		m.msg = "opened " + filepath.Base(path)
		return
	}
	m.msg = "no image in this cell"
}
