package ui

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// Notebook-wide search (/ n N *), :s substitute, output folding and an
// outline of headings and definitions.

type match struct {
	cell     int
	row, col int // rune column
	n        int // length in runes
}

func (m *Model) searchRe(pat string) (*regexp.Regexp, error) {
	q := regexp.QuoteMeta(pat)
	// smartcase: all lowercase means case-insensitive
	if strings.ToLower(pat) == pat {
		q = "(?i)" + q
	}
	return regexp.Compile(q)
}

// matches finds every occurrence of the search in cell sources, using
// the live editor text for the open cell.
func (m *Model) matches(pat string) []match {
	re, err := m.searchRe(pat)
	if err != nil || pat == "" {
		return nil
	}
	var out []match
	for i, c := range m.nb.Cells {
		src := c.Source
		if m.mode == editMode && m.ed != nil && i == m.sel {
			src = m.ed.text()
		}
		for r, line := range strings.Split(src, "\n") {
			for _, loc := range re.FindAllStringIndex(line, -1) {
				if loc[1] == loc[0] {
					continue
				}
				out = append(out, match{
					cell: i, row: r,
					col: len([]rune(line[:loc[0]])),
					n:   len([]rune(line[loc[0]:loc[1]])),
				})
			}
		}
	}
	return out
}

func (m *Model) openSearch() tea.Cmd {
	if m.mode == editMode {
		m.commitEdit()
	}
	m.searchFrom = m.mode
	m.searching = true
	m.searchIn.Reset()
	m.searchIn.SetWidth(max(m.width-4, 10))
	return m.searchIn.Focus()
}

func (m *Model) searchKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.searching = false
		m.searchIn.Blur()
		return nil
	case "enter":
		m.searching = false
		m.searchIn.Blur()
		if v := m.searchIn.Value(); v != "" {
			m.search = v
		}
		m.hlsearch = true
		m.searchNext(1)
		return nil
	}
	var cmd tea.Cmd
	m.searchIn, cmd = m.searchIn.Update(msg)
	return cmd
}

// searchNext jumps to the next (dir 1) or previous (dir -1) match,
// wrapping around the notebook.
func (m *Model) searchNext(dir int) {
	ms := m.matches(m.search)
	if len(ms) == 0 {
		m.msg = "not found: " + m.search
		return
	}
	cell, row, col := m.sel, 0, -1
	if m.mode == editMode && m.ed != nil {
		row, col = m.ed.cur.row, m.ed.cur.col
	}
	after := func(x match) bool {
		return x.cell > cell || x.cell == cell && (x.row > row || x.row == row && x.col > col)
	}
	before := func(x match) bool {
		return x.cell < cell || x.cell == cell && (x.row < row || x.row == row && x.col < col)
	}
	pick, idx := -1, 0
	if dir > 0 {
		for i, x := range ms {
			if after(x) {
				pick = i
				break
			}
		}
		if pick < 0 {
			pick = 0
		}
	} else {
		for i := len(ms) - 1; i >= 0; i-- {
			if before(ms[i]) {
				pick = i
				break
			}
		}
		if pick < 0 {
			pick = len(ms) - 1
		}
	}
	idx = pick
	x := ms[pick]
	m.gotoLine(m.lineStarts()[x.cell]+x.row, x.col)
	m.msg = fmt.Sprintf("[%d/%d] /%s", idx+1, len(ms), m.search)
}

// searchWord is *: search the word under the cursor.
func (m *Model) searchWord() {
	e := m.ed
	if e == nil {
		return
	}
	l := e.line(e.cur.row)
	s, t := e.cur.col, e.cur.col
	for s > 0 && cls(l[s-1], false) == 1 {
		s--
	}
	for t < len(l) && cls(l[t], false) == 1 {
		t++
	}
	if t <= s {
		return
	}
	m.search = string(l[s:t])
	m.hlsearch = true
	m.searchNext(1)
}

// searchCols are the display ranges on a source line to highlight.
func (m *Model) searchCols(line []rune) [][2]int {
	if !m.hlsearch || m.search == "" {
		return nil
	}
	re, err := m.searchRe(m.search)
	if err != nil {
		return nil
	}
	s := string(line)
	var out [][2]int
	for _, loc := range re.FindAllStringIndex(s, -1) {
		a := len([]rune(s[:loc[0]]))
		b := len([]rune(s[:loc[1]]))
		out = append(out, [2]int{dispCol(line, a), dispCol(line, b)})
	}
	return out
}

// highlightRanges paints ranges of display columns in a segment that
// starts at display column off.
func highlightRanges(seg string, ranges [][2]int, off, w int, paint func(string) string) string {
	for _, r := range ranges {
		s, e := max(r[0], off)-off, min(r[1], off+w)-off
		if e <= s {
			continue
		}
		part := ansi.Strip(ansi.Cut(seg, s, e))
		seg = ansi.Cut(seg, 0, s) + paint(part) + ansi.Cut(seg, e, ansi.StringWidth(seg))
	}
	return seg
}

// ---- :s ----

var subRe = regexp.MustCompile(`^(%?)s(.)`)

// substitute handles :s/pat/rep/flags (current line, or the whole cell
// from the notebook view) and :%s/... (every cell).
func (m *Model) substitute(line string) bool {
	h := subRe.FindStringSubmatch(line)
	if h == nil || strings.ContainsRune(`\ "|`, rune(h[2][0])) || h[2][0] >= 'a' && h[2][0] <= 'z' {
		return false
	}
	all := h[1] == "%"
	parts := splitDelim(line[len(h[0]):], h[2][0])
	if len(parts) < 2 {
		m.msg = "usage: :s/pattern/replacement/[gi]"
		return true
	}
	pat, rep, flags := parts[0], parts[1], ""
	if len(parts) > 2 {
		flags = parts[2]
	}
	pat = strings.NewReplacer(`\<`, `\b`, `\>`, `\b`).Replace(pat)
	if strings.Contains(flags, "i") {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		m.msg = "bad pattern: " + err.Error()
		return true
	}
	rep = vimReplacement(rep)
	global := strings.Contains(flags, "g")

	count, cells := 0, 0
	sub := func(s string) string {
		if global {
			n := len(re.FindAllStringIndex(s, -1))
			count += n
			return re.ReplaceAllString(s, rep)
		}
		loc := re.FindStringSubmatchIndex(s)
		if loc == nil {
			return s
		}
		count++
		var dst []byte
		dst = re.ExpandString(dst, rep, s, loc)
		return s[:loc[0]] + string(dst) + s[loc[1]:]
	}
	apply := func(i int, c *notebook.Cell, onlyRow int) {
		editing := m.mode == editMode && m.ed != nil && i == m.sel
		src := c.Source
		if editing {
			src = m.ed.text()
		}
		lines := strings.Split(src, "\n")
		before := count
		for r := range lines {
			if onlyRow < 0 || r == onlyRow {
				lines[r] = sub(lines[r])
			}
		}
		if count == before {
			return
		}
		cells++
		out := strings.Join(lines, "\n")
		if editing {
			m.ed.setText(out)
			m.commitEdit()
		} else {
			c.Source = out
		}
		m.dirty = true
	}
	switch {
	case all:
		for i, c := range m.nb.Cells {
			apply(i, c, -1)
		}
	case m.mode == editMode && m.ed != nil:
		apply(m.sel, m.cell(), m.ed.cur.row)
	default:
		apply(m.sel, m.cell(), -1)
	}
	if count == 0 {
		m.msg = "pattern not found"
	} else {
		m.msg = fmt.Sprintf("%d substitution%s in %d cell%s", count, plural(count), cells, plural(cells))
	}
	return true
}

// splitDelim splits on an unescaped delimiter, unescaping \<delim>.
func splitDelim(s string, d byte) []string {
	var parts []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s) && s[i+1] == d:
			cur.WriteByte(d)
			i++
		case s[i] == d:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(parts, cur.String())
}

// vimReplacement turns \1 and & into Go's ${1} and ${0}.
func vimReplacement(r string) string {
	var b strings.Builder
	for i := 0; i < len(r); i++ {
		switch {
		case r[i] == '\\' && i+1 < len(r) && r[i+1] >= '0' && r[i+1] <= '9':
			b.WriteString("${" + string(r[i+1]) + "}")
			i++
		case r[i] == '\\' && i+1 < len(r) && r[i+1] == '&':
			b.WriteByte('&')
			i++
		case r[i] == '&':
			b.WriteString("${0}")
		case r[i] == '$':
			b.WriteString("$$")
		default:
			b.WriteByte(r[i])
		}
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- folding ----

func (m *Model) fold(key string) {
	c := m.cell()
	switch key {
	case "a":
		m.folded[c] = !m.folded[c]
	case "M":
		for _, cc := range m.nb.Cells {
			if len(cc.Outputs) > 0 {
				m.folded[cc] = true
			}
		}
	case "R":
		m.folded = map[*notebook.Cell]bool{}
	}
}

// ---- outline ----

type outlineItem struct {
	cell, row, level int
	text             string
	code             bool
}

type outlinePanel struct {
	items []outlineItem
	sel   int
}

var defRe = regexp.MustCompile(`^(?:async\s+)?(def|class)\s+(\w+)`)

func (m *Model) openOutline() {
	var items []outlineItem
	for i, c := range m.nb.Cells {
		for r, l := range strings.Split(c.Source, "\n") {
			switch c.Type {
			case notebook.Markdown:
				t := strings.TrimLeft(l, "#")
				if level := len(l) - len(t); level > 0 && level <= 6 && strings.HasPrefix(t, " ") {
					items = append(items, outlineItem{cell: i, row: r, level: level, text: strings.TrimSpace(t)})
				}
			case notebook.Code:
				if d := defRe.FindStringSubmatch(l); d != nil {
					items = append(items, outlineItem{cell: i, row: r, level: 7, text: d[1] + " " + d[2], code: true})
				}
			}
		}
	}
	if len(items) == 0 {
		m.msg = "no headings or definitions"
		return
	}
	p := &outlinePanel{items: items}
	for i, it := range items {
		if it.cell <= m.sel {
			p.sel = i
		}
	}
	m.outline = p
}

func (m *Model) outlineKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.outline
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.outline = nil
	case "j", "down":
		p.sel = min(p.sel+1, len(p.items)-1)
	case "k", "up":
		p.sel = max(p.sel-1, 0)
	case "enter":
		it := p.items[p.sel]
		m.outline = nil
		if it.code {
			m.gotoLine(m.lineStarts()[it.cell]+it.row, -2)
		} else {
			if m.mode == editMode {
				m.stopEdit()
			}
			m.sel = it.cell
		}
	}
	return nil
}

func (m *Model) renderOutline() string {
	p := m.outline
	width := max(m.width, minWidth)
	lines := []string{" " + m.st.accent.Bold(true).Render("Outline"), ""}
	n := m.bodyHeight() - 3
	top := max(0, min(p.sel-n/2, len(p.items)-n))
	minLevel := 7
	for _, it := range p.items {
		minLevel = min(minLevel, it.level)
	}
	for i := top; i < len(p.items) && i < top+n; i++ {
		it := p.items[i]
		indent := strings.Repeat("  ", max(it.level-minLevel, 0))
		if it.code {
			indent = strings.Repeat("  ", 6-minLevel+1)
		}
		text := indent + it.text
		st := lipglossBold
		if it.code {
			st = func(s string) string { return m.st.dim.Render(s) }
		}
		line := " " + ansi.Truncate(st(text), width-4, "…")
		if i == p.sel {
			line = " " + m.st.selection.Render(padRight(ansi.Truncate(text, width-4, "…"), width-4))
		}
		lines = append(lines, line)
	}
	for len(lines) < m.bodyHeight()-1 {
		lines = append(lines, "")
	}
	lines = append(lines[:m.bodyHeight()-1], " "+m.st.dim.Render("j/k move · enter jump · q close"))
	return strings.Join(lines, "\n")
}
