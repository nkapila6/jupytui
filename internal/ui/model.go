// Package ui is the Bubble Tea front end.
package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"github.com/nkapila6/jupytui/internal/envs"
	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/lsp"
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
	cmd    textinput.Model
	offset int
	layout []layoutRow
	starts []int // notebook-wide first line of each cell, set per render

	// per-cell editors keep undo history while the app runs
	ed      *editor
	eds     map[*notebook.Cell]*editor
	reg     register
	cmdFrom mode
	flash   *flashState

	vimOn, number, relative bool
	// set by ctrl+d/u so the view stops snapping to the selection
	manualScroll bool

	pendingKey string // first key of dd, yy, gg
	count      string // count typed before a cell command, as in 5j
	yank       *notebook.Cell
	undo       []deleted
	help       bool
	ext        *extEdit
	picker     *envPicker
	msgIDs     map[*notebook.Cell]string // request id of each cell's latest run
	attach     *Attach                   // reattaching to a detached session
	detached   bool
	remote     *remoteInfo
	vars       *varsPanel
	input      *inputState // a cell waiting on input()

	search     string // last / pattern
	hlsearch   bool
	searching  bool
	searchIn   textinput.Model
	searchFrom mode
	folded     map[*notebook.Cell]bool
	outline    *outlinePanel

	// stale tracking (stale.go)
	deps     map[*notebook.Cell]cellDeps
	depsSrc  map[*notebook.Cell]string
	depsBusy bool
	depsSeq  int
	anonIDs  map[*notebook.Cell]string
	ran      map[*notebook.Cell]runInfo
	execSrc  map[*notebook.Cell]string
	statuses map[*notebook.Cell]cellStatus // per render
	reactive bool

	noSaveOutputs bool // :set nosaveoutputs
	dfv           *frameView
	comp          *compState
	compSeq       int // latest completion request
	compFrom      int // request the open popup was built from

	lsp       *lsp.Client
	lspState  string // "", starting, ready, failed
	lspOn     bool
	diagOn    bool
	lspCtx    context.Context
	lspCancel context.CancelFunc
	doc       lspDoc
	docVer    int
	diags     map[*notebook.Cell][]cellDiag
	hoverText string
	sig       *lsp.Signature
	sigSeq    int
	bracket   string // first key of ]d / [d
	heldDiags *lsp.Notification
	env       envs.Env

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
	// rendered dataframe tables by width + html
	tableCache map[string]string
	imgCache   map[imgKey][]string

	gfxMode    gfxMode
	cellW      int
	cellH      int
	gfx        map[*notebook.Output]*gfxImage
	nextGfxID  int
	sixelWant  []sixelPlace // set by View
	sixelDrawn []sixelPlace
	sixelCache map[string]string
	imgDir     string // temp files for gx
	imgN       int
	mdr        *glamour.TermRenderer
	mdrW       int
}

func New(path string, nb *notebook.Notebook, opts kernel.Options, attach *Attach) *Model {
	if len(nb.Cells) == 0 {
		nb.Insert(0, notebook.NewCell(notebook.Code))
	}
	// reopen in the env picked last time with :env
	env, ok := envs.Resolve(nb.JupytuiPython(), filepath.Dir(path))
	var startMsg string
	if !ok {
		startMsg = "saved env " + nb.JupytuiPython() + " is gone, using the project env"
		env, _ = envs.Resolve("", "")
	}
	opts.Cmd = kernelCmd(env)
	opts.Remote = remoteFor(env)
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
		eds:     map[*notebook.Cell]*editor{},
		vimOn:   true,
		lspOn:   true,
		diagOn:  true,
		env:     env,
		remote:  remoteInfoFor(env),
		msg:     startMsg,
		number:  true,
		// relative by default, like LazyVim
		relative: true,
	}
	m.lspCtx, m.lspCancel = context.WithCancel(context.Background())
	m.gfxMode = detectGraphics()
	m.cellW, m.cellH = cellPixels()
	m.gfx = map[*notebook.Output]*gfxImage{}
	m.deps = map[*notebook.Cell]cellDeps{}
	m.depsSrc = map[*notebook.Cell]string{}
	m.anonIDs = map[*notebook.Cell]string{}
	m.ran = map[*notebook.Cell]runInfo{}
	m.execSrc = map[*notebook.Cell]string{}
	m.msgIDs = map[*notebook.Cell]string{}
	m.attach = attach
	m.sixelCache = map[string]string{}
	m.searchIn = textinput.New()
	m.searchIn.Prompt = "/"
	m.searchIn.SetVirtualCursor(false)
	m.folded = map[*notebook.Cell]bool{}
	m.cmd = textinput.New()
	m.cmd.Prompt = ":"
	m.cmd.SetVirtualCursor(false)
	m.applyTheme()
	return m
}

// Close shuts down kernels and cleans temp files. Call after Run returns.
func (m *Model) Close() {
	if s := m.kittyClear(); s != "" {
		os.Stdout.WriteString(s)
	}
	if m.imgDir != "" {
		os.RemoveAll(m.imgDir)
	}
	m.lspCancel()
	if m.lsp != nil {
		m.lsp.Close()
	}
	m.host.Close()
	m.ext.cleanup()
}

func (m *Model) Init() tea.Cmd {
	start := m.host.start(nil)
	if m.attach != nil {
		start = m.host.attach(m.attach.Session)
	}
	cmds := []tea.Cmd{tea.RequestBackgroundColor, start}
	if m.gfxMode == gfxSixel {
		cmds = append(cmds, sixelTick())
	}
	return tea.Batch(cmds...)
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.cmd.SetWidth(max(m.width-4, 10))
		m.cellW, m.cellH = cellPixels()
		return m, m.syncKitty()

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
		cmds := []tea.Cmd{m.waitStatus(), m.depsCmd()}
		if m.attach != nil {
			cmds = append(cmds, m.adopt(msg.Kernel))
		}
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
		cmd := m.handleEditor(msg)
		m.lspSync()
		return m, cmd

	case tea.PasteMsg:
		switch {
		case m.mode == editMode && m.ed != nil:
			m.ed.paste(msg.Content)
			m.commitEdit()
		case m.mode == cmdMode:
			var cmd tea.Cmd
			m.cmd, cmd = m.cmd.Update(msg)
			return m, cmd
		}
		return m, nil

	case sixelTickMsg:
		return m, m.handleSixelTick()

	case sixelDrawMsg:
		return m, m.handleSixelDraw(msg)

	case envsMsg:
		m.handleEnvs(msg)
		return m, nil

	case detachedMsg:
		return m, m.handleDetached(msg)

	case depsMsg:
		return m, m.handleDeps(msg)

	case varsMsg:
		m.handleVars(msg)
		return m, nil

	case reprMsg:
		if m.vars != nil {
			m.vars.detail = msg.text
			if msg.err != nil {
				m.vars.detail = msg.err.Error()
			}
		}
		return m, nil

	case frameMsg:
		m.handleFrame(msg)
		if m.dfv != nil {
			return m, m.ensureRows()
		}
		return m, nil

	case compMsg:
		m.handleCompletion(msg)
		return m, nil

	case lspReadyMsg:
		return m, m.handleLSPReady(msg)

	case lspNotifyMsg:
		if msg.c != m.lsp {
			return m, nil
		}
		m.handleDiagnostics(msg.n)
		return m, waitLSP(m.lsp)

	case lspDeadMsg:
		if msg.c == m.lsp {
			m.lsp = nil
			m.lspState = "failed"
			m.diags = nil
			m.msg = "language server exited"
		}
		return m, nil

	case hoverMsg:
		if strings.TrimSpace(msg.text) == "" {
			m.msg = "no hover info"
		}
		m.hoverText = msg.text
		return m, nil

	case defMsg:
		return m, m.handleDefinition(msg)

	case sigMsg:
		if msg.seq == m.sigSeq && m.mode == editMode && m.ed != nil && m.ed.mode == vInsert {
			m.sig = msg.sig
		}
		return m, nil

	case tea.KeyPressMsg:
		// popups like hover go away on the next key, which still counts
		m.hoverText = ""
		defer m.lspSync()
		if m.help {
			m.help = false
			return m, nil
		}
		if m.input != nil {
			return m, m.inputKey(msg)
		}
		if m.searching {
			return m, m.searchKey(msg)
		}
		if m.outline != nil {
			return m, m.outlineKey(msg)
		}
		if m.picker != nil {
			return m, m.pickerKey(msg)
		}
		if m.dfv != nil {
			return m, m.frameKey(msg)
		}
		if m.vars != nil {
			return m, m.varsKey(msg)
		}
		if m.flash != nil {
			return m, m.flashKey(msg)
		}
		var cmd tea.Cmd
		switch m.mode {
		case editMode:
			cmd = m.editKey(msg)
		case cmdMode:
			cmd = m.cmdKey(msg)
		default:
			cmd = m.normalKey(msg)
		}
		// re-analyse names once you're out of a cell (edits, pastes, deletes)
		if m.mode != editMode {
			cmd = tea.Batch(cmd, m.depsCmd())
		}
		return m, cmd
	}

	var cmd tea.Cmd
	if m.mode == cmdMode {
		m.cmd, cmd = m.cmd.Update(msg)
	}
	return m, cmd
}

// editorFor returns the cell's editor, picking up any change made to
// the cell from outside (external editor, paste of a new cell).
func (m *Model) editorFor(c *notebook.Cell) *editor {
	e, ok := m.eds[c]
	if !ok {
		e = newEditor(c.Source, m.vimOn, &m.reg, strings.EqualFold(m.lang, "python"))
		m.eds[c] = e
	}
	e.setText(c.Source)
	e.vim = m.vimOn
	return e
}

// startEdit opens the selected cell. how is normal, insert or append.
func (m *Model) startEdit(how string) tea.Cmd {
	m.mode = editMode
	m.ed = m.editorFor(m.cell())
	m.ed.keys = nil
	switch {
	case !m.ed.vim:
		m.ed.mode = vInsert
		if how == "append" {
			m.ed.cur = pos{m.ed.last(), len(m.ed.line(m.ed.last()))}
		}
	case how == "insert":
		m.ed.save()
		m.ed.startInsert([]string{"i"})
	case how == "append":
		m.ed.save()
		m.ed.cur = pos{m.ed.last(), len(m.ed.line(m.ed.last()))}
		m.ed.startInsert([]string{"A"})
	default:
		m.ed.setMode(vNormal)
	}
	m.ed.clamp()
	if m.cell().Type == notebook.Code {
		return m.startLSP()
	}
	return nil
}

func (m *Model) commitEdit() {
	if m.ed == nil {
		return
	}
	if c := m.cell(); c.Source != m.ed.text() {
		c.Source = m.ed.text()
		m.dirty = true
	}
}

func (m *Model) stopEdit() {
	m.commitEdit()
	if m.ed != nil {
		if m.ed.mode == vInsert && m.ed.vim {
			m.ed.finishInsert()
		}
		m.ed.keys = nil
		if m.ed.mode != vInsert {
			m.ed.setMode(vNormal)
		}
	}
	m.ed = nil
	m.comp = nil
	m.sig = nil
	m.mode = normalMode
	m.releaseDiags()
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
	m.execSrc[c] = c.Source
	var cmd tea.Cmd
	if m.k == nil {
		m.pending = append(m.pending, c)
	} else {
		cmd = m.submit(c)
	}
	return tea.Batch(cmd, m.startTick())
}

func (m *Model) submit(c *notebook.Cell) tea.Cmd {
	id, ch, err := m.k.ExecuteID(c.Source)
	m.msgIDs[c] = id
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
	var refresh tea.Cmd // an open variables panel reloads when runs finish
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
		c.AddOutput(e.ev.Output)
		m.dirty = true
		return tea.Batch(waitEvent(e.k, c, e.ch), m.syncKitty())
	case kernel.EvClear:
		c.Outputs = nil
	case kernel.EvInput:
		return tea.Batch(waitEvent(e.k, c, e.ch), m.startInput(e.k, c, e.ev.Input))
	case kernel.EvDone:
		delete(m.runs, c)
		if m.input != nil && m.input.cell == c {
			m.input = nil // interrupted while waiting
		}
		if e.ev.Err == nil {
			m.markRun(c, e.ev.ExecCount)
		}
		refresh = tea.Batch(m.refreshVarsAfterRun(e.ev), m.depsCmd(), m.reactiveRun())
		if e.ev.ExecCount > 0 {
			n := e.ev.ExecCount
			c.ExecutionCount = &n
		}
		if e.ev.Err != nil {
			m.msg = firstLine(e.ev.Err.Error())
		}
	}
	return tea.Batch(waitEvent(e.k, c, e.ch), refresh)
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
	// a fresh kernel has none of the old state, so nothing has "run"
	m.ran = map[*notebook.Cell]runInfo{}
	m.depsBusy = false
	m.msg = "restarting kernel"
	return m.host.start(old)
}

func (m *Model) save() {
	nb := m.nb
	if m.noSaveOutputs {
		nb = nb.Stripped()
	}
	if err := nb.Save(m.path); err != nil {
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
