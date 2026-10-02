// Package ui is the Bubble Tea front end.
package ui

import (
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/notebook"
)

type mode int

const (
	normalMode mode = iota
	editMode
)

type runState int

const (
	queued runState = iota
	running
)

// KernelMsg delivers the result of starting the kernel, which happens
// outside the program so the UI is up immediately.
type KernelMsg struct {
	Kernel *kernel.Kernel
	Err    error
}

type statusMsg string

type eventMsg struct {
	cell *notebook.Cell
	ch   <-chan kernel.Event
	ev   kernel.Event
	ok   bool
}

type tickMsg struct{}

type Model struct {
	path string
	nb   *notebook.Notebook
	lang string

	k      *kernel.Kernel
	kstate string

	sel    int
	mode   mode
	ta     textarea.Model
	offset int
	// set by ctrl+d/u so the view stops snapping to the selection
	manualScroll bool

	width, height int
	dirty         bool
	msg           string
	quitArmed     bool

	// keyed by cell pointer so runs survive cells moving around
	runs    map[*notebook.Cell]runState
	pending []*notebook.Cell
	frame   int
	ticking bool

	dark    bool
	st      styles
	hlCache map[string]string
	mdCache map[string]string
	mdr     *glamour.TermRenderer
	mdrW    int
}

func New(path string, nb *notebook.Notebook) *Model {
	if len(nb.Cells) == 0 {
		nb.Insert(0, notebook.NewCell(notebook.Code))
	}
	m := &Model{
		path:    path,
		nb:      nb,
		lang:    nb.Language(),
		kstate:  "starting",
		runs:    map[*notebook.Cell]runState{},
		dark:    true,
		hlCache: map[string]string{},
		mdCache: map[string]string{},
	}
	m.ta = textarea.New()
	m.ta.ShowLineNumbers = false
	m.ta.Prompt = ""
	m.ta.CharLimit = 0
	m.ta.MaxHeight = 0
	m.ta.SetVirtualCursor(false)
	m.applyTheme()
	return m
}

func (m *Model) Init() tea.Cmd {
	return tea.RequestBackgroundColor
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sizeEditor()
		return m, nil

	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.applyTheme()
		return m, nil

	case KernelMsg:
		if msg.Err != nil {
			m.kstate = "error"
			m.msg = "kernel failed: " + firstLine(msg.Err.Error())
			m.pending = nil
			for c := range m.runs {
				delete(m.runs, c)
			}
			return m, nil
		}
		m.k = msg.Kernel
		m.kstate = "idle"
		cmds := []tea.Cmd{m.waitStatus()}
		for _, c := range m.pending {
			cmds = append(cmds, m.submit(c))
		}
		m.pending = nil
		return m, tea.Batch(cmds...)

	case statusMsg:
		m.kstate = string(msg)
		if msg == "dead" {
			m.msg = "kernel died"
			return m, nil
		}
		return m, m.waitStatus()

	case eventMsg:
		return m, m.handleEvent(msg)

	case tickMsg:
		m.frame++
		if len(m.runs) == 0 {
			m.ticking = false
			return m, nil
		}
		return m, tick()

	case tea.KeyPressMsg:
		if m.mode == editMode {
			return m, m.editKey(msg)
		}
		return m, m.normalKey(msg)
	}

	if m.mode == editMode {
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *Model) normalKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if key != "q" && key != "ctrl+c" {
		m.quitArmed = false
	}
	m.msg = ""
	m.manualScroll = false
	switch key {
	case "j", "down":
		m.sel = min(m.sel+1, len(m.nb.Cells)-1)
	case "k", "up":
		m.sel = max(m.sel-1, 0)
	case "g", "home":
		m.sel = 0
	case "G", "end":
		m.sel = len(m.nb.Cells) - 1
	case "ctrl+d":
		m.offset += m.bodyHeight() / 2
		m.manualScroll = true
	case "ctrl+u":
		m.offset -= m.bodyHeight() / 2
		m.manualScroll = true
	case "enter", "i":
		return m.startEdit()
	case "shift+enter", "ctrl+r":
		return m.runAndAdvance()
	case "ctrl+s":
		m.save()
	case "q", "ctrl+c":
		if m.dirty && !m.quitArmed {
			m.quitArmed = true
			m.msg = "unsaved changes: ctrl+s to save, q again to quit anyway"
			return nil
		}
		return tea.Quit
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

func (m *Model) startEdit() tea.Cmd {
	m.mode = editMode
	m.ta.SetValue(m.cell().Source)
	m.ta.MoveToBegin()
	m.sizeEditor()
	return m.ta.Focus()
}

func (m *Model) commitEdit() {
	if c := m.cell(); c.Source != m.ta.Value() {
		c.Source = m.ta.Value()
		m.dirty = true
	}
}

func (m *Model) stopEdit() {
	m.commitEdit()
	m.ta.Blur()
	m.mode = normalMode
}

// sizeEditor grows the textarea to fit its content so it never scrolls
// internally; the notebook view does the scrolling. One spare row avoids
// a jump when a newline is typed.
func (m *Model) sizeEditor() {
	w := max(m.boxWidth()-4, 10)
	m.ta.SetWidth(w)
	rows := 1
	for _, l := range strings.Split(m.ta.Value(), "\n") {
		rows += max(1, (ansi.StringWidth(expandTabs(l))+w-1)/w)
	}
	m.ta.SetHeight(rows)
}

func (m *Model) runAndAdvance() tea.Cmd {
	c := m.cell()
	cmd := m.execute(c)
	if m.sel == len(m.nb.Cells)-1 {
		m.nb.Insert(m.sel+1, notebook.NewCell(notebook.Code))
		m.dirty = true
	}
	m.sel++
	return cmd
}

func (m *Model) execute(c *notebook.Cell) tea.Cmd {
	if c.Type != notebook.Code {
		return nil
	}
	if _, busy := m.runs[c]; busy {
		return nil
	}
	switch m.kstate {
	case "error", "dead":
		m.msg = "kernel is not running"
		return nil
	}
	c.ClearOutputs()
	m.dirty = true
	m.runs[c] = queued
	var cmd tea.Cmd
	if m.k == nil {
		m.pending = append(m.pending, c)
	} else {
		cmd = m.submit(c)
	}
	return tea.Batch(cmd, m.startTick())
}

func (m *Model) submit(c *notebook.Cell) tea.Cmd {
	ch, err := m.k.Execute(c.Source)
	if err != nil {
		delete(m.runs, c)
		m.msg = "execute failed: " + err.Error()
		return nil
	}
	return waitEvent(c, ch)
}

func (m *Model) handleEvent(e eventMsg) tea.Cmd {
	if !e.ok {
		return nil
	}
	c := e.cell
	switch e.ev.Kind {
	case kernel.EvStarted:
		m.runs[c] = running
		n := e.ev.ExecCount
		c.ExecutionCount = &n
	case kernel.EvOutput:
		c.Outputs = mergeStream(c.Outputs, e.ev.Output)
		m.dirty = true
	case kernel.EvClear:
		c.Outputs = nil
	case kernel.EvDone:
		delete(m.runs, c)
		if e.ev.ExecCount > 0 {
			n := e.ev.ExecCount
			c.ExecutionCount = &n
		}
		if e.ev.Err != nil {
			m.msg = firstLine(e.ev.Err.Error())
		}
	}
	return waitEvent(c, e.ch)
}

func (m *Model) save() {
	if err := m.nb.Save(m.path); err != nil {
		m.msg = "save failed: " + err.Error()
		return
	}
	m.dirty = false
	m.msg = "saved " + filepath.Base(m.path)
}

func (m *Model) cell() *notebook.Cell { return m.nb.Cells[m.sel] }

func (m *Model) waitStatus() tea.Cmd {
	k := m.k
	return func() tea.Msg {
		select {
		case s := <-k.Status():
			return statusMsg(s)
		case <-k.Dead():
			return statusMsg("dead")
		}
	}
}

func waitEvent(c *notebook.Cell, ch <-chan kernel.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return eventMsg{cell: c, ch: ch, ev: ev, ok: ok}
	}
}

func (m *Model) startTick() tea.Cmd {
	if m.ticking {
		return nil
	}
	m.ticking = true
	return tick()
}

func tick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}
