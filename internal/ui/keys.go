package ui

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// maxUndo bounds the deleted-cell stack.
const maxUndo = 50

func (m *Model) normalKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if key != "q" && key != "ctrl+c" {
		m.quitArmed = false
	}
	m.msg = ""
	m.manualScroll = false

	// counts: 5j, 12G, 3gg
	if len(key) == 1 && key[0] >= '0' && key[0] <= '9' && (key != "0" || m.count != "") {
		m.count += key
		return nil
	}
	n, _ := strconv.Atoi(m.count)
	hasCount := m.count != ""
	if m.pendingKey == "" {
		defer func() {
			if m.pendingKey == "" {
				m.count = ""
			}
		}()
	}
	// with a count, j/k/G/gg go by the line numbers on screen (one
	// numbering for the whole notebook) and open the cell at that line;
	// without one they move between cells
	jump := func(def int) {
		if hasCount {
			m.gotoLine(n-1, -2)
		} else {
			m.sel = def
		}
	}

	if p := m.pendingKey; p != "" {
		m.pendingKey = ""
		m.count = ""
		switch p + key {
		case "]d":
			m.jumpDiag(1)
		case "[d":
			m.jumpDiag(-1)
		case "dd":
			m.deleteCell()
		case "yy":
			m.yank = m.cell().Clone()
			m.msg = "yanked cell"
		case "gg":
			jump(0)
		case "gx":
			m.openImage()
		case "gv":
			return m.openVars()
		}
		return nil
	}

	switch key {
	case "d", "y", "g", "]", "[":
		m.pendingKey = key
	case "j", "down":
		if hasCount {
			m.gotoLine(m.lineStarts()[m.sel]+n, -2)
		} else {
			m.sel = min(m.sel+1, len(m.nb.Cells)-1)
		}
	case "k", "up":
		if hasCount {
			m.gotoLine(m.lineStarts()[m.sel]-n, -2)
		} else {
			m.sel = max(m.sel-1, 0)
		}
	case "G", "end":
		jump(len(m.nb.Cells) - 1)
	case "home":
		m.sel = 0
	case "ctrl+d":
		m.offset += m.bodyHeight() / 2
		m.manualScroll = true
	case "ctrl+u":
		m.offset -= m.bodyHeight() / 2
		m.manualScroll = true

	case "enter":
		return m.startEdit("normal")
	case "i":
		return m.startEdit("insert")
	case "A":
		return m.startEdit("append")
	case "s":
		m.startFlash()
	case "o":
		return m.insertCell(m.sel + 1)
	case "O":
		return m.insertCell(m.sel)
	case "p", "P":
		if m.yank == nil {
			m.msg = "nothing yanked"
			return nil
		}
		at := m.sel + 1
		if key == "P" {
			at = m.sel
		}
		m.nb.Insert(at, m.yank.Clone())
		m.sel = at
		m.dirty = true
	case "u":
		m.undoDelete()
	case "J":
		if m.nb.Move(m.sel, 1) {
			m.sel++
			m.dirty = true
		}
	case "K":
		if m.nb.Move(m.sel, -1) {
			m.sel--
			m.dirty = true
		}
	case "M":
		m.convert(notebook.Markdown)
	case "C":
		m.convert(notebook.Code)
	case "R":
		m.convert(notebook.Raw)
	case "x":
		if len(m.cell().Outputs) > 0 || m.cell().ExecutionCount != nil {
			m.cell().ClearOutputs()
			m.dirty = true
		}
	case "e":
		return m.openEditor(m.cell())

	case "shift+enter", "ctrl+r":
		return m.runAndAdvance()
	case "ctrl+enter":
		return m.execute(m.cell())
	case "ctrl+s":
		m.save()
	case ":":
		m.cmdFrom = normalMode
		m.mode = cmdMode
		m.cmd.Reset()
		return m.cmd.Focus()
	case "?":
		m.help = true
	case "ctrl+c":
		if len(m.runs) > 0 {
			return m.interrupt()
		}
		return m.quit(false)
	case "q":
		return m.quit(false)
	}
	return nil
}

// keyTok turns a key press into what the vim editor expects: the typed
// text for printable keys, otherwise the key's name.
func keyTok(msg tea.KeyPressMsg) string {
	k := msg.Key()
	if k.Text != "" && k.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
		return k.Text
	}
	return msg.String()
}

func (m *Model) editKey(msg tea.KeyPressMsg) tea.Cmd {
	m.msg = ""
	e := m.ed
	switch msg.String() {
	case "ctrl+enter":
		m.commitEdit()
		return m.execute(m.cell())
	case "shift+enter":
		m.stopEdit()
		return m.runAndAdvance()
	case "ctrl+s":
		m.commitEdit()
		m.save()
		return nil
	case "ctrl+e":
		m.stopEdit()
		return m.openEditor(m.cell())
	case "ctrl+c":
		if len(m.runs) > 0 {
			return m.interrupt()
		}
	}
	tok := keyTok(msg)
	if e.mode == vInsert {
		if m.comp != nil {
			if ok, cmd := m.compKey(msg.String()); ok {
				return cmd
			}
		}
		switch msg.String() {
		case "ctrl+space", "ctrl+n":
			return m.requestCompletion()
		case "tab":
			if wantsCompletion(e) {
				return m.requestCompletion()
			}
		}
	}
	if e.vim && e.mode == vNormal {
		if b := m.bracket; b != "" {
			m.bracket = ""
			if tok == "d" {
				if b == "]" {
					m.jumpDiag(1)
				} else {
					m.jumpDiag(-1)
				}
			}
			return nil
		}
		if len(e.keys) == 1 && e.keys[0] == "g" && tok == "d" {
			e.keys = nil
			return m.definition()
		}
		if len(e.keys) == 1 && e.keys[0] == "g" && tok == "x" {
			e.keys = nil
			m.openImage()
			return nil
		}
	}
	if e.vim && e.mode == vNormal && len(e.keys) == 0 {
		switch tok {
		case "]", "[":
			m.bracket = tok
			return nil
		case "K":
			return m.hover()
		case ":":
			m.commitEdit()
			m.cmdFrom = editMode
			m.mode = cmdMode
			m.cmd.Reset()
			return m.cmd.Focus()
		case "s":
			// s is flash, like LazyVim; cl still does what s used to
			m.startFlash()
			return nil
		}
	}
	res := e.key(tok)
	m.commitEdit()
	compCmd := m.afterInsertKey(tok)
	if e.mode != vInsert {
		m.sig = nil
		m.releaseDiags()
	} else {
		switch tok {
		case "(", ",":
			compCmd = tea.Batch(compCmd, m.signatureHelp())
		case ")":
			m.sig = nil
		}
	}
	switch res {
	case edLeave:
		m.stopEdit()
	case edCross:
		m.gotoLine(m.lineStarts()[m.sel]+e.cur.row+e.jump, e.want)
	case edGoto:
		m.gotoLine(e.gotoLine-1, -2)
	}
	return compCmd
}

// lineStarts is the notebook-wide number of each cell's first source
// line, plus the total at the end. Outputs don't count.
func (m *Model) lineStarts() []int {
	starts := make([]int, len(m.nb.Cells)+1)
	for i, c := range m.nb.Cells {
		n := strings.Count(c.Source, "\n") + 1
		if m.mode == editMode && m.ed != nil && i == m.sel {
			n = len(m.ed.lines)
		}
		starts[i+1] = starts[i] + n
	}
	return starts
}

// gotoLine opens the cell holding notebook line g (0-based) in vim
// normal mode. col -1 means end of line, -2 first non-blank.
func (m *Model) gotoLine(g, col int) {
	starts := m.lineStarts()
	g = max(0, min(g, starts[len(starts)-1]-1))
	cell := 0
	for cell+1 < len(m.nb.Cells) && starts[cell+1] <= g {
		cell++
	}
	if m.mode == editMode {
		m.stopEdit()
	}
	m.sel = cell
	m.startEdit("normal")
	e := m.ed
	e.cur.row = g - starts[cell]
	switch {
	case col == -2:
		e.cur.col = firstNonBlank(e.line(e.cur.row))
	case col < 0:
		e.cur.col = len(e.line(e.cur.row))
	default:
		e.cur.col = col
	}
	e.clamp()
	if col != -2 {
		e.want = col
	}
}

func (m *Model) cmdKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.leaveCmd()
		return nil
	case "enter":
		line := strings.TrimSpace(m.cmd.Value())
		m.leaveCmd()
		return m.runCommand(line)
	case "backspace":
		if m.cmd.Value() == "" {
			m.leaveCmd()
			return nil
		}
	}
	var cmd tea.Cmd
	m.cmd, cmd = m.cmd.Update(msg)
	return cmd
}

// leaveCmd goes back to wherever : was pressed from.
func (m *Model) leaveCmd() {
	m.cmd.Blur()
	m.mode = normalMode
	if m.cmdFrom == editMode && m.ed != nil {
		m.mode = editMode
	}
}

func (m *Model) runCommand(line string) tea.Cmd {
	// :42 is notebook line 42, matching the line numbers
	if n, err := strconv.Atoi(line); err == nil {
		m.gotoLine(n-1, -2)
		return nil
	}
	if cmd, arg, _ := strings.Cut(line, " "); cmd == "export" || cmd == "export!" {
		m.export(strings.TrimSpace(arg), cmd == "export!")
		return nil
	}
	if name, ok := strings.CutPrefix(line, "view "); ok {
		return m.openFrame(strings.TrimSpace(name))
	}
	if opt, ok := strings.CutPrefix(line, "set "); ok {
		opt = strings.TrimSpace(opt)
		if v, ok := strings.CutPrefix(opt, "images="); ok {
			return m.setGraphics(v)
		}
		m.setOption(opt)
		return nil
	}
	switch line {
	case "":
	case "w":
		m.save()
	case "q":
		return m.quit(false)
	case "q!":
		return m.quit(true)
	case "wq", "x":
		m.save()
		if !m.dirty {
			return tea.Quit
		}
	case "restart":
		return m.restart()
	case "env":
		return m.openEnvPicker()
	case "vars":
		return m.openVars()
	case "runall", "ra":
		return m.runAll()
	case "clear":
		for _, c := range m.nb.Cells {
			if len(c.Outputs) > 0 || c.ExecutionCount != nil {
				c.ClearOutputs()
				m.dirty = true
			}
		}
	case "interrupt", "int":
		return m.interrupt()
	case "help", "h":
		m.help = true
	default:
		m.msg = "unknown command: " + line
	}
	return nil
}

func (m *Model) export(path string, force bool) {
	m.commitEdit()
	if path == "" {
		path = notebook.PyPath(m.path)
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(m.path), path)
	}
	if err := m.nb.ExportPercent(path, force); err != nil {
		m.msg = "export: " + err.Error()
		if errors.Is(err, fs.ErrExist) {
			m.msg = filepath.Base(path) + " exists, :export! to overwrite"
		}
		return
	}
	m.msg = "exported " + filepath.Base(path)
}

func (m *Model) setGraphics(v string) tea.Cmd {
	g, ok := parseGfx(v)
	if !ok {
		m.msg = "images= kitty, sixel or blocks"
		return nil
	}
	old := m.gfxMode
	m.gfxMode = g
	m.sixelDrawn = nil
	for _, gi := range m.gfx {
		gi.sent = false
	}
	m.msg = "images: " + g.String()
	var cmds []tea.Cmd
	if old == gfxKitty {
		cmds = append(cmds, tea.Raw("\x1b_Ga=d,d=A,q=2\x1b\\"))
	}
	if old == gfxSixel || g == gfxSixel {
		cmds = append(cmds, tea.ClearScreen)
	}
	if g == gfxSixel && old != gfxSixel {
		cmds = append(cmds, sixelTick())
	}
	return tea.Batch(append(cmds, m.syncKitty())...)
}

func (m *Model) setOption(opt string) {
	switch opt {
	case "nu", "number":
		m.number = true
	case "nonu", "nonumber":
		m.number = false
	case "rnu", "relativenumber":
		m.relative = true
	case "nornu", "norelativenumber":
		m.relative = false
	case "lsp":
		m.lspOn = true
		if m.lspState == "failed" {
			m.lspState = ""
		}
	case "nolsp":
		m.lspOn = false
		if m.lsp != nil {
			go m.lsp.Close()
			m.lsp, m.lspState, m.diags = nil, "", nil
		}
	case "diag":
		m.diagOn = true
	case "nodiag":
		m.diagOn = false
	case "vim":
		m.vimOn = true
		if m.ed != nil {
			m.ed.vim = true
			m.ed.setMode(vNormal)
		}
	case "novim":
		m.vimOn = false
		if m.ed != nil {
			m.ed.vim = false
			m.ed.keys = nil
			m.ed.setMode(vInsert)
		}
	default:
		m.msg = "unknown option: " + opt
	}
}

func (m *Model) quit(force bool) tea.Cmd {
	if m.dirty && !force && !m.quitArmed {
		m.quitArmed = true
		m.msg = "unsaved changes: :w to save, q again or :q! to quit anyway"
		return nil
	}
	return tea.Quit
}

func (m *Model) insertCell(at int) tea.Cmd {
	m.nb.Insert(at, notebook.NewCell(notebook.Code))
	m.sel = at
	m.dirty = true
	return m.startEdit("insert")
}

func (m *Model) deleteCell() {
	c := m.nb.Delete(m.sel)
	if c == nil {
		return
	}
	m.undo = append(m.undo, deleted{m.sel, c})
	if len(m.undo) > maxUndo {
		m.undo = m.undo[1:]
	}
	if len(m.nb.Cells) == 0 {
		m.nb.Insert(0, notebook.NewCell(notebook.Code))
	}
	m.sel = min(m.sel, len(m.nb.Cells)-1)
	m.dirty = true
}

func (m *Model) undoDelete() {
	if len(m.undo) == 0 {
		m.msg = "nothing to undo"
		return
	}
	d := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	m.nb.Insert(d.idx, d.cell)
	m.sel = min(d.idx, len(m.nb.Cells)-1)
	m.dirty = true
}

func (m *Model) convert(t notebook.CellType) {
	c := m.cell()
	if _, busy := m.runs[c]; busy {
		m.msg = "cell is running"
		return
	}
	if c.Type != t {
		c.SetType(t)
		m.dirty = true
	}
}
