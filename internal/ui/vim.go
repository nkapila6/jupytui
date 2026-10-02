package ui

import (
	"strings"
	"unicode"
)

// A small vim for editing one cell. It owns the cell's text while the
// cell is open; the UI feeds it keys and draws lines/cursor from it.

type vmode int

const (
	vNormal vmode = iota
	vInsert
	vVisual
	vVisualLine
)

type pos struct{ row, col int }

func (a pos) before(b pos) bool { return a.row < b.row || a.row == b.row && a.col < b.col }

func order(a, b pos) (pos, pos) {
	if b.before(a) {
		return b, a
	}
	return a, b
}

// register is shared by all cells, like vim's unnamed register.
type register struct {
	text     string
	linewise bool
}

type snapshot struct {
	lines []string
	cur   pos
}

// edResult tells the UI when a key should leave the cell.
type edResult int

const (
	edNone edResult = iota
	edLeave
	edNextCell
	edPrevCell
	edCross // j/k ran past the cell; jump holds the signed line delta
	edGoto  // {n}G / {n}gg: gotoLine is a notebook-wide line number
)

// command parse status
const (
	stMore = iota
	stDone
	stBad
)

type motion struct {
	name string
	ch   rune
}

type editor struct {
	lines  [][]rune
	cur    pos
	want   int // column to aim for on j/k, -1 means end of line
	mode   vmode
	anchor pos
	vim    bool
	python bool
	reg    *register

	keys       []string
	undo, redo []snapshot
	lastFind   motion
	lastChange []string
	recording  []string
	inChange   bool

	jump     int
	gotoLine int
}

const indentUnit = "    "

func newEditor(src string, vim bool, reg *register, python bool) *editor {
	e := &editor{vim: vim, reg: reg, python: python}
	e.load(src)
	if !vim {
		e.mode = vInsert
	}
	return e
}

func (e *editor) load(src string) {
	parts := strings.Split(src, "\n")
	e.lines = make([][]rune, len(parts))
	for i, p := range parts {
		e.lines[i] = []rune(p)
	}
	e.cur.row = min(e.cur.row, len(e.lines)-1)
	e.clamp()
}

func (e *editor) text() string {
	parts := make([]string, len(e.lines))
	for i, l := range e.lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

// setText replaces the content from outside (external editor) as an
// undoable change.
func (e *editor) setText(src string) {
	if src == e.text() {
		return
	}
	e.save()
	e.load(src)
}

func (e *editor) pending() string { return strings.Join(e.keys, "") }

func (e *editor) last() int { return len(e.lines) - 1 }

func (e *editor) line(r int) []rune { return e.lines[r] }

func (e *editor) charAt(p pos) rune {
	if p.col >= len(e.lines[p.row]) {
		return '\n'
	}
	return e.lines[p.row][p.col]
}

// clamp keeps the cursor on a real character in normal mode; insert
// mode may sit one past the end.
func (e *editor) clamp() {
	e.cur.row = max(0, min(e.cur.row, e.last()))
	n := len(e.lines[e.cur.row])
	if e.mode == vInsert {
		e.cur.col = max(0, min(e.cur.col, n))
	} else {
		e.cur.col = max(0, min(e.cur.col, n-1))
	}
}

func (e *editor) setMode(m vmode) {
	e.mode = m
	e.clamp()
}

// selection returns the visual range, ordered and inclusive.
func (e *editor) selection() (pos, pos, bool, bool) {
	if e.mode != vVisual && e.mode != vVisualLine {
		return pos{}, pos{}, false, false
	}
	a, b := order(e.anchor, e.cur)
	return a, b, e.mode == vVisualLine, true
}

// ---- undo ----

func (e *editor) save() {
	s := snapshot{lines: make([]string, len(e.lines)), cur: e.cur}
	for i, l := range e.lines {
		s.lines[i] = string(l)
	}
	e.undo = append(e.undo, s)
	if len(e.undo) > 200 {
		e.undo = e.undo[1:]
	}
	e.redo = nil
}

func (e *editor) restore(s snapshot) {
	e.lines = make([][]rune, len(s.lines))
	for i, l := range s.lines {
		e.lines[i] = []rune(l)
	}
	e.cur = s.cur
	e.clamp()
}

func (e *editor) current() snapshot {
	s := snapshot{lines: make([]string, len(e.lines)), cur: e.cur}
	for i, l := range e.lines {
		s.lines[i] = string(l)
	}
	return s
}

func (e *editor) undoN(n int) {
	for ; n > 0 && len(e.undo) > 0; n-- {
		e.redo = append(e.redo, e.current())
		s := e.undo[len(e.undo)-1]
		e.undo = e.undo[:len(e.undo)-1]
		e.restore(s)
	}
}

func (e *editor) redoN(n int) {
	for ; n > 0 && len(e.redo) > 0; n-- {
		e.undo = append(e.undo, e.current())
		s := e.redo[len(e.redo)-1]
		e.redo = e.redo[:len(e.redo)-1]
		e.restore(s)
	}
}

// ---- key entry ----

func (e *editor) key(tok string) edResult {
	if e.mode == vInsert || !e.vim {
		return e.insertKey(tok)
	}
	e.keys = append(e.keys, tok)
	var res edResult
	var st int
	if e.mode == vNormal {
		res, st = e.normal(e.keys)
	} else {
		res, st = e.visual(e.keys)
	}
	if st != stMore {
		e.keys = e.keys[:0]
	}
	return res
}

func (e *editor) paste(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if e.mode != vInsert {
		e.save()
	}
	e.insertText(s)
	e.clamp()
}

// ---- insert mode ----

func (e *editor) insertKey(tok string) edResult {
	if e.inChange {
		e.recording = append(e.recording, tok)
	}
	switch tok {
	case "esc", "ctrl+c", "ctrl+[":
		if !e.vim {
			return edLeave
		}
		e.finishInsert()
	case "enter":
		e.newline()
	case "backspace":
		e.backspace()
	case "delete":
		l := e.line(e.cur.row)
		if e.cur.col < len(l) {
			e.lines[e.cur.row] = append(l[:e.cur.col:e.cur.col], l[e.cur.col+1:]...)
		} else if e.cur.row < e.last() {
			e.joinRaw(e.cur.row, "")
		}
	case "tab":
		n := len(indentUnit) - e.cur.col%len(indentUnit)
		e.insertText(strings.Repeat(" ", n))
	case "left":
		e.cur.col = max(0, e.cur.col-1)
	case "right":
		e.cur.col = min(len(e.line(e.cur.row)), e.cur.col+1)
	case "up":
		e.cur.row = max(0, e.cur.row-1)
		e.clamp()
	case "down":
		e.cur.row = min(e.last(), e.cur.row+1)
		e.clamp()
	case "home":
		e.cur.col = 0
	case "end":
		e.cur.col = len(e.line(e.cur.row))
	case "ctrl+w":
		l := e.line(e.cur.row)
		c := e.cur.col
		for c > 0 && unicode.IsSpace(l[c-1]) {
			c--
		}
		if c > 0 {
			k := cls(l[c-1], false)
			for c > 0 && cls(l[c-1], false) == k {
				c--
			}
		}
		e.lines[e.cur.row] = append(l[:c:c], l[e.cur.col:]...)
		e.cur.col = c
	case "ctrl+u":
		l := e.line(e.cur.row)
		e.lines[e.cur.row] = append([]rune{}, l[e.cur.col:]...)
		e.cur.col = 0
	default:
		if isText(tok) {
			e.insertText(tok)
		}
	}
	return edNone
}

func isText(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if unicode.IsControl(r) {
			return false
		}
	}
	if len([]rune(tok)) == 1 {
		return true
	}
	// named keys come through as lowercase words like "up" or "ctrl+x"
	for _, r := range tok {
		if r == '+' || r > unicode.MaxASCII || unicode.IsUpper(r) {
			return r != '+'
		}
	}
	return false
}

func (e *editor) finishInsert() {
	e.mode = vNormal
	if e.cur.col > 0 {
		e.cur.col--
	}
	e.clamp()
	e.want = e.cur.col
	if e.inChange {
		e.lastChange = e.recording
		e.inChange = false
	}
	// an insert that changed nothing shouldn't leave an undo step
	if n := len(e.undo); n > 0 && strings.Join(e.undo[n-1].lines, "\n") == e.text() {
		e.undo = e.undo[:n-1]
	}
}

func (e *editor) startInsert(keys []string) {
	e.mode = vInsert
	e.recording = append([]string(nil), keys...)
	e.inChange = true
	e.clamp()
}

func (e *editor) insertText(s string) {
	for i, part := range strings.Split(s, "\n") {
		if i > 0 {
			e.splitLine("")
		}
		l := e.line(e.cur.row)
		r := []rune(part)
		nl := make([]rune, 0, len(l)+len(r))
		nl = append(nl, l[:e.cur.col]...)
		nl = append(nl, r...)
		nl = append(nl, l[e.cur.col:]...)
		e.lines[e.cur.row] = nl
		e.cur.col += len(r)
	}
}

func (e *editor) splitLine(indent string) {
	l := e.line(e.cur.row)
	before := append([]rune{}, l[:e.cur.col]...)
	after := []rune(indent + string(l[e.cur.col:]))
	e.lines[e.cur.row] = before
	e.lines = append(e.lines[:e.cur.row+1], append([][]rune{after}, e.lines[e.cur.row+1:]...)...)
	e.cur = pos{e.cur.row + 1, len([]rune(indent))}
}

// newline keeps the indent and adds one after a python block opener.
func (e *editor) newline() {
	l := e.line(e.cur.row)
	indent := leadingSpace(l[:e.cur.col])
	if e.python && strings.HasSuffix(strings.TrimRight(string(l[:e.cur.col]), " \t"), ":") {
		indent += indentUnit
	}
	// drop whitespace that would end up at the start of the new line
	rest := l[e.cur.col:]
	trim := 0
	for trim < len(rest) && (rest[trim] == ' ' || rest[trim] == '\t') {
		trim++
	}
	e.lines[e.cur.row] = append(l[:e.cur.col:e.cur.col], rest[trim:]...)
	e.splitLine(indent)
}

func (e *editor) backspace() {
	if e.cur.col == 0 {
		if e.cur.row > 0 {
			prev := len(e.line(e.cur.row - 1))
			e.joinRaw(e.cur.row-1, "")
			e.cur = pos{e.cur.row - 1, prev}
		}
		return
	}
	l := e.line(e.cur.row)
	n := 1
	// in leading indent, delete back to the previous indent stop
	if strings.TrimLeft(string(l[:e.cur.col]), " ") == "" {
		n = (e.cur.col-1)%len(indentUnit) + 1
	}
	e.lines[e.cur.row] = append(l[:e.cur.col-n:e.cur.col-n], l[e.cur.col:]...)
	e.cur.col -= n
}

// joinRaw appends line r+1 to line r with sep between.
func (e *editor) joinRaw(r int, sep string) {
	joined := append(append(append([]rune{}, e.lines[r]...), []rune(sep)...), e.lines[r+1]...)
	e.lines[r] = joined
	e.lines = append(e.lines[:r+1], e.lines[r+2:]...)
}

// ---- normal mode ----

func parseCount(keys []string, i *int) (int, bool) {
	n, has := 0, false
	for *i < len(keys) {
		k := keys[*i]
		if len(k) != 1 || k[0] < '0' || k[0] > '9' || (!has && k == "0") {
			break
		}
		n = n*10 + int(k[0]-'0')
		has = true
		*i++
	}
	return n, has
}

func parseMotion(keys []string, i int) (motion, int) {
	if i >= len(keys) {
		return motion{}, stMore
	}
	k := keys[i]
	switch k {
	case "h", "l", "j", "k", "w", "b", "e", "W", "B", "E", "0", "^", "$", "G", ";", ",", "%",
		"{", "}", "left", "right", "up", "down", "home", "end", "backspace", " ", "enter", "+", "-", "_":
		return motion{name: k}, stDone
	case "g":
		if i+1 >= len(keys) {
			return motion{}, stMore
		}
		if keys[i+1] == "g" {
			return motion{name: "gg"}, stDone
		}
		if keys[i+1] == "_" {
			return motion{name: "g_"}, stDone
		}
	case "f", "t", "F", "T":
		if i+1 >= len(keys) {
			return motion{}, stMore
		}
		if r := []rune(keys[i+1]); len(r) == 1 {
			return motion{name: k, ch: r[0]}, stDone
		}
	}
	return motion{}, stBad
}

func (e *editor) normal(keys []string) (edResult, int) {
	i := 0
	n1, has1 := parseCount(keys, &i)
	if i >= len(keys) {
		return edNone, stMore
	}
	count := max(n1, 1)
	k := keys[i]
	i++

	switch k {
	case "esc", "ctrl+c":
		if len(keys) == 1 {
			return edLeave, stDone
		}
		return edNone, stBad

	case "d", "c", "y", ">", "<":
		if i >= len(keys) {
			return edNone, stMore
		}
		if keys[i] == k {
			e.opLines(k, e.cur.row, min(e.cur.row+count-1, e.last()))
			e.recordChange(k, keys)
			return edNone, stDone
		}
		n2, has2 := parseCount(keys, &i)
		if i >= len(keys) {
			return edNone, stMore
		}
		total := count
		if has2 {
			total = count * n2
		}
		if keys[i] == "i" || keys[i] == "a" {
			if i+1 >= len(keys) {
				return edNone, stMore
			}
			a, b, ok := e.textObject(keys[i] == "a", keys[i+1])
			if !ok {
				return edNone, stBad
			}
			e.applyOp(k, a, b, false, true)
			e.recordChange(k, keys)
			return edNone, stDone
		}
		mv, st := parseMotion(keys, i)
		if st != stDone {
			return edNone, st
		}
		target, lw, incl, ok := e.motion(mv, total, has1 || has2, k)
		if !ok {
			return edNone, stBad
		}
		e.applyOp(k, e.cur, target, lw, incl)
		e.recordChange(k, keys)
		return edNone, stDone

	case "i", "a", "I", "A", "o", "O":
		e.save()
		l := e.line(e.cur.row)
		switch k {
		case "a":
			if len(l) > 0 {
				e.cur.col++
			}
		case "I":
			e.cur.col = firstNonBlank(l)
		case "A":
			e.cur.col = len(l)
		case "o":
			indent := leadingSpace(l)
			if e.python && strings.HasSuffix(strings.TrimRight(string(l), " \t"), ":") {
				indent += indentUnit
			}
			e.lines = append(e.lines[:e.cur.row+1], append([][]rune{[]rune(indent)}, e.lines[e.cur.row+1:]...)...)
			e.cur = pos{e.cur.row + 1, len([]rune(indent))}
		case "O":
			indent := leadingSpace(l)
			e.lines = append(e.lines[:e.cur.row], append([][]rune{[]rune(indent)}, e.lines[e.cur.row:]...)...)
			e.cur = pos{e.cur.row, len([]rune(indent))}
		}
		e.mode = vInsert
		e.startInsert(keys)
		return edNone, stDone

	// shorthands that are just an operator plus a motion
	case "x", "X", "s", "S", "D", "C", "Y":
		if (k == "x" || k == "X") && len(e.line(e.cur.row)) == 0 {
			return edNone, stDone
		}
		mapped := map[string][]string{
			"x": {"d", "l"}, "X": {"d", "h"}, "s": {"c", "l"}, "S": {"c", "c"},
			"D": {"d", "$"}, "C": {"c", "$"}, "Y": {"y", "y"},
		}[k]
		sub := append(countKeys(n1, has1), mapped...)
		saved := e.lastChange
		e.normal(sub)
		if k == "Y" {
			e.lastChange = saved
		} else if e.mode == vInsert {
			e.recording = append([]string(nil), keys...)
		} else {
			e.lastChange = append([]string(nil), keys...)
		}
		return edNone, stDone

	case "p", "P":
		e.put(k == "P", count)
		e.recordChange(k, keys)
		return edNone, stDone

	case "J":
		e.save()
		for range max(count-1, 1) {
			if e.cur.row >= e.last() {
				break
			}
			e.join(e.cur.row)
		}
		e.recordChange(k, keys)
		return edNone, stDone

	case "~":
		l := e.line(e.cur.row)
		if len(l) == 0 {
			return edNone, stDone
		}
		e.save()
		end := min(e.cur.col+count, len(l))
		for c := e.cur.col; c < end; c++ {
			l[c] = toggleCase(l[c])
		}
		e.cur.col = end
		e.clamp()
		e.recordChange(k, keys)
		return edNone, stDone

	case "r":
		if i >= len(keys) {
			return edNone, stMore
		}
		r := []rune(keys[i])
		l := e.line(e.cur.row)
		if len(r) != 1 || e.cur.col+count > len(l) {
			return edNone, stBad
		}
		e.save()
		for c := e.cur.col; c < e.cur.col+count; c++ {
			l[c] = r[0]
		}
		e.cur.col += count - 1
		e.recordChange(k, keys)
		return edNone, stDone

	case "u":
		e.undoN(count)
		return edNone, stDone
	case "ctrl+r":
		e.redoN(count)
		return edNone, stDone

	case ".":
		if len(e.lastChange) > 0 {
			replay := e.lastChange
			e.keys = nil
			for _, t := range replay {
				e.key(t)
			}
		}
		return edNone, stDone

	case "v", "V":
		e.anchor = e.cur
		if k == "v" {
			e.mode = vVisual
		} else {
			e.mode = vVisualLine
		}
		return edNone, stDone
	}

	// plain cursor motion
	mv, st := parseMotion(keys, i-1)
	if st != stDone {
		return edNone, st
	}
	// line numbers run across the whole notebook, so counts and
	// numbered jumps carry on into other cells
	switch mv.name {
	case "j", "down", "enter", "+":
		if e.cur.row+count > e.last() {
			e.jump = count
			return edCross, stDone
		}
	case "k", "up", "-":
		if e.cur.row-count < 0 {
			e.jump = -count
			return edCross, stDone
		}
	case "gg", "G":
		if has1 {
			e.gotoLine = count
			return edGoto, stDone
		}
	}
	target, _, _, ok := e.motion(mv, count, has1, "")
	if ok {
		e.cur = target
		e.clamp()
	}
	return edNone, stDone
}

func countKeys(n int, has bool) []string {
	if !has {
		return nil
	}
	var out []string
	for _, r := range itoa(n) {
		out = append(out, string(r))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// recordChange remembers keys for "." unless the op didn't change text.
func (e *editor) recordChange(op string, keys []string) {
	if op == "y" {
		return
	}
	if e.mode == vInsert {
		e.startInsert(keys)
		return
	}
	e.lastChange = append([]string(nil), keys...)
}

// ---- visual mode ----

func (e *editor) visual(keys []string) (edResult, int) {
	i := 0
	n, has := parseCount(keys, &i)
	if i >= len(keys) {
		return edNone, stMore
	}
	count := max(n, 1)
	k := keys[i]
	switch k {
	case "esc", "ctrl+c":
		e.setMode(vNormal)
		return edNone, stDone
	case "v", "V":
		want := vVisual
		if k == "V" {
			want = vVisualLine
		}
		if e.mode == want {
			e.setMode(vNormal)
		} else {
			e.mode = want
		}
		return edNone, stDone
	case "o":
		e.anchor, e.cur = e.cur, e.anchor
		return edNone, stDone
	case "d", "x", "c", "s", "y", ">", "<":
		op := map[string]string{"x": "d", "s": "c"}[k]
		if op == "" {
			op = k
		}
		a, b, lw, _ := e.selection()
		e.mode = vNormal
		if lw || op == ">" || op == "<" {
			e.opLines(op, a.row, b.row)
		} else {
			e.applyOp(op, a, b, false, true)
		}
		if e.mode == vInsert {
			e.startInsert(nil)
		}
		return edNone, stDone
	case "J":
		a, b, _, _ := e.selection()
		e.save()
		e.cur = a
		for range max(b.row-a.row, 1) {
			if e.cur.row >= e.last() {
				break
			}
			e.join(e.cur.row)
		}
		e.setMode(vNormal)
		return edNone, stDone
	case "~", "u", "U":
		a, b, lw, _ := e.selection()
		e.save()
		for r := a.row; r <= b.row; r++ {
			l := e.lines[r]
			from, to := 0, len(l)-1
			if !lw {
				if r == a.row {
					from = a.col
				}
				if r == b.row {
					to = min(b.col, len(l)-1)
				}
			}
			for c := from; c <= to && c < len(l); c++ {
				switch k {
				case "~":
					l[c] = toggleCase(l[c])
				case "u":
					l[c] = unicode.ToLower(l[c])
				case "U":
					l[c] = unicode.ToUpper(l[c])
				}
			}
		}
		e.cur = a
		e.setMode(vNormal)
		return edNone, stDone
	case "p", "P":
		a, b, lw, _ := e.selection()
		reg := *e.reg
		e.mode = vNormal
		if lw {
			e.opLines("d", a.row, b.row)
			if a.row > e.last() || a.row == e.last() && b.row > e.last() {
				e.put(false, 1)
			} else {
				e.put(true, 1)
			}
		} else {
			e.applyOp("d", a, b, false, true)
			*e.reg = reg
			e.put(true, 1)
		}
		*e.reg = reg
		return edNone, stDone
	case "i", "a":
		if i+1 >= len(keys) {
			return edNone, stMore
		}
		a, b, ok := e.textObject(k == "a", keys[i+1])
		if !ok {
			return edNone, stBad
		}
		e.mode = vVisual
		e.anchor, e.cur = a, b
		return edNone, stDone
	}
	mv, st := parseMotion(keys, i)
	if st != stDone {
		return edNone, st
	}
	if target, _, _, ok := e.motion(mv, count, has, ""); ok {
		e.cur = target
		e.cur.col = min(e.cur.col, max(len(e.line(e.cur.row))-1, 0))
	}
	return edNone, stDone
}

// ---- motions ----

func (e *editor) motion(mv motion, count int, hasCount bool, op string) (pos, bool, bool, bool) {
	p := e.cur
	l := e.line(p.row)
	vertical := func(rows int) {
		p.row = max(0, min(e.last(), p.row+rows))
		n := len(e.line(p.row))
		if e.want < 0 {
			p.col = max(n-1, 0)
		} else {
			p.col = max(0, min(e.want, n-1))
		}
	}
	setWant := true
	defer func() {
		if setWant && op == "" {
			e.want = p.col
		}
	}()

	switch mv.name {
	case "h", "left", "backspace":
		p.col = max(0, p.col-count)
		return p, false, false, true
	case "l", "right", " ":
		limit := len(l) - 1
		if op != "" {
			limit = len(l)
		}
		p.col = max(0, min(p.col+count, limit))
		return p, false, false, true
	case "j", "down":
		setWant = false
		vertical(count)
		return p, true, false, true
	case "k", "up":
		setWant = false
		vertical(-count)
		return p, true, false, true
	case "enter", "+", "-", "_":
		switch mv.name {
		case "-":
			p.row = max(0, p.row-count)
		case "_":
			p.row = min(e.last(), p.row+count-1)
		default:
			p.row = min(e.last(), p.row+count)
		}
		p.col = firstNonBlank(e.line(p.row))
		return p, true, false, true
	case "0", "home":
		p.col = 0
		return p, false, false, true
	case "^":
		p.col = firstNonBlank(l)
		return p, false, false, true
	case "$", "end":
		p.row = min(e.last(), p.row+count-1)
		p.col = max(len(e.line(p.row))-1, 0)
		if op == "" {
			setWant = false
			e.want = -1
		}
		return p, false, true, true
	case "g_":
		p.row = min(e.last(), p.row+count-1)
		lr := e.line(p.row)
		c := len(lr) - 1
		for c > 0 && unicode.IsSpace(lr[c]) {
			c--
		}
		p.col = max(c, 0)
		return p, false, true, true
	case "gg", "G":
		switch {
		case hasCount:
			p.row = min(count-1, e.last())
		case mv.name == "G":
			p.row = e.last()
		default:
			p.row = 0
		}
		p.col = firstNonBlank(e.line(p.row))
		return p, true, false, true
	case "w", "W":
		big := mv.name == "W"
		// cw on a word acts like ce, as in vim
		if op == "c" && !unicode.IsSpace(e.charAt(p)) && e.charAt(p) != '\n' {
			for range count {
				p = e.wordEnd(p, big, true)
			}
			return p, false, true, true
		}
		start := p
		for range count {
			p = e.wordFwd(p, big)
		}
		// dw on the last word of a line stops at the line end
		if op != "" && p.row > start.row {
			r := p.row - 1
			if p.col > firstNonBlank(e.line(p.row)) || len(e.line(p.row)) == 0 && p.row == e.last() {
				r = p.row
			}
			if r > start.row || p.col == 0 || firstNonBlank(e.line(p.row)) >= p.col {
				p = pos{max(r, start.row), len(e.line(max(r, start.row)))}
				if p.row == start.row || p.col == 0 {
					p = pos{p.row, len(e.line(p.row))}
				}
			}
		}
		return p, false, false, true
	case "b", "B":
		for range count {
			p = e.wordBack(p, mv.name == "B")
		}
		return p, false, false, true
	case "e", "E":
		for range count {
			p = e.wordEnd(p, mv.name == "E", false)
		}
		return p, false, true, true
	case "f", "t", "F", "T":
		e.lastFind = mv
		q, ok := e.find(mv, count, false)
		return q, false, mv.name == "f" || mv.name == "t", ok
	case ";", ",":
		if e.lastFind.name == "" {
			return p, false, false, false
		}
		mv2 := e.lastFind
		if mv.name == "," {
			mv2.name = map[string]string{"f": "F", "F": "f", "t": "T", "T": "t"}[mv2.name]
		}
		q, ok := e.find(mv2, count, true)
		return q, false, mv2.name == "f" || mv2.name == "t", ok
	case "%":
		q, ok := e.matchBracket(p)
		return q, false, true, ok
	case "{", "}":
		r := p.row
		blank := func(r int) bool { return strings.TrimSpace(string(e.line(r))) == "" }
		for range count {
			if mv.name == "}" {
				for r < e.last() && blank(r) {
					r++
				}
				for r < e.last() && !blank(r) {
					r++
				}
			} else {
				for r > 0 && blank(r) {
					r--
				}
				for r > 0 && !blank(r) {
					r--
				}
			}
		}
		p = pos{r, 0}
		if mv.name == "}" && r == e.last() && !blank(r) {
			p.col = max(len(e.line(r))-1, 0)
		}
		return p, false, false, true
	}
	return p, false, false, false
}

func cls(r rune, big bool) int {
	switch {
	case unicode.IsSpace(r):
		return 0
	case big:
		return 1
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return 1
	}
	return 2
}

func (e *editor) stepFwd(p pos) (pos, bool) {
	if p.col < len(e.line(p.row)) {
		return pos{p.row, p.col + 1}, true
	}
	if p.row < e.last() {
		return pos{p.row + 1, 0}, true
	}
	return p, false
}

func (e *editor) stepBack(p pos) (pos, bool) {
	if p.col > 0 {
		return pos{p.row, min(p.col-1, len(e.line(p.row)))}, true
	}
	if p.row > 0 {
		return pos{p.row - 1, len(e.line(p.row - 1))}, true
	}
	return p, false
}

func (e *editor) wordFwd(p pos, big bool) pos {
	l := e.line(p.row)
	if p.col < len(l) {
		if k := cls(l[p.col], big); k != 0 {
			for p.col < len(l) && cls(l[p.col], big) == k {
				p.col++
			}
		}
	}
	for {
		l = e.line(p.row)
		if p.col >= len(l) {
			if p.row == e.last() {
				return p
			}
			p = pos{p.row + 1, 0}
			if len(e.line(p.row)) == 0 {
				return p
			}
			continue
		}
		if unicode.IsSpace(l[p.col]) {
			p.col++
			continue
		}
		return p
	}
}

// wordEnd moves to the end of the next word. stay lets it stop on the
// current word's end (what cw needs).
func (e *editor) wordEnd(p pos, big, stay bool) pos {
	if !stay {
		np, ok := e.stepFwd(p)
		if !ok {
			return p
		}
		p = np
	}
	for {
		c := e.charAt(p)
		if c != '\n' && !unicode.IsSpace(c) {
			break
		}
		np, ok := e.stepFwd(p)
		if !ok {
			return p
		}
		p = np
	}
	k := cls(e.charAt(p), big)
	l := e.line(p.row)
	for p.col+1 < len(l) && cls(l[p.col+1], big) == k {
		p.col++
	}
	return p
}

func (e *editor) wordBack(p pos, big bool) pos {
	np, ok := e.stepBack(p)
	if !ok {
		return p
	}
	p = np
	for {
		c := e.charAt(p)
		if c == '\n' && len(e.line(p.row)) == 0 {
			return p
		}
		if c == '\n' || unicode.IsSpace(c) {
			np, ok := e.stepBack(p)
			if !ok {
				return p
			}
			p = np
			continue
		}
		break
	}
	l := e.line(p.row)
	k := cls(l[p.col], big)
	for p.col > 0 && cls(l[p.col-1], big) == k {
		p.col--
	}
	return p
}

func (e *editor) find(mv motion, count int, repeat bool) (pos, bool) {
	l := e.line(e.cur.row)
	col := e.cur.col
	switch mv.name {
	case "f", "t":
		start := col + 1
		// repeating t from right before the char would get stuck
		if repeat && mv.name == "t" && start < len(l) && l[start] == mv.ch {
			start++
		}
		for c := start; c < len(l); c++ {
			if l[c] == mv.ch {
				if count--; count == 0 {
					if mv.name == "t" {
						c--
					}
					return pos{e.cur.row, c}, true
				}
			}
		}
	case "F", "T":
		start := col - 1
		if repeat && mv.name == "T" && start >= 0 && l[start] == mv.ch {
			start--
		}
		for c := start; c >= 0; c-- {
			if l[c] == mv.ch {
				if count--; count == 0 {
					if mv.name == "T" {
						c++
					}
					return pos{e.cur.row, c}, true
				}
			}
		}
	}
	return e.cur, false
}

var pairs = map[rune]rune{'(': ')', '[': ']', '{': '}', ')': '(', ']': '[', '}': '{'}

func (e *editor) matchBracket(p pos) (pos, bool) {
	l := e.line(p.row)
	c := p.col
	for c < len(l) && pairs[l[c]] == 0 {
		c++
	}
	if c >= len(l) {
		return p, false
	}
	open := l[c]
	close := pairs[open]
	fwd := open == '(' || open == '[' || open == '{'
	depth := 0
	q := pos{p.row, c}
	for {
		var ok bool
		if fwd {
			q, ok = e.stepFwd(q)
		} else {
			q, ok = e.stepBack(q)
		}
		if !ok {
			return p, false
		}
		switch e.charAt(q) {
		case open:
			depth++
		case close:
			if depth == 0 {
				return q, true
			}
			depth--
		}
	}
}

// ---- text objects ----

func (e *editor) textObject(around bool, obj string) (pos, pos, bool) {
	row := e.cur.row
	l := e.line(row)
	switch obj {
	case "w", "W":
		if len(l) == 0 {
			return e.cur, e.cur, false
		}
		big := obj == "W"
		col := min(e.cur.col, len(l)-1)
		k := cls(l[col], big)
		s, t := col, col
		for s > 0 && cls(l[s-1], big) == k {
			s--
		}
		for t < len(l)-1 && cls(l[t+1], big) == k {
			t++
		}
		if around {
			if k != 0 {
				if t+1 < len(l) && unicode.IsSpace(l[t+1]) {
					for t+1 < len(l) && unicode.IsSpace(l[t+1]) {
						t++
					}
				} else {
					for s > 0 && unicode.IsSpace(l[s-1]) {
						s--
					}
				}
			} else if t+1 < len(l) {
				k2 := cls(l[t+1], big)
				t++
				for t+1 < len(l) && cls(l[t+1], big) == k2 {
					t++
				}
			}
		}
		return pos{row, s}, pos{row, t}, true

	case `"`, "'", "`":
		q := []rune(obj)[0]
		var idx []int
		for c, r := range l {
			if r == q && (c == 0 || l[c-1] != '\\') {
				idx = append(idx, c)
			}
		}
		for j := 0; j+1 < len(idx); j += 2 {
			a, b := idx[j], idx[j+1]
			if b < e.cur.col {
				continue
			}
			if around {
				return pos{row, a}, pos{row, b}, true
			}
			return pos{row, a + 1}, pos{row, b - 1}, true
		}
		return e.cur, e.cur, false
	}

	var open, close rune
	switch obj {
	case "(", ")", "b":
		open, close = '(', ')'
	case "[", "]":
		open, close = '[', ']'
	case "{", "}", "B":
		open, close = '{', '}'
	case "<", ">":
		open, close = '<', '>'
	default:
		return e.cur, e.cur, false
	}
	var o pos
	if e.charAt(e.cur) == open {
		o = e.cur
	} else {
		depth := 0
		p := e.cur
		found := false
		for {
			var ok bool
			p, ok = e.stepBack(p)
			if !ok {
				break
			}
			c := e.charAt(p)
			if c == close {
				depth++
			} else if c == open {
				if depth == 0 {
					o, found = p, true
					break
				}
				depth--
			}
		}
		if !found {
			return e.cur, e.cur, false
		}
	}
	depth := 0
	p := o
	for {
		var ok bool
		p, ok = e.stepFwd(p)
		if !ok {
			return e.cur, e.cur, false
		}
		c := e.charAt(p)
		if c == open {
			depth++
		} else if c == close {
			if depth > 0 {
				depth--
				continue
			}
			if around {
				return o, p, true
			}
			a, _ := e.stepFwd(o)
			// a block opened at a line end starts on the next line
			if a.col >= len(e.line(a.row)) && a.row < p.row {
				a = pos{a.row + 1, 0}
			}
			b := pos{p.row, p.col - 1}
			if p.col == 0 && p.row > a.row {
				b = pos{p.row - 1, len(e.line(p.row-1)) - 1}
			}
			return a, b, true
		}
	}
}

// ---- operators ----

func (e *editor) applyOp(op string, a, b pos, linewise, inclusive bool) {
	if linewise {
		e.opLines(op, min(a.row, b.row), max(a.row, b.row))
		return
	}
	if op == ">" || op == "<" {
		e.opLines(op, min(a.row, b.row), max(a.row, b.row))
		return
	}
	a, b = order(a, b)
	end := b
	if inclusive {
		if b.col < 0 || b.before(a) {
			// empty range, e.g. di( on ()
			end = a
		} else if b.col >= len(e.line(b.row)) && b.row < e.last() {
			end = pos{b.row + 1, 0}
		} else {
			end = pos{b.row, min(b.col+1, len(e.line(b.row)))}
		}
	}
	end.col = min(end.col, len(e.line(end.row)))
	switch op {
	case "y":
		e.reg.text, e.reg.linewise = e.textRange(a, end), false
		e.cur = a
	case "d", "c":
		e.save()
		e.reg.text, e.reg.linewise = e.textRange(a, end), false
		e.deleteRange(a, end)
		e.cur = a
		if op == "c" {
			e.mode = vInsert
		}
	}
	e.clamp()
}

func (e *editor) opLines(op string, r1, r2 int) {
	var sb strings.Builder
	for r := r1; r <= r2; r++ {
		sb.WriteString(string(e.lines[r]))
		sb.WriteByte('\n')
	}
	switch op {
	case "y":
		e.reg.text, e.reg.linewise = sb.String(), true
		e.cur.row = r1
	case "d":
		e.save()
		e.reg.text, e.reg.linewise = sb.String(), true
		e.lines = append(e.lines[:r1], e.lines[r2+1:]...)
		if len(e.lines) == 0 {
			e.lines = [][]rune{{}}
		}
		e.cur.row = min(r1, e.last())
		e.cur.col = firstNonBlank(e.line(e.cur.row))
	case "c":
		e.save()
		e.reg.text, e.reg.linewise = sb.String(), true
		indent := []rune(leadingSpace(e.lines[r1]))
		e.lines = append(e.lines[:r1], append([][]rune{indent}, e.lines[r2+1:]...)...)
		e.cur = pos{r1, len(indent)}
		e.mode = vInsert
	case ">", "<":
		e.save()
		for r := r1; r <= r2; r++ {
			l := e.lines[r]
			if op == ">" {
				if len(l) > 0 {
					e.lines[r] = append([]rune(indentUnit), l...)
				}
				continue
			}
			n := 0
			for n < len(indentUnit) && n < len(l) && l[n] == ' ' {
				n++
			}
			if n == 0 && len(l) > 0 && l[0] == '\t' {
				n = 1
			}
			e.lines[r] = l[n:]
		}
		e.cur = pos{r1, firstNonBlank(e.lines[r1])}
	}
	e.clamp()
}

func (e *editor) textRange(a, b pos) string {
	if a.row == b.row {
		return string(e.lines[a.row][a.col:b.col])
	}
	var sb strings.Builder
	sb.WriteString(string(e.lines[a.row][a.col:]))
	for r := a.row + 1; r < b.row; r++ {
		sb.WriteByte('\n')
		sb.WriteString(string(e.lines[r]))
	}
	sb.WriteByte('\n')
	sb.WriteString(string(e.lines[b.row][:b.col]))
	return sb.String()
}

func (e *editor) deleteRange(a, b pos) {
	head := append([]rune{}, e.lines[a.row][:a.col]...)
	tail := e.lines[b.row][b.col:]
	e.lines[a.row] = append(head, tail...)
	e.lines = append(e.lines[:a.row+1], e.lines[b.row+1:]...)
}

func (e *editor) put(before bool, count int) {
	if e.reg.text == "" {
		return
	}
	e.save()
	if e.reg.linewise {
		text := strings.TrimSuffix(e.reg.text, "\n")
		var block [][]rune
		for range count {
			for _, s := range strings.Split(text, "\n") {
				block = append(block, []rune(s))
			}
		}
		at := e.cur.row + 1
		if before {
			at = e.cur.row
		}
		e.lines = append(e.lines[:at], append(block, e.lines[at:]...)...)
		e.cur = pos{at, firstNonBlank(e.lines[at])}
		return
	}
	if !before && len(e.line(e.cur.row)) > 0 {
		e.cur.col++
	}
	e.mode = vInsert // let insertText place the cursor past the end
	e.insertText(strings.Repeat(e.reg.text, count))
	e.mode = vNormal
	e.cur.col--
	e.clamp()
}

func (e *editor) join(r int) {
	next := strings.TrimLeft(string(e.lines[r+1]), " \t")
	cur := strings.TrimRight(string(e.lines[r]), " \t")
	sep := " "
	if cur == "" || next == "" || strings.HasPrefix(next, ")") {
		sep = ""
	}
	e.lines[r] = []rune(cur)
	e.lines[r+1] = []rune(next)
	e.joinRaw(r, sep)
	e.cur = pos{r, len([]rune(cur))}
	e.clamp()
}

// replaceInLine swaps runes [from, to) on row for text and puts the
// cursor after it. Used for accepting completions.
func (e *editor) replaceInLine(row, from, to int, text string) {
	l := e.lines[row]
	from, to = max(0, min(from, len(l))), max(0, min(to, len(l)))
	r := []rune(text)
	nl := append(append(append([]rune{}, l[:from]...), r...), l[to:]...)
	e.lines[row] = nl
	e.cur = pos{row, from + len(r)}
}

func leadingSpace(l []rune) string {
	n := 0
	for n < len(l) && (l[n] == ' ' || l[n] == '\t') {
		n++
	}
	return string(l[:n])
}

func firstNonBlank(l []rune) int {
	for i, r := range l {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return max(len(l)-1, 0)
}

func toggleCase(r rune) rune {
	if unicode.IsUpper(r) {
		return unicode.ToLower(r)
	}
	return unicode.ToUpper(r)
}
