package ui

import (
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

	if p := m.pendingKey; p != "" {
		m.pendingKey = ""
		switch p + key {
		case "dd":
			m.deleteCell()
		case "yy":
			m.yank = m.cell().Clone()
			m.msg = "yanked cell"
		case "gg":
			m.sel = 0
		}
		return nil
	}

	switch key {
	case "d", "y", "g":
		m.pendingKey = key
	case "j", "down":
		m.sel = min(m.sel+1, len(m.nb.Cells)-1)
	case "k", "up":
		m.sel = max(m.sel-1, 0)
	case "G", "end":
		m.sel = len(m.nb.Cells) - 1
	case "home":
		m.sel = 0
	case "ctrl+d":
		m.offset += m.bodyHeight() / 2
		m.manualScroll = true
	case "ctrl+u":
		m.offset -= m.bodyHeight() / 2
		m.manualScroll = true

	case "enter", "i":
		return m.startEdit(false)
	case "A":
		return m.startEdit(true)
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
	case "ctrl+s":
		m.save()
	case ":":
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

func (m *Model) editKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.stopEdit()
		return nil
	case "shift+enter", "ctrl+r":
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
		return nil
	case "tab":
		m.ta.InsertString("    ")
		m.sizeEditor()
		return nil
	}
	m.sizeEditor()
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	m.sizeEditor()
	return cmd
}

func (m *Model) cmdKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = normalMode
		m.cmd.Blur()
		return nil
	case "enter":
		line := strings.TrimSpace(m.cmd.Value())
		m.mode = normalMode
		m.cmd.Blur()
		return m.runCommand(line)
	case "backspace":
		if m.cmd.Value() == "" {
			m.mode = normalMode
			m.cmd.Blur()
			return nil
		}
	}
	var cmd tea.Cmd
	m.cmd, cmd = m.cmd.Update(msg)
	return cmd
}

func (m *Model) runCommand(line string) tea.Cmd {
	if n, err := strconv.Atoi(line); err == nil {
		m.sel = max(0, min(n-1, len(m.nb.Cells)-1))
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
	return m.startEdit(false)
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
