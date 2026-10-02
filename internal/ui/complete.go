package ui

import (
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// Completion popup for insert mode. Items come from the kernel (live
// objects, so df. shows real columns) and, when running, the LSP.

const compMaxRows = 8

type compItem struct {
	label  string
	kind   string
	source string // kernel or lsp
}

type compState struct {
	items []compItem // everything the sources returned
	shown []compItem // filtered by what's typed since start
	sel   int
	row   int // editor row the completion is on
	start int // rune column where the replacement starts
}

type compMsg struct {
	seq    int
	source string
	row    int
	start  int
	items  []compItem
}

// requestCompletion asks the sources for completions at the cursor.
// Replies for anything but the latest request are dropped.
func (m *Model) requestCompletion() tea.Cmd {
	e := m.ed
	if e == nil || m.cell().Type != notebook.Code {
		return nil
	}
	m.compSeq++
	seq := m.compSeq
	code := e.text()
	cursor := runeOffset(e.lines, e.cur)
	row := e.cur.row
	var cmds []tea.Cmd
	if k := m.k; k != nil && m.kstate != "busy" {
		cmds = append(cmds, func() tea.Msg {
			r, err := k.Complete(code, cursor, 1500*time.Millisecond)
			if err != nil {
				return compMsg{seq: seq, source: "kernel"}
			}
			p := posAt(code, r.Start)
			msg := compMsg{seq: seq, source: "kernel", row: p.row, start: p.col}
			for _, it := range r.Items {
				msg.items = append(msg.items, compItem{label: it.Text, kind: it.Type, source: "kernel"})
			}
			return msg
		})
	}
	if c := m.lspCompletion(seq, row); c != nil {
		cmds = append(cmds, c)
	}
	return tea.Batch(cmds...)
}

func (m *Model) handleCompletion(msg compMsg) {
	e := m.ed
	if msg.seq != m.compSeq || e == nil || m.mode != editMode || e.mode != vInsert || msg.row != e.cur.row {
		return
	}
	if len(msg.items) == 0 {
		if m.comp != nil && m.comp.row == msg.row {
			m.filterComp()
		}
		return
	}
	// merge: kernel items first since they reflect what's actually in
	// memory, then anything new from the other source
	c := m.comp
	if c == nil || c.row != msg.row || c.start != msg.start || m.compFrom != msg.seq {
		c = &compState{row: msg.row, start: msg.start}
		m.compFrom = msg.seq
	}
	seen := map[string]bool{}
	var merged []compItem
	add := func(items []compItem) {
		for _, it := range items {
			if !seen[it.label] {
				seen[it.label] = true
				merged = append(merged, it)
			}
		}
	}
	if msg.source == "kernel" {
		add(msg.items)
		add(c.items)
	} else {
		var kernelItems, rest []compItem
		for _, it := range c.items {
			if it.source == "kernel" {
				kernelItems = append(kernelItems, it)
			} else {
				rest = append(rest, it)
			}
		}
		add(kernelItems)
		add(msg.items)
		add(rest)
	}
	c.items = merged
	m.comp = c
	m.filterComp()
}

// filterComp narrows the list to what's been typed since it opened.
func (m *Model) filterComp() {
	c, e := m.comp, m.ed
	if c == nil || e == nil {
		return
	}
	line := e.line(e.cur.row)
	if e.cur.row != c.row || e.cur.col < c.start || c.start > len(line) {
		m.comp = nil
		return
	}
	prefix := strings.ToLower(string(line[c.start:e.cur.col]))
	var prev string
	if c.sel < len(c.shown) {
		prev = c.shown[c.sel].label
	}
	c.shown = c.shown[:0]
	for _, it := range c.items {
		if strings.HasPrefix(strings.ToLower(it.label), prefix) {
			c.shown = append(c.shown, it)
		}
	}
	c.sel = 0
	for i, it := range c.shown {
		if it.label == prev {
			c.sel = i
		}
	}
	// nothing left, or the only match is already typed out
	if len(c.shown) == 0 || len(c.shown) == 1 && strings.EqualFold(c.shown[0].label, prefix) {
		m.comp = nil
	}
}

// compKey handles keys while the popup is open; false means not ours.
func (m *Model) compKey(key string) (bool, tea.Cmd) {
	c := m.comp
	n := len(c.shown)
	switch key {
	case "ctrl+n", "down", "tab":
		c.sel = (c.sel + 1) % n
	case "ctrl+p", "up", "shift+tab":
		c.sel = (c.sel - 1 + n) % n
	case "enter", "ctrl+y":
		m.acceptCompletion()
	case "esc":
		m.comp = nil
	default:
		return false, nil
	}
	return true, nil
}

func (m *Model) acceptCompletion() {
	c, e := m.comp, m.ed
	m.comp = nil
	if c == nil || e == nil || c.sel >= len(c.shown) {
		return
	}
	e.replaceInLine(c.row, c.start, e.cur.col, c.shown[c.sel].label)
	m.commitEdit()
}

// afterInsertKey decides whether typing tok should open or refresh
// the popup.
func (m *Model) afterInsertKey(tok string) tea.Cmd {
	e := m.ed
	if e == nil || e.mode != vInsert {
		m.comp = nil
		return nil
	}
	if m.comp != nil {
		m.filterComp()
	}
	switch {
	case tok == ".":
		return m.requestCompletion()
	case isIdentTok(tok):
		if identBefore(e) >= 2 {
			return m.requestCompletion()
		}
	case tok == "backspace":
	default:
		m.comp = nil
	}
	return nil
}

func isIdentTok(tok string) bool {
	r := []rune(tok)
	return len(r) == 1 && (r[0] == '_' || unicode.IsLetter(r[0]) || unicode.IsDigit(r[0]))
}

// identBefore is how many identifier characters sit right before the cursor.
func identBefore(e *editor) int {
	l := e.line(e.cur.row)
	n := 0
	for c := e.cur.col - 1; c >= 0 && (l[c] == '_' || unicode.IsLetter(l[c]) || unicode.IsDigit(l[c])); c-- {
		n++
	}
	return n
}

// wantsCompletion is true when tab should complete rather than indent.
func wantsCompletion(e *editor) bool {
	l := e.line(e.cur.row)
	if e.cur.col == 0 {
		return false
	}
	r := l[e.cur.col-1]
	return r == '.' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func runeOffset(lines [][]rune, p pos) int {
	n := 0
	for r := 0; r < p.row; r++ {
		n += len(lines[r]) + 1
	}
	return n + p.col
}

func posAt(code string, off int) pos {
	var p pos
	for i, r := range []rune(code) {
		if i == off {
			break
		}
		if r == '\n' {
			p.row++
			p.col = 0
		} else {
			p.col++
		}
	}
	return p
}

var kindShort = map[string]string{
	"function": "fn", "method": "fn", "module": "mod", "instance": "var", "variable": "var",
	"class": "class", "keyword": "kw", "statement": "stmt", "param": "param", "path": "path",
	"property": "prop", "field": "field", "constant": "const",
}

// renderCompletion draws the popup just under (or above) the cursor.
func (m *Model) renderCompletion(visible []string, curX, curY int) []string {
	c := m.comp
	if c == nil || len(c.shown) == 0 {
		return visible
	}
	labelW, kindW := 0, 0
	for _, it := range c.shown {
		labelW = max(labelW, ansi.StringWidth(it.label))
		kindW = max(kindW, len(kindShort[it.kind]))
	}
	labelW = min(labelW, 40)
	w := 1 + labelW + 2 + kindW + 1
	rows := min(len(c.shown), compMaxRows)
	top := max(0, min(c.sel-rows/2, len(c.shown)-rows))

	prefixW := 0
	if e := m.ed; e != nil && e.cur.col >= c.start {
		prefixW = dispCol(e.line(e.cur.row), e.cur.col) - dispCol(e.line(e.cur.row), c.start)
	}
	x := max(0, min(curX-prefixW, max(m.width, minWidth)-w))
	y := curY + 1
	if y+rows > len(visible) {
		y = curY - rows
	}
	if y < 0 {
		return visible
	}
	for i := range rows {
		it := c.shown[top+i]
		label := ansi.Truncate(it.label, labelW, "…")
		text := " " + padRight(label, labelW) + "  " + padRight(kindShort[it.kind], kindW) + " "
		st := m.st.popup
		if top+i == c.sel {
			st = m.st.selection
		}
		line := visible[y+i]
		if lw := ansi.StringWidth(line); lw < x {
			line += strings.Repeat(" ", x-lw)
		}
		visible[y+i] = ansi.Cut(line, 0, x) + st.Render(text) + ansi.Cut(line, x+w, ansi.StringWidth(line))
	}
	return visible
}
