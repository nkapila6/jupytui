package ui

import (
	"sort"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Flash-style jumping, after folke/flash.nvim: press s, type a few
// characters, every match on screen gets a label, type the label to go
// there. Labels never use a letter that could continue the search, so
// typing more always narrows instead of jumping by accident.

const flashLabels = "asdfghjklqwertyuiopzxcvbnm"

type flashTarget struct {
	cell     int
	md       bool // markdown matches just select the cell
	row, col int
}

type flashState struct {
	pattern string
	targets map[string]flashTarget
	first   *flashTarget
}

func (m *Model) startFlash() {
	m.flash = &flashState{}
	m.msg = ""
}

func (m *Model) flashKey(msg tea.KeyPressMsg) tea.Cmd {
	f := m.flash
	switch msg.String() {
	case "esc", "ctrl+c":
		m.flash = nil
		return nil
	case "backspace":
		if f.pattern == "" {
			m.flash = nil
		} else {
			r := []rune(f.pattern)
			f.pattern = string(r[:len(r)-1])
		}
		return nil
	case "enter":
		m.flash = nil
		if f.first != nil {
			m.flashJump(*f.first)
		}
		return nil
	}
	tok := keyTok(msg)
	if !isText(tok) {
		return nil
	}
	if t, ok := f.targets[tok]; ok && f.pattern != "" {
		m.flash = nil
		m.flashJump(t)
		return nil
	}
	f.pattern += tok
	return nil
}

func (m *Model) flashJump(t flashTarget) {
	if m.mode == editMode {
		m.stopEdit()
	}
	m.sel = t.cell
	if t.md {
		return
	}
	m.startEdit("normal")
	m.ed.cur = pos{t.row, t.col}
	m.ed.clamp()
	m.ed.want = m.ed.cur.col
}

type flashHit struct {
	y, x, w int
	text    string
	next    rune
	t       flashTarget
}

// renderFlash dims the screen, highlights matches and draws labels. It
// also stores the label -> target map the next key press uses.
func (m *Model) renderFlash(visible []string) []string {
	f := m.flash
	for i, l := range visible {
		visible[i] = m.st.dim.Render(ansi.Strip(l))
	}
	f.targets, f.first = nil, nil
	if f.pattern == "" {
		return visible
	}
	pat := []rune(f.pattern)
	fold := strings.ToLower(f.pattern) == f.pattern // smartcase

	var hits []flashHit
	refY, refX := m.flashRef()
	for _, r := range m.layout {
		y := r.line - m.offset
		if y < 0 || y >= len(visible) {
			continue
		}
		var seg string
		var srcLine []rune
		if r.md {
			seg = ansi.Cut(ansi.Strip(visible[y]), r.textX, r.textX+r.width)
		} else {
			srcLine = m.cellLine(r.cell, r.src)
			seg = ansi.Cut(expandTabs(string(srcLine)), r.dcol, r.dcol+r.width)
		}
		runes := []rune(seg)
		for i := 0; i+len(pat) <= len(runes); i++ {
			if !runesMatch(runes[i:i+len(pat)], pat, fold) {
				continue
			}
			off := ansi.StringWidth(string(runes[:i]))
			h := flashHit{
				y:    y,
				x:    r.textX + off,
				w:    ansi.StringWidth(string(pat)),
				text: string(runes[i : i+len(pat)]),
				t:    flashTarget{cell: r.cell, md: r.md},
			}
			if i+len(pat) < len(runes) {
				h.next = unicode.ToLower(runes[i+len(pat)])
			}
			if !r.md {
				h.t.row = r.src
				h.t.col = runeAt(srcLine, r.dcol+off)
			}
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 {
		return visible
	}

	// closest matches get the easiest labels
	abs := func(n int) int { return max(n, -n) }
	sort.SliceStable(hits, func(i, j int) bool {
		di := abs(hits[i].y-refY)*1000 + abs(hits[i].x-refX)
		dj := abs(hits[j].y-refY)*1000 + abs(hits[j].x-refX)
		return di < dj
	})
	skip := map[rune]bool{}
	for _, h := range hits {
		skip[h.next] = true
	}
	var labels []string
	for _, r := range flashLabels + strings.ToUpper(flashLabels) {
		if !skip[unicode.ToLower(r)] {
			labels = append(labels, string(r))
		}
	}

	f.targets = map[string]flashTarget{}
	first := hits[0].t
	f.first = &first
	for i, h := range hits {
		line := visible[h.y]
		line = ansi.Cut(line, 0, h.x) + m.st.flashMatch.Render(h.text) + ansi.Cut(line, h.x+h.w, ansi.StringWidth(line))
		if i < len(labels) {
			f.targets[labels[i]] = h.t
			lx := h.x + h.w
			lw := ansi.StringWidth(line)
			if lx >= lw {
				line += strings.Repeat(" ", lx-lw+1)
			}
			line = ansi.Cut(line, 0, lx) + m.st.flashLabel.Render(labels[i]) + ansi.Cut(line, lx+1, ansi.StringWidth(line))
		}
		visible[h.y] = line
	}
	return visible
}

// flashRef is where labels count distance from: the edit cursor, or the
// top of the selected cell.
func (m *Model) flashRef() (int, int) {
	if m.mode == editMode && m.ed != nil {
		if r, x, ok := m.cursorRow(m.layout); ok {
			return r.line - m.offset, x
		}
	}
	for _, r := range m.layout {
		if r.cell == m.sel {
			return r.line - m.offset, r.textX
		}
	}
	return 0, 0
}

// cellLine is source line r of cell i, from the live editor if open.
func (m *Model) cellLine(i, r int) []rune {
	if m.mode == editMode && m.ed != nil && i == m.sel {
		if r <= m.ed.last() {
			return m.ed.line(r)
		}
		return nil
	}
	lines := strings.Split(m.nb.Cells[i].Source, "\n")
	if r < len(lines) {
		return []rune(lines[r])
	}
	return nil
}

// runeAt converts a display column back to a rune index.
func runeAt(line []rune, d int) int {
	w := 0
	for i, r := range line {
		if w >= d {
			return i
		}
		if r == '\t' {
			w += 4
		} else {
			w += ansi.StringWidth(string(r))
		}
	}
	return len(line)
}

func runesMatch(a, b []rune, fold bool) bool {
	for i := range a {
		x, y := a[i], b[i]
		if fold {
			x = unicode.ToLower(x)
		}
		if x != y {
			return false
		}
	}
	return true
}
