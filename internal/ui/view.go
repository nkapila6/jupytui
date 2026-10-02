package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/notebook"
)

const (
	gutter    = 7 // width of the "[12]" prompt column
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
	m.st = st

	ts := textarea.DefaultStyles(m.dark)
	ts.Focused.CursorLine = ts.Focused.Text
	ts.Focused.Base = lipgloss.NewStyle()
	ts.Blurred.Base = lipgloss.NewStyle()
	m.ta.SetStyles(ts)

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
	right := m.st.dim.Render(m.nb.KernelName()+" ") + dot.Render("● "+m.kstate) + " "
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return left + strings.Repeat(" ", gap) + right
}

func (m *Model) renderFooter() string {
	var left string
	if m.mode == editMode {
		left = m.st.editAccent.Bold(true).Render(" EDIT ")
	} else {
		left = m.st.accent.Bold(true).Render(" NORMAL ")
	}
	text := m.msg
	if text == "" {
		if m.mode == editMode {
			text = "esc normal · ctrl+r run · ctrl+e $EDITOR · ctrl+s save"
		} else {
			text = "enter edit · ctrl+r run · : commands · ? help"
		}
	}
	pos := fmt.Sprintf(" %d/%d ", m.sel+1, len(m.nb.Cells))
	mid := m.st.footer.Render(" " + text)
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(mid)-len(pos), 1)
	return ansi.Truncate(left+mid+strings.Repeat(" ", gap)+m.st.dim.Render(pos), m.width, "")
}

// renderBody lays out every cell, scrolls so the selection (or the
// edit cursor) is visible, and returns the visible slice.
func (m *Model) renderBody() (string, *tea.Cursor) {
	var (
		lines     []string
		selTop    int
		selBottom int
		editTop   = -1
		bodyH     = m.bodyHeight()
		width     = max(m.width, minWidth)
	)
	for i, c := range m.nb.Cells {
		if i == m.sel {
			selTop = len(lines)
		}
		cl, edit := m.renderCell(i, c)
		if edit >= 0 {
			editTop = len(lines) + edit
		}
		lines = append(lines, cl...)
		if i == m.sel {
			selBottom = len(lines)
		}
		lines = append(lines, "")
	}

	var cur *tea.Cursor
	var curLine int
	if m.mode == editMode && editTop >= 0 {
		row, x := m.editCursor()
		cur = tea.NewCursor(x, 0)
		curLine = editTop + row
	}

	// keep the thing we care about on screen
	if cur != nil {
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

	end := min(m.offset+bodyH, len(lines))
	visible := lines[m.offset:end]
	for len(visible) < bodyH {
		visible = append(visible, "")
	}
	for i, l := range visible {
		visible[i] = ansi.Truncate(l, width, "")
	}

	if cur != nil {
		c := *cur
		c.X += gutter + 2
		c.Y = headerH + curLine - m.offset
		cur = &c
	}
	return strings.Join(visible, "\n"), cur
}

// renderCell returns the cell's lines and, if it holds the editor, the
// line index where the editor starts (else -1).
func (m *Model) renderCell(i int, c *notebook.Cell) ([]string, int) {
	selected := i == m.sel
	editing := selected && m.mode == editMode
	boxW := m.boxWidth()
	inner := boxW - 4

	border := m.st.faint
	if editing {
		border = m.st.editAccent
	} else if selected {
		border = m.st.accent
	}

	if c.Type == notebook.Markdown && !editing {
		return m.renderMarkdownCell(c, selected, boxW), -1
	}

	// the textarea only handles input; edit mode draws through the same
	// highlighter so it looks the same as normal mode, see editCursor
	src := c.Source
	if editing {
		src = m.ta.Value()
	}
	var content string
	switch c.Type {
	case notebook.Code:
		content = m.highlight(src, m.lang)
	case notebook.Markdown:
		content = m.highlight(src, "markdown")
	default:
		content = src
	}
	var wrapped []string
	for _, l := range strings.Split(expandTabs(content), "\n") {
		wrapped = append(wrapped, strings.Split(ansi.Hardwrap(l, inner, true), "\n")...)
	}
	wrapped = carrySGR(wrapped)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border.GetForeground()).
		Padding(0, 1).
		Width(boxW).
		Render(strings.Join(wrapped, "\n"))
	boxLines := strings.Split(box, "\n")

	pad := strings.Repeat(" ", gutter)
	out := make([]string, 0, len(boxLines)+len(c.Outputs))
	for j, l := range boxLines {
		g := pad
		if j == 1 {
			g = m.prompt(c, selected)
		}
		out = append(out, g+l)
	}

	if c.Type == notebook.Code {
		indent := strings.Repeat(" ", gutter+2)
		for _, l := range m.renderOutputs(c, boxW-2) {
			out = append(out, indent+l)
		}
	}
	edit := -1
	if editing {
		edit = 1
	}
	return out, edit
}

func (m *Model) prompt(c *notebook.Cell, selected bool) string {
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
	return st.Render(fmt.Sprintf("%*s ", gutter-1, p))
}

func (m *Model) isRunning(c *notebook.Cell) bool {
	_, ok := m.runs[c]
	return ok
}

func (m *Model) renderMarkdownCell(c *notebook.Cell, selected bool, boxW int) []string {
	bar := m.st.faint.Render("▏")
	if selected {
		bar = m.st.accent.Render("▌")
	}
	src := strings.TrimSpace(c.Source)
	var body string
	if src == "" {
		body = m.st.dim.Render("empty markdown cell")
	} else {
		body = m.markdown(src, boxW-2)
	}
	pad := strings.Repeat(" ", gutter)
	var out []string
	for _, l := range strings.Split(body, "\n") {
		out = append(out, pad+bar+" "+l)
	}
	return out
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

// editCursor maps the textarea's logical line/column onto the
// hard-wrapped rows renderCell draws, as (row, x) inside the box.
func (m *Model) editCursor() (int, int) {
	inner := max(m.boxWidth()-4, 1)
	lines := strings.Split(m.ta.Value(), "\n")
	line := min(m.ta.Line(), len(lines)-1)
	row := 0
	for _, l := range lines[:line] {
		row += max(1, (ansi.StringWidth(expandTabs(l))+inner-1)/inner)
	}
	runes := []rune(lines[line])
	col := min(m.ta.Column(), len(runes))
	w := ansi.StringWidth(expandTabs(string(runes[:col])))
	full := ansi.StringWidth(expandTabs(lines[line]))
	// cursor right after a line that exactly fills its last row stays on
	// that row instead of jumping to a row that isn't drawn
	if w > 0 && w%inner == 0 && w == full {
		return row + w/inner - 1, inner
	}
	return row + w/inner, w % inner
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
	{"j k / arrows", "move between cells"},
	{"gg G", "first / last cell"},
	{"ctrl+d ctrl+u", "scroll half a page"},
	{"enter i / A", "edit cell (cursor at start / end)"},
	{"e  (ctrl+e in edit)", "edit cell in $EDITOR or host nvim"},
	{"esc", "back to normal mode"},
	{"ctrl+r shift+enter", "run cell, move to next"},
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
	{":restart :interrupt", "kernel control"},
	{":<n>", "jump to cell n"},
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
