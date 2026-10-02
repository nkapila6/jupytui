package ui

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// maxOutputLines caps how much of one cell's output gets drawn. Huge
// outputs make every frame slow and nobody scrolls through 50k lines.
const maxOutputLines = 500

// renderOutputs draws a cell's outputs as lines at most width wide,
// plus where any sixel images go.
func (m *Model) renderOutputs(c *notebook.Cell, width int) ([]string, []outImg) {
	var lines []string
	var imgs []outImg
	if m.folded[c] && len(c.Outputs) > 0 {
		return []string{m.st.dim.Render(fmt.Sprintf("▸ %d output%s folded (za)", len(c.Outputs), plural(len(c.Outputs))))}, nil
	}
	truncated := false
	for _, o := range c.Outputs {
		if truncated {
			break
		}
		// images are already sized to width, skip the wrapping below
		if img, place, ok := m.renderImage(o, width); ok {
			if place != nil {
				place.line = len(lines)
				imgs = append(imgs, *place)
			}
			lines = append(lines, img...)
			continue
		}
		var s string
		var style *lipgloss.Style
		switch o.OutputType {
		case "stream":
			s = termText(o.Text)
			if o.Name == "stderr" {
				style = &m.st.stderr
			}
		case "error":
			s = strings.Join(o.Traceback, "\n")
			if s == "" {
				s = m.st.errText.Render(o.Ename + ": " + o.Evalue)
			}
		case "execute_result", "display_data":
			s = m.renderMime(o, width)
		}
		s = strings.TrimRight(expandTabs(s), "\n")
		if s == "" {
			continue
		}
		// stop before wrapping everything, huge outputs would make
		// every frame slow
		raw := strings.Split(s, "\n")
		for i, l := range raw {
			if len(lines) >= maxOutputLines {
				lines = append(lines, m.st.dim.Render(fmt.Sprintf("... output truncated, %d more lines", len(raw)-i)))
				truncated = true
				break
			}
			for _, w := range strings.Split(ansi.Hardwrap(l, width, true), "\n") {
				if style != nil {
					w = style.Render(w)
				}
				lines = append(lines, w)
			}
		}
	}
	return lines, imgs
}

// renderMime picks the richest mime type we can actually draw.
func (m *Model) renderMime(o *notebook.Output, width int) string {
	if s, ok := o.DataText("text/markdown"); ok {
		return m.markdown(s, width)
	}
	if h, ok := o.DataText("text/html"); ok && isDataFrameHTML(h) {
		key := strconv.Itoa(width) + "\x00" + h
		if s, ok := m.tableCache[key]; ok {
			return s
		}
		if t, ok := parseDataFrame(h); ok {
			s := m.renderTable(t, width)
			if len(m.tableCache) > 200 {
				m.tableCache = map[string]string{}
			}
			m.tableCache[key] = s
			return s
		}
	}
	plain, hasPlain := o.DataText("text/plain")
	// IPython's HTML() and friends only have a "<... object>" repr as
	// plain text, the HTML is the actual content
	if h, ok := o.DataText("text/html"); ok && (!hasPlain || objectRepr.MatchString(plain)) {
		return htmlText(h)
	}
	if hasPlain {
		return plain
	}
	var mimes []string
	for k := range o.Data {
		mimes = append(mimes, k)
	}
	sort.Strings(mimes)
	return m.st.dim.Render("[" + strings.Join(mimes, ", ") + "]")
}

var objectRepr = regexp.MustCompile(`^<[\w.]+ object( at 0x[0-9a-f]+)?>$`)

// termText is stream text as a terminal would show it.
func termText(s string) string {
	return strings.ReplaceAll(notebook.CollapseCR(s), "\r", "")
}

func expandTabs(s string) string {
	return strings.ReplaceAll(s, "\t", "    ")
}

func padRight(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
