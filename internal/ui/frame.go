package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Full-screen dataframe viewer. Rows come from the kernel a page at a
// time (sorted and filtered there), so a 10M row frame is as quick to
// browse as a small one.

const framePage = 200

type framePageData struct {
	Type   string     `json:"type"`
	Cols   []string   `json:"cols"`
	Dtypes []string   `json:"dtypes"`
	Total  int        `json:"total"`
	Rows   [][]string `json:"rows"`
	Index  string     `json:"index"`
}

type frameView struct {
	name    string
	data    framePageData
	pageOff int // absolute row of data.Rows[0]
	row     int // cursor row (absolute) and column
	col     int
	top     int // first visible row
	left    int // first visible column
	sortCol int // -1 for none
	asc     bool
	filter  string
	input   textinput.Model
	editing bool // typing a filter
	loading bool
	err     string
	detail  string
	seq     int
}

type frameMsg struct {
	seq  int
	off  int
	page framePageData
	err  error
}

func (m *Model) openFrame(name string) tea.Cmd {
	if m.k == nil {
		m.msg = "kernel isn't running"
		return nil
	}
	f := &frameView{name: name, sortCol: -1, asc: true}
	f.input = textinput.New()
	f.input.Prompt = "/"
	f.input.SetVirtualCursor(false)
	m.dfv = f
	return m.fetchFrame(0)
}

func (m *Model) fetchFrame(off int) tea.Cmd {
	f, k := m.dfv, m.k
	if f == nil || k == nil {
		return nil
	}
	f.seq++
	f.loading = true
	seq := f.seq
	var sortCol any
	if f.sortCol >= 0 {
		sortCol = f.sortCol
	}
	params := map[string]any{"name": f.name, "offset": off, "limit": framePage, "filter": f.filter, "sort": sortCol, "asc": f.asc}
	return func() tea.Msg {
		out, err := k.Eval(pyCall(framePy, params), 60*time.Second)
		var page framePageData
		if err == nil {
			err = json.Unmarshal([]byte(lastLine(out)), &page)
		}
		return frameMsg{seq: seq, off: off, page: page, err: err}
	}
}

func (m *Model) handleFrame(msg frameMsg) {
	f := m.dfv
	if f == nil || msg.seq != f.seq {
		return
	}
	f.loading = false
	if msg.err != nil {
		f.err = msg.err.Error()
		return
	}
	f.err = ""
	f.data, f.pageOff = msg.page, msg.off
	f.row = min(f.row, max(f.data.Total-1, 0))
	f.col = min(f.col, max(len(f.data.Cols)-1, 0))
}

// visibleRows is how many data rows fit under the title and headers.
func (m *Model) frameRows() int { return max(m.bodyHeight()-6, 1) }

// ensureRows refetches when the visible window leaves the loaded page.
func (m *Model) ensureRows() tea.Cmd {
	f := m.dfv
	n := m.frameRows()
	if f.row < f.top {
		f.top = f.row
	} else if f.row >= f.top+n {
		f.top = f.row - n + 1
	}
	if f.loading {
		return nil
	}
	if f.top < f.pageOff || f.top+n > f.pageOff+len(f.data.Rows) && f.pageOff+len(f.data.Rows) < f.data.Total {
		return m.fetchFrame(max(0, f.top-framePage/4))
	}
	return nil
}

func (m *Model) frameKey(msg tea.KeyPressMsg) tea.Cmd {
	f := m.dfv
	if f.editing {
		switch msg.String() {
		case "esc":
			f.editing = false
			f.input.Blur()
		case "enter":
			f.editing = false
			f.input.Blur()
			f.filter = strings.TrimSpace(f.input.Value())
			f.row, f.top = 0, 0
			return m.fetchFrame(0)
		default:
			var cmd tea.Cmd
			f.input, cmd = f.input.Update(msg)
			return cmd
		}
		return nil
	}
	if f.detail != "" {
		f.detail = ""
		return nil
	}
	last := max(f.data.Total-1, 0)
	half := max(m.frameRows()/2, 1)
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.dfv = nil
		return nil
	case "j", "down":
		f.row = min(f.row+1, last)
	case "k", "up":
		f.row = max(f.row-1, 0)
	case "ctrl+d", "pgdown":
		f.row = min(f.row+half, last)
	case "ctrl+u", "pgup":
		f.row = max(f.row-half, 0)
	case "g", "home":
		f.row = 0
	case "G", "end":
		f.row = last
	case "l", "right":
		f.col = min(f.col+1, max(len(f.data.Cols)-1, 0))
	case "h", "left":
		f.col = max(f.col-1, 0)
	case "0":
		f.col = 0
	case "$":
		f.col = max(len(f.data.Cols)-1, 0)
	case "s":
		// cycle: ascending, descending, unsorted
		switch {
		case f.sortCol != f.col:
			f.sortCol, f.asc = f.col, true
		case f.asc:
			f.asc = false
		default:
			f.sortCol = -1
		}
		f.row, f.top = 0, 0
		return m.fetchFrame(0)
	case "/":
		f.editing = true
		f.input.SetValue(f.filter)
		f.input.CursorEnd()
		return f.input.Focus()
	case "enter":
		if r := f.row - f.pageOff; r >= 0 && r < len(f.data.Rows) && f.col+1 < len(f.data.Rows[r]) {
			f.detail = f.data.Rows[r][f.col+1]
		}
		return nil
	case "r":
		return m.fetchFrame(f.pageOff)
	}
	return m.ensureRows()
}

func numericDtype(d string) bool {
	d = strings.ToLower(d)
	for _, k := range []string{"int", "float", "decimal", "f32", "f64", "i64", "i32", "u8", "u16", "u32", "u64", "complex"} {
		if strings.Contains(d, k) {
			return true
		}
	}
	return false
}

func (m *Model) renderFrame() string {
	f := m.dfv
	width := max(m.width, minWidth)
	bodyH := m.bodyHeight()
	d := f.data

	title := m.st.accent.Bold(true).Render(f.name)
	info := []string{d.Type, fmt.Sprintf("%d rows × %d cols", d.Total, len(d.Cols))}
	if f.sortCol >= 0 && f.sortCol < len(d.Cols) {
		arrow := "↑"
		if !f.asc {
			arrow = "↓"
		}
		info = append(info, "sorted by "+d.Cols[f.sortCol]+" "+arrow)
	}
	if f.filter != "" {
		info = append(info, fmt.Sprintf("filter %q", f.filter))
	}
	if f.loading {
		info = append(info, "loading…")
	}
	lines := []string{" " + title + "  " + m.st.dim.Render(strings.Join(info, " · ")), ""}
	if f.err != "" {
		lines = append(lines, " "+m.st.errText.Render(f.err))
	}

	// widths from what's loaded
	idxW := max(ansi.StringWidth(d.Index), 1)
	colW := make([]int, len(d.Cols))
	for c := range d.Cols {
		colW[c] = ansi.StringWidth(d.Cols[c])
		if c < len(d.Dtypes) {
			colW[c] = max(colW[c], ansi.StringWidth(d.Dtypes[c]))
		}
	}
	for _, r := range d.Rows {
		if len(r) > 0 {
			idxW = max(idxW, ansi.StringWidth(r[0]))
		}
		for c := 1; c < len(r) && c-1 < len(colW); c++ {
			colW[c-1] = max(colW[c-1], ansi.StringWidth(r[c]))
		}
	}
	idxW = min(idxW, 14)
	for c := range colW {
		colW[c] = max(min(colW[c], 28), 3)
	}

	// horizontal window that keeps the cursor column on screen
	fits := func(left int) int {
		used, n := idxW+3, 0
		for c := left; c < len(colW) && used+colW[c]+3 <= width; c++ {
			used += colW[c] + 3
			n++
		}
		return max(n, 1)
	}
	if f.col < f.left {
		f.left = f.col
	}
	for f.col >= f.left+fits(f.left) && f.left < f.col {
		f.left++
	}
	shown := min(fits(f.left), len(colW)-f.left)
	if len(colW) == 0 {
		shown = 0
	}

	cell := func(text string, w int, right bool) string {
		text = ansi.Truncate(text, w, "…")
		pad := strings.Repeat(" ", w-ansi.StringWidth(text))
		if right {
			return pad + text
		}
		return text + pad
	}
	header := " " + m.st.dim.Render(cell(d.Index, idxW, false)) + " │"
	types := " " + strings.Repeat(" ", idxW) + " │"
	for c := f.left; c < f.left+shown; c++ {
		name := cell(d.Cols[c], colW[c], false)
		if c == f.col {
			name = m.st.accent.Bold(true).Render(name)
		} else {
			name = lipglossBold(name)
		}
		header += " " + name + " │"
		dt := ""
		if c < len(d.Dtypes) {
			dt = d.Dtypes[c]
		}
		types += " " + m.st.dim.Render(cell(dt, colW[c], false)) + " │"
	}
	if f.left+shown < len(colW) {
		header += m.st.dim.Render(" …")
	}
	lines = append(lines, header, types, " "+m.st.faint.Render(strings.Repeat("─", min(width-2, idxW+3+sumPlus(colW[f.left:f.left+shown], 3)))))

	n := m.frameRows()
	for i := range n {
		abs := f.top + i
		if abs >= d.Total {
			break
		}
		r := abs - f.pageOff
		if r < 0 || r >= len(d.Rows) {
			lines = append(lines, " "+m.st.dim.Render("…"))
			continue
		}
		row := d.Rows[r]
		line := " " + m.st.dim.Render(cell(row[0], idxW, true)) + " " + m.st.faint.Render("│")
		for c := f.left; c < f.left+shown; c++ {
			v := ""
			if c+1 < len(row) {
				v = row[c+1]
			}
			numeric := c < len(d.Dtypes) && numericDtype(d.Dtypes[c])
			text := " " + cell(v, colW[c], numeric) + " "
			switch {
			case abs == f.row && c == f.col:
				text = m.st.selection.Render(text)
			case abs == f.row:
				text = m.st.popup.Render(text)
			}
			line += text + m.st.faint.Render("│")
		}
		lines = append(lines, line)
	}

	for len(lines) < bodyH-1 {
		lines = append(lines, "")
	}
	lines = lines[:bodyH-1]
	status := fmt.Sprintf(" row %d/%d", min(f.row+1, d.Total), d.Total)
	if f.col < len(d.Cols) {
		status += " · " + d.Cols[f.col]
	}
	status += "  " + m.st.dim.Render("hjkl move · s sort · / filter · enter value · q close")
	if f.editing {
		status = " " + f.input.View()
	}
	lines = append(lines, status)
	out := strings.Join(lines, "\n")
	if f.detail != "" {
		out = m.overlayText(out, f.detail)
	}
	return out
}

func sumPlus(ws []int, extra int) int {
	t := 0
	for _, w := range ws {
		t += w + extra
	}
	return t
}

// overlayText shows a wrapped text box in the middle of the body.
func (m *Model) overlayText(base, text string) string {
	w := min(max(m.width-10, 20), 90)
	body := ansi.Wordwrap(text, w-4, " ,")
	bl := strings.Split(body, "\n")
	if len(bl) > m.bodyHeight()-6 {
		bl = append(bl[:max(m.bodyHeight()-7, 1)], "…")
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(m.st.accent.GetForeground()).Padding(0, 1).Render(strings.Join(bl, "\n") + "\n\n" + m.st.dim.Render("any key to close"))
	return lipgloss.Place(max(m.width, minWidth), m.bodyHeight(), lipgloss.Center, lipgloss.Center, box)
}

func (m *Model) renderVars() string {
	v := m.vars
	width := max(m.width, minWidth)
	lines := []string{" " + m.st.accent.Bold(true).Render("Variables") + "  " + m.st.dim.Render(fmt.Sprintf("%d in the kernel", len(v.list))), ""}
	if v.loading && len(v.list) == 0 {
		lines = append(lines, " "+m.st.dim.Render("asking the kernel…"))
	}
	if v.err != "" {
		lines = append(lines, " "+m.st.errText.Render(v.err))
	}
	nameW, typeW, shapeW := 4, 4, 5
	for _, it := range v.list {
		nameW = max(nameW, ansi.StringWidth(it.Name))
		typeW = max(typeW, ansi.StringWidth(it.Type))
		shapeW = max(shapeW, ansi.StringWidth(it.Shape))
	}
	nameW, typeW, shapeW = min(nameW, 24), min(typeW, 18), min(shapeW, 16)
	pad := func(s string, w int) string { return padRight(ansi.Truncate(s, w, "…"), w) }
	lines = append(lines, " "+lipglossBold(pad("name", nameW)+"  "+pad("type", typeW)+"  "+pad("shape", shapeW)+"  "+pad("size", 9)+"  value"))
	n := m.bodyHeight() - 5
	top := max(0, min(v.sel-n/2, len(v.list)-n))
	for i := top; i < len(v.list) && i < top+n; i++ {
		it := v.list[i]
		prevW := max(width-nameW-typeW-shapeW-9-12, 10)
		line := pad(it.Name, nameW) + "  " + pad(it.Type, typeW) + "  " + pad(it.Shape, shapeW) + "  " + pad(humanSize(it.Size), 9) + "  " + ansi.Truncate(it.Repr, prevW, "…")
		if i == v.sel {
			line = m.st.selection.Render(padRight(line, width-2))
		} else if it.Kind == "frame" {
			line = m.st.accent.Render(line)
		}
		lines = append(lines, " "+line)
	}
	for len(lines) < m.bodyHeight()-1 {
		lines = append(lines, "")
	}
	lines = lines[:m.bodyHeight()-1]
	lines = append(lines, " "+m.st.dim.Render("j/k move · enter open (tables open the viewer) · r refresh · q close"))
	out := strings.Join(lines, "\n")
	if v.detail != "" {
		out = m.overlayText(out, v.detail)
	}
	return out
}
