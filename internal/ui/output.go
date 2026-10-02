package ui

import (
	"fmt"
	"sort"
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
	for _, o := range c.Outputs {
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
		for _, l := range strings.Split(s, "\n") {
			for _, w := range strings.Split(ansi.Hardwrap(l, width, true), "\n") {
				if style != nil {
					w = style.Render(w)
				}
				lines = append(lines, w)
			}
		}
	}
	if len(lines) > maxOutputLines {
		more := len(lines) - maxOutputLines
		lines = append(lines[:maxOutputLines], m.st.dim.Render(fmt.Sprintf("... %d more lines", more)))
	}
	return lines
}

// renderMime picks the richest mime type we can actually draw.
func (m *Model) renderMime(o *notebook.Output, width int) string {
	if s, ok := o.DataText("text/markdown"); ok {
		return m.markdown(s, width)
	}
	if s, ok := o.DataText("text/plain"); ok {
		return s
	}
	var mimes []string
	for k := range o.Data {
		mimes = append(mimes, k)
	}
	sort.Strings(mimes)
	return m.st.dim.Render("[" + strings.Join(mimes, ", ") + "]")
}

// termText applies carriage returns the way a terminal would, so
// progress bars (tqdm etc) show their latest state instead of every frame.
func termText(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndex(l, "\r"); j >= 0 {
			// a trailing \r means the next write redraws; keep what's there
			if j == len(l)-1 {
				l = l[:j]
				if k := strings.LastIndex(l, "\r"); k >= 0 {
					l = l[k+1:]
				}
			} else {
				l = l[j+1:]
			}
			lines[i] = l
		}
	}
	return strings.Join(lines, "\n")
}

func expandTabs(s string) string {
	return strings.ReplaceAll(s, "\t", "    ")
}

// mergeStream appends to the previous output when it's the same stream,
// like Jupyter does, so \r redraws work across chunks.
func mergeStream(outs []*notebook.Output, o *notebook.Output) []*notebook.Output {
	if o.OutputType == "stream" && len(outs) > 0 {
		last := outs[len(outs)-1]
		if last.OutputType == "stream" && last.Name == o.Name {
			last.Text += o.Text
			return outs
		}
	}
	return append(outs, o)
}

func padRight(s string, w int) string {
	if d := w - lipgloss.Width(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
