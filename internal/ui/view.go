package ui

import (
	"fmt"
	"image/color"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/notebook"
)

const (
	gutter    = 10 // cell index + "[12]" prompt
	minWidth  = 30
	headerH   = 1
	footerH   = 1
	spinChars = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
)

type styles struct {
	accent, editAccent, faint lipgloss.Style
	header, footer            lipgloss.Style
	dim, prompt, stderr       lipgloss.Style
	errText, ok, busy         lipgloss.Style
	selection, flashMatch     lipgloss.Style
	flashLabel                lipgloss.Style
	chroma                    string
}

func (m *Model) applyTheme() {
	pick := func(dark, light string) lipgloss.Style {
		if m.dark {
			return lipgloss.NewStyle().Foreground(lipgloss.Color(dark))
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Color(light))
	}
	st := styles{
		accent:     pick("#7aa2f7", "#2e59c8"),
		editAccent: pick("#9ece6a", "#3d7a1a"),
		faint:      pick("#3b4261", "#c8cbd6"),
		dim:        pick("#737aa2", "#8a8fa8"),
		prompt:     pick("#737aa2", "#8a8fa8"),
		stderr:     pick("#e0af68", "#9a6700"),
		errText:    pick("#f7768e", "#c4314b"),
		ok:         pick("#9ece6a", "#3d7a1a"),
		busy:       pick("#e0af68", "#9a6700"),
		chroma:     "monokai",
	}
	if !m.dark {
		st.chroma = "friendly"
	}
	st.header = st.dim.Bold(true)
	st.footer = st.dim

	bg := func(dark, light string) color.Color {
		if m.dark {
			return lipgloss.Color(dark)
		}
		return lipgloss.Color(light)
	}
	st.selection = lipgloss.NewStyle().Background(bg("#364a82", "#b6c8f4"))
	st.flashMatch = lipgloss.NewStyle().Background(bg("#3d59a1", "#b6c8f4")).Foreground(bg("#c0caf5", "#1a1b26"))
	st.flashLabel = lipgloss.NewStyle().Background(bg("#ff007c", "#d20065")).Foreground(lipgloss.Color("#ffffff")).Bold(true)
	m.st = st

	m.hlCache = map[string]string{}
	m.mdCache = map[string]string{}
	m.mdr = nil
}

func (m *Model) bodyHeight() int { return max(m.height-headerH-footerH, 1) }

func (m *Model) boxWidth() int { return max(m.width, minWidth) - gutter - 1 }

func (m *Model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	body, cursor := m.renderBody()
	if m.help {
		body, cursor = m.renderHelp(), nil
	}
	if m.picker != nil {
		body, cursor = m.renderPicker(), nil
	}
	footer := m.renderFooter()
	if m.mode == cmdMode {
		footer = m.cmd.View()
		if c := m.cmd.Cursor(); c != nil {
			cc := *c
			cc.Y = m.height - 1
			cursor = &cc
		}
	}
	v := tea.NewView(m.renderHeader() + "\n" + body + "\n" + footer)
	v.AltScreen = true
	v.WindowTitle = "jupytui - " + filepath.Base(m.path)
	v.Cursor = cursor
	return v
}

func (m *Model) renderHeader() string {
	name := filepath.Base(m.path)
	if m.dirty {
		name += " [+]"
	}
	left := m.st.accent.Bold(true).Render(" jupytui ") + m.st.header.Render(name)

	var dot lipgloss.Style
	switch m.kstate {
	case "idle":
		dot = m.st.ok
	case "busy", "starting":
		dot = m.st.busy
	default:
		dot = m.st.errText
	}
	right := m.st.dim.Render(m.env.Label()+" ") + dot.Render("● "+m.kstate) + " "
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) renderFooter() string {
	badge, text := "NOTEBOOK", "enter edit · ctrl+enter run · s jump · : commands · ? help"
	st := m.st.accent
	if m.mode == editMode && m.ed != nil {
		st = m.st.editAccent
		switch m.ed.mode {
		case vInsert:
			badge, text = "INSERT", "esc normal · ctrl+enter run · ctrl+e nvim"
			if !m.ed.vim {
				badge, text = "EDIT", "esc leave · ctrl+enter run · ctrl+e nvim"
			}
		case vVisual:
			badge, text = "VISUAL", "d y c > < on selection · esc cancel"
		case vVisualLine:
			badge, text = "V-LINE", "d y c > < on selection · esc cancel"
		default:
			badge, text = "NORMAL", "esc leave cell · i insert · s jump · ctrl+enter run"
		}
	}
	if m.flash != nil {
		badge, text, st = "JUMP", "type to search, then a label · esc cancel", m.st.flashLabel
		text = "/" + m.flash.pattern + "  " + m.st.dim.Render(text)
	}
	left := st.Bold(true).Render(" " + badge + " ")
	if m.msg != "" && m.flash == nil {
		text = m.msg
	}
	pending := m.count
	if m.mode == editMode && m.ed != nil {
		pending = m.ed.pending()
	}
	pos := fmt.Sprintf(" %s  %d/%d ", pending, m.sel+1, len(m.nb.Cells))
	mid := m.st.footer.Render(" " + text)
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(mid)-len(pos), 1)
	return ansi.Truncate(left+mid+strings.Repeat(" ", gap)+m.st.dim.Render(pos), m.width, "")
}

// layoutRow records what one body line shows, so the cursor and flash
// labels can be placed on screen without re-deriving the wrapping.
type layoutRow struct {
	line  int // index into the body lines
	cell  int
	md    bool // rendered markdown line rather than source
	src   int  // source line
	dcol  int  // display column of the first char on this row
	textX int  // screen column where text starts
	width int  // text width of the row
}

// renderBody lays out every cell, scrolls so the selection (or the
// edit cursor) is visible, and returns the visible slice.
func (m *Model) renderBody() (string, *tea.Cursor) {
	var (
		lines     []string
		layout    []layoutRow
		selTop    int
		selBottom int
		bodyH     = m.bodyHeight()
		width     = max(m.width, minWidth)
	)
	m.starts = m.lineStarts()
	for i, c := range m.nb.Cells {
		if i == m.sel {
			selTop = len(lines)
		}
		cl, rows := m.renderCell(i, c)
		for _, r := range rows {
			r.line += len(lines)
			r.cell = i
			layout = append(layout, r)
		}
		lines = append(lines, cl...)
		if i == m.sel {
			selBottom = len(lines)
		}
		lines = append(lines, "")
	}

	var cur *tea.Cursor
	curLine := -1
	if m.mode == editMode && m.ed != nil {
		if r, x, ok := m.cursorRow(layout); ok {
			cur = tea.NewCursor(x, 0)
			curLine = r.line
			if m.ed.mode == vInsert {
				cur.Shape = tea.CursorBar
			}
		}
	}

	// keep the thing we care about on screen
	if curLine >= 0 {
		if curLine < m.offset {
			m.offset = curLine
		} else if curLine >= m.offset+bodyH {
			m.offset = curLine - bodyH + 1
		}
	} else if !m.manualScroll {
		if selBottom-selTop <= bodyH && selBottom > m.offset+bodyH {
			m.offset = selBottom - bodyH
		}
		if selTop < m.offset || selBottom-selTop > bodyH && selTop > m.offset {
			m.offset = selTop
		}
	}
	m.offset = max(0, min(m.offset, len(lines)-bodyH))
	m.layout = layout

	end := min(m.offset+bodyH, len(lines))
	visible := append([]string(nil), lines[m.offset:end]...)
	for len(visible) < bodyH {
		visible = append(visible, "")
	}
	for i, l := range visible {
		visible[i] = ansi.Truncate(l, width, "")
	}
	if m.flash != nil {
		visible = m.renderFlash(visible)
	}

	if cur != nil {
		cur.Y = headerH + curLine - m.offset
	}
	return strings.Join(visible, "\n"), cur
}

// cursorRow finds the layout row and screen x of the edit cursor.
func (m *Model) cursorRow(layout []layoutRow) (layoutRow, int, bool) {
	e := m.ed
	dc := dispCol(e.line(e.cur.row), e.cur.col)
	var hit layoutRow
	found := false
	for _, r := range layout {
		if r.cell != m.sel || r.md || r.src != e.cur.row {
			continue
		}
		// the last row of a line also takes the column just past the end
		if !found || dc >= r.dcol {
			hit, found = r, true
		}
		if dc < r.dcol+r.width {
			break
		}
	}
	if !found {
		return hit, 0, false
	}
	return hit, hit.textX + dc - hit.dcol, true
}

// renderCell returns the cell's lines and the layout of its text rows,
// with line indexes relative to the cell.
func (m *Model) renderCell(i int, c *notebook.Cell) ([]string, []layoutRow) {
	selected := i == m.sel
	editing := selected && m.mode == editMode && m.ed != nil
	boxW := m.boxWidth()

	border := m.st.faint
	if editing {
		border = m.st.editAccent
	} else if selected {
		border = m.st.accent
	}

	if c.Type == notebook.Markdown && !editing {
		return m.renderMarkdownCell(i, c, selected, boxW)
	}

	src := c.Source
	if editing {
		src = m.ed.text()
	}
	hl := src
	switch c.Type {
	case notebook.Code:
		hl = m.highlight(src, m.lang)
	case notebook.Markdown:
		hl = m.highlight(src, "markdown")
	}
	srcLines := strings.Split(src, "\n")
	hlLines := strings.Split(expandTabs(hl), "\n")
	for len(hlLines) < len(srcLines) {
		hlLines = append(hlLines, "")
	}

	// one width for the whole notebook so the columns line up
	numW := 0
	if m.number || m.relative {
		numW = max(2, len(itoa(m.starts[len(m.starts)-1]))) + 1
	}
	inner := max(boxW-4-numW, 4)

	var segs, nums []string
	var rows []layoutRow
	for r, src := range srcLines {
		runes := []rune(src)
		w := ansi.StringWidth(expandTabs(src))
		nseg := max(1, (w+inner-1)/inner)
		selFrom, selTo := m.selectedCols(editing, r, runes)
		for k := range nseg {
			seg := ansi.Cut(hlLines[r], k*inner, (k+1)*inner)
			if s, e := max(selFrom, k*inner)-k*inner, min(selTo, (k+1)*inner)-k*inner; e > s {
				part := ansi.Strip(ansi.Cut(seg, s, e))
				seg = ansi.Cut(seg, 0, s) + m.st.selection.Render(padRight(part, e-s)) + ansi.Cut(seg, e, inner)
			}
			segs = append(segs, seg)
			num := ""
			if numW > 0 && k == 0 {
				num = m.lineNumber(m.starts[i]+r, numW-1)
			} else if numW > 0 {
				num = strings.Repeat(" ", numW-1)
			}
			nums = append(nums, num)
			rows = append(rows, layoutRow{src: r, dcol: k * inner, width: inner})
		}
	}
	segs = carrySGR(segs)
	content := make([]string, len(segs))
	for j := range segs {
		if numW > 0 {
			content[j] = nums[j] + " " + segs[j]
		} else {
			content[j] = segs[j]
		}
	}

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border.GetForeground()).
		Padding(0, 1).
		Width(boxW).
		Render(strings.Join(content, "\n"))
	boxLines := strings.Split(box, "\n")

	out := make([]string, 0, len(boxLines)+len(c.Outputs))
	for j, l := range boxLines {
		g := strings.Repeat(" ", gutter)
		if j == 1 {
			g = m.prompt(i, c, selected)
		}
		out = append(out, g+l)
	}
	for j := range rows {
		rows[j].line = j + 1
		rows[j].textX = gutter + 2 + numW
	}

	if c.Type == notebook.Code {
		indent := strings.Repeat(" ", gutter+2)
		for _, l := range m.renderOutputs(c, boxW-2) {
			out = append(out, indent+l)
		}
	}
	return out, rows
}

// lineNumber is vim's number/relativenumber over the whole notebook as
// one buffer: g is the notebook-wide line, distances count from the
// cursor (or the selected cell's first line outside a cell).
func (m *Model) lineNumber(g, w int) string {
	if !m.relative {
		return m.st.faint.Render(fmt.Sprintf("%*d", w, g+1))
	}
	ref := m.starts[m.sel]
	if m.mode == editMode && m.ed != nil {
		ref += m.ed.cur.row
	}
	d := g - ref
	if d == 0 {
		if m.number {
			return m.st.accent.Render(fmt.Sprintf("%-*d", w, g+1))
		}
		return m.st.accent.Render(fmt.Sprintf("%*d", w, 0))
	}
	return m.st.faint.Render(fmt.Sprintf("%*d", w, max(d, -d)))
}

// selectedCols is the visual selection on source line r, as display
// columns [from, to).
func (m *Model) selectedCols(editing bool, r int, line []rune) (int, int) {
	if !editing {
		return 0, 0
	}
	a, b, lw, ok := m.ed.selection()
	if !ok || r < a.row || r > b.row {
		return 0, 0
	}
	w := dispCol(line, len(line))
	if lw {
		return 0, max(w, 1)
	}
	from, to := 0, w+1
	if r == a.row {
		from = dispCol(line, a.col)
	}
	if r == b.row {
		to = dispCol(line, min(b.col+1, len(line)))
		if b.col >= len(line) {
			to = w + 1
		}
	}
	return from, max(to, from+1)
}

// dispCol is the display width of line[:col] as drawn (tabs expanded).
func dispCol(line []rune, col int) int {
	return ansi.StringWidth(expandTabs(string(line[:min(col, len(line))])))
}

func (m *Model) prompt(i int, c *notebook.Cell, selected bool) string {
	var p string
	switch {
	case c.Type != notebook.Code:
		p = ""
	case m.runs[c] == running && m.isRunning(c):
		r := []rune(spinChars)
		p = "[" + string(r[m.frame%len(r)]) + "]"
	case m.isRunning(c):
		p = "[*]"
	case c.ExecutionCount != nil:
		p = fmt.Sprintf("[%d]", *c.ExecutionCount)
	default:
		p = "[ ]"
	}
	st := m.st.prompt
	if selected {
		st = m.st.accent
	}
	return m.cellIndex(i) + " " + st.Render(fmt.Sprintf("%5s ", p))
}

// cellIndex works like relativenumber for cells: distance from the
// selected cell, so 3j lands on the cell marked 3.
func (m *Model) cellIndex(i int) string {
	if i == m.sel {
		return m.st.accent.Render(fmt.Sprintf("%-3d", i+1))
	}
	d := i - m.sel
	return m.st.faint.Render(fmt.Sprintf("%3d", max(d, -d)))
}

func (m *Model) isRunning(c *notebook.Cell) bool {
	_, ok := m.runs[c]
	return ok
}

func (m *Model) renderMarkdownCell(i int, c *notebook.Cell, selected bool, boxW int) ([]string, []layoutRow) {
	bar := m.st.faint.Render("▏")
	if selected {
		bar = m.st.accent.Render("▌")
	}
	// same number column as code cells; rendered markdown doesn't map to
	// source lines, so only the cell's first line gets a number
	numW := 0
	if m.number || m.relative {
		numW = max(2, len(itoa(m.starts[len(m.starts)-1]))) + 1
	}
	src := strings.TrimSpace(c.Source)
	var body string
	if src == "" {
		body = m.st.dim.Render("empty markdown cell")
	} else {
		body = m.markdown(src, boxW-2-numW)
	}
	var out []string
	var rows []layoutRow
	for j, l := range strings.Split(body, "\n") {
		g := strings.Repeat(" ", gutter)
		num := strings.Repeat(" ", numW)
		if j == 0 {
			g = m.cellIndex(i) + strings.Repeat(" ", gutter-3)
			if numW > 0 {
				num = m.lineNumber(m.starts[i], numW-1) + " "
			}
		}
		out = append(out, g+bar+" "+num+l)
		rows = append(rows, layoutRow{line: j, md: true, textX: gutter + 2 + numW, width: boxW - 2 - numW})
	}
	return out, rows
}

func (m *Model) highlight(src, lang string) string {
	if src == "" {
		return ""
	}
	key := lang + "\x00" + src
	if s, ok := m.hlCache[key]; ok {
		return s
	}
	var b strings.Builder
	if err := quick.Highlight(&b, src, lang, "terminal256", m.st.chroma); err != nil {
		return src
	}
	// lexers may add or eat a trailing newline; the line count has to
	// match the source or the edit cursor lands on the wrong row
	want := strings.Count(src, "\n") + 1
	lines := strings.Split(b.String(), "\n")
	for len(lines) < want {
		lines = append(lines, "")
	}
	s := strings.Join(lines[:want], "\n")
	// typing makes a new entry per keystroke, don't let that pile up
	if len(m.hlCache) > 1000 {
		m.hlCache = map[string]string{}
	}
	m.hlCache[key] = s
	return s
}

// carrySGR re-opens a colour that's still active at the end of a line on
// the next one. Chroma emits one escape per token, so multi-line strings
// and wrapped lines would otherwise lose their colour after line one.
func carrySGR(lines []string) []string {
	active := ""
	for i, l := range lines {
		if active != "" {
			l = active + l
		}
		for j := 0; j < len(l); {
			k := strings.Index(l[j:], "\x1b[")
			if k < 0 {
				break
			}
			start := j + k
			end := strings.IndexByte(l[start:], 'm')
			if end < 0 {
				break
			}
			seq := l[start : start+end+1]
			if seq == "\x1b[0m" || seq == "\x1b[m" {
				active = ""
			} else {
				active = seq
			}
			j = start + end + 1
		}
		if active != "" {
			l += "\x1b[0m"
		}
		lines[i] = l
	}
	return lines
}

// markdown renders with glamour and trims the blank margins it adds.
func (m *Model) markdown(src string, width int) string {
	key := fmt.Sprintf("%d\x00%s", width, src)
	if s, ok := m.mdCache[key]; ok {
		return s
	}
	if m.mdr == nil || m.mdrW != width {
		style := "dark"
		if !m.dark {
			style = "light"
		}
		r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width))
		if err != nil {
			return src
		}
		m.mdr, m.mdrW = r, width
	}
	out, err := m.mdr.Render(src)
	if err != nil {
		return src
	}
	lines := strings.Split(out, "\n")
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[0])) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		lines = lines[:len(lines)-1]
	}
	s := strings.Join(lines, "\n")
	m.mdCache[key] = s
	return s
}

var helpText = [][2]string{
	{"j k  5j 3k", "move between cells (counts match the gutter)"},
	{"gg G  12G", "first / last / nth cell"},
	{"ctrl+d ctrl+u", "scroll half a page"},
	{"enter / i / A", "open cell in vim normal / insert / append"},
	{"esc", "insert -> normal -> back to the cell list"},
	{"s", "flash jump: type, then the label"},
	{"e  (ctrl+e in cell)", "edit cell in $EDITOR or host nvim"},
	{"ctrl+enter", "run cell"},
	{"shift+enter ctrl+r", "run cell, move to next"},
	{"ctrl+c", "interrupt (or quit when idle)"},
	{"o O", "new cell below / above"},
	{"dd u", "delete cell / undo delete"},
	{"yy p P", "yank / paste below / paste above"},
	{"J K", "move cell down / up"},
	{"M C R", "make markdown / code / raw"},
	{"x", "clear cell output"},
	{"ctrl+s :w", "save"},
	{"q :q :q! :wq", "quit"},
	{":runall :clear", "run all / clear all outputs"},
	{":export[!] [file.py]", "write a # %% percent .py"},
	{":restart :interrupt", "kernel control"},
	{":env", "pick the python environment"},
	{":<n>", "jump to cell n"},
	{":set [no]nu [no]rnu", "line numbers / relative numbers"},
	{":set [no]vim", "vim editing inside cells"},
}

func (m *Model) renderHelp() string {
	var b strings.Builder
	for _, h := range helpText {
		b.WriteString(m.st.accent.Render(padRight(h[0], 22)) + m.st.dim.Render(h[1]) + "\n")
	}
	b.WriteString("\n" + m.st.dim.Render("press any key"))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.st.accent.GetForeground()).
		Padding(1, 2).
		Render(b.String())
	return lipgloss.Place(max(m.width, minWidth), m.bodyHeight(), lipgloss.Center, lipgloss.Center, box)
}
