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

// renderOutputs draws a cell's outputs as lines at most width wide.
func (m *Model) renderOutputs(c *notebook.Cell, width int) []string {
	var lines []string
	truncated := false
	for _, o := range c.Outputs {
		if truncated {
			break
		}
		// images are already sized to width, skip the wrapping below
		if img, ok := m.renderImage(o, width); ok {
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
	return lines
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
	return strings.ReplaceAll(collapseCR(s), "\r", "")
}

// collapseCR drops text that a carriage return has overwritten, so a
// tqdm bar keeps only its latest frame (JupyterLab does the same). A
// trailing \r is kept since the next chunk is meant to overwrite.
func collapseCR(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		trail := strings.HasSuffix(l, "\r")
		l = strings.TrimSuffix(l, "\r")
		if j := strings.LastIndex(l, "\r"); j >= 0 {
			l = l[j+1:]
		}
		if trail {
			l += "\r"
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

func expandTabs(s string) string {
	return strings.ReplaceAll(s, "\t", "    ")
}

// mergeStream appends to the previous output when it's the same stream,
// like Jupyter does, so \r redraws work across chunks.
func mergeStream(outs []*notebook.Output, o *notebook.Output) []*notebook.Output {
	if o.OutputType != "stream" {
		return append(outs, o)
	}
	if len(outs) > 0 {
		last := outs[len(outs)-1]
		if last.OutputType == "stream" && last.Name == o.Name {
			last.Text = collapseCR(last.Text + o.Text)
			return outs
		}
	}
	o.Text = collapseCR(o.Text)
	return append(outs, o)
}

func padRight(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
