// Package ui is the Bubble Tea front end.
package ui

import (
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
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
	cmdMode
)

type runState int

const (
	queued runState = iota
	running
)

// KernelMsg delivers the result of starting a kernel.
type KernelMsg struct {
	Kernel *kernel.Kernel
	Err    error
}

type statusMsg struct {
	k     *kernel.Kernel
	state string
}

type eventMsg struct {
	k    *kernel.Kernel
	cell *notebook.Cell
	ch   <-chan kernel.Event
	ev   kernel.Event
	ok   bool
}

type interruptMsg struct{ err error }

type tickMsg struct{}

type deleted struct {
	idx  int
	cell *notebook.Cell
}

type Model struct {
	path string
	nb   *notebook.Notebook
	lang string

	host   *kernelHost
	k      *kernel.Kernel
	kstate string

	sel    int
	mode   mode
	ta     textarea.Model
	cmd    textinput.Model
	offset int
	// set by ctrl+d/u so the view stops snapping to the selection
	manualScroll bool

	pendingKey string // first key of dd, yy, gg
	yank       *notebook.Cell
	undo       []deleted
	help       bool
	ext        *extEdit

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

func New(path string, nb *notebook.Notebook, opts kernel.Options) *Model {
	if len(nb.Cells) == 0 {
		nb.Insert(0, notebook.NewCell(notebook.Code))
	}
	m := &Model{
		path:    path,
		nb:      nb,
		lang:    nb.Language(),
		host:    newKernelHost(opts),
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
	m.cmd = textinput.New()
	m.cmd.Prompt = ":"
	m.cmd.SetVirtualCursor(false)
	m.applyTheme()
	return m
}

// Close shuts down kernels and cleans temp files. Call after Run returns.
func (m *Model) Close() {
	m.host.Close()
	m.ext.cleanup()
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, m.host.start(nil))
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.sizeEditor()
		m.cmd.SetWidth(max(m.width-4, 10))
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
			m.runs = map[*notebook.Cell]runState{}
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
		if msg.k != m.k {
			return m, nil
		}
		m.kstate = msg.state
		if msg.state == "dead" {
			m.msg = "kernel died, :restart to start a new one"
			return m, nil
		}
		return m, m.waitStatus()

	case eventMsg:
		return m, m.handleEvent(msg)

	case interruptMsg:
		if msg.err != nil {
			m.msg = "interrupt failed: " + msg.err.Error()
		}
		return m, nil

	case tickMsg:
		m.frame++
		if len(m.runs) == 0 {
			m.ticking = false
			return m, nil
		}
		return m, tick()

	case editorDoneMsg, editorPollMsg:
		return m, m.handleEditor(msg)

	case tea.KeyPressMsg:
		if m.help {
			m.help = false
			return m, nil
		}
		switch m.mode {
		case editMode:
			return m, m.editKey(msg)
		case cmdMode:
			return m, m.cmdKey(msg)
		}
		return m, m.normalKey(msg)
	}

	var cmd tea.Cmd
	switch m.mode {
	case editMode:
		m.ta, cmd = m.ta.Update(msg)
	case cmdMode:
		m.cmd, cmd = m.cmd.Update(msg)
	}
	return m, cmd
}

func (m *Model) startEdit(atEnd bool) tea.Cmd {
	m.mode = editMode
	m.ta.SetValue(m.cell().Source)
	if atEnd {
		m.ta.MoveToEnd()
	} else {
		m.ta.MoveToBegin()
	}
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

func (m *Model) runAll() tea.Cmd {
	var cmds []tea.Cmd
	for _, c := range m.nb.Cells {
		cmds = append(cmds, m.execute(c))
	}
	return tea.Batch(cmds...)
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
		m.msg = "kernel is not running, :restart to start one"
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
	return waitEvent(m.k, c, ch)
}

func (m *Model) handleEvent(e eventMsg) tea.Cmd {
	if !e.ok {
		return nil
	}
	// keep draining channels of a kernel we restarted away from, but
	// don't let them touch cells
	if e.k != m.k {
		return waitEvent(e.k, e.cell, e.ch)
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
	return waitEvent(e.k, c, e.ch)
}

func (m *Model) interrupt() tea.Cmd {
	// cells still waiting for the kernel to come up just get dropped
	for _, c := range m.pending {
		delete(m.runs, c)
	}
	m.pending = nil
	k := m.k
	if k == nil {
		return nil
	}
	m.msg = "interrupting"
	return func() tea.Msg { return interruptMsg{k.Interrupt()} }
}

func (m *Model) restart() tea.Cmd {
	old := m.k
	m.k = nil
	m.kstate = "restarting"
	m.runs = map[*notebook.Cell]runState{}
	m.pending = nil
	m.msg = "restarting kernel"
	return m.host.start(old)
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
			return statusMsg{k, s}
		case <-k.Dead():
			return statusMsg{k, "dead"}
		}
	}
}

func waitEvent(k *kernel.Kernel, c *notebook.Cell, ch <-chan kernel.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return eventMsg{k: k, cell: c, ch: ch, ev: ev, ok: ok}
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
