package ui

import (
	"encoding/json"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// Stale cell tracking. The kernel parses every code cell with Python's
// ast (after IPython turns magics into calls) to get the names it
// defines and uses. A cell that ran is stale when a cell above it that
// defines one of its names re-ran after it, changed, or is stale itself;
// modified when its own source changed since it ran. Reactive mode
// reruns stale cells as soon as whatever they depend on finishes, like
// marimo.

const depsPy = `
import ast, builtins, json
SKIP = set(dir(builtins)) | {"get_ipython", "display", "In", "Out"}

class A(ast.NodeVisitor):
    def __init__(s):
        s.defs, s.loads, s.scopes = set(), set(), []
    def bind(s, name):
        (s.scopes[-1] if s.scopes else s.defs).add(name)
    def visit_Name(s, n):
        if isinstance(n.ctx, (ast.Store, ast.Del)):
            s.bind(n.id)
        elif not any(n.id in sc for sc in s.scopes):
            s.loads.add(n.id)
    def visit_Global(s, n):
        s.defs.update(n.names)
    def visit_Import(s, n):
        for a in n.names:
            s.bind((a.asname or a.name).split(".")[0])
    def visit_ImportFrom(s, n):
        for a in n.names:
            if a.name != "*":
                s.bind(a.asname or a.name)
    def func(s, n):
        if hasattr(n, "name"):
            s.bind(n.name)
        for d in getattr(n, "decorator_list", []):
            s.visit(d)
        args = n.args
        for d in args.defaults + [d for d in args.kw_defaults if d]:
            s.visit(d)
        local = {a.arg for a in args.posonlyargs + args.args + args.kwonlyargs}
        local |= {a.arg for a in (args.vararg, args.kwarg) if a}
        s.scopes.append(local)
        for st in (n.body if isinstance(n.body, list) else [n.body]):
            s.visit(st)
        s.scopes.pop()
    visit_FunctionDef = visit_AsyncFunctionDef = visit_Lambda = func
    def visit_ClassDef(s, n):
        s.bind(n.name)
        for e in n.bases + n.decorator_list + [k.value for k in n.keywords]:
            s.visit(e)
        s.scopes.append(set())
        for st in n.body:
            s.visit(st)
        s.scopes.pop()
    def comp(s, n):
        s.scopes.append(set())
        for g in n.generators:
            s.visit(g.iter)
            s.visit(g.target)
            for i in g.ifs:
                s.visit(i)
        for f in ("elt", "key", "value"):
            if hasattr(n, f):
                s.visit(getattr(n, f))
        s.scopes.pop()
    visit_ListComp = visit_SetComp = visit_GeneratorExp = visit_DictComp = comp

out = {}
for c in P["cells"]:
    src = c["src"]
    try:
        src = G["get_ipython"]().transform_cell(src)
    except Exception:
        pass
    try:
        tree = ast.parse(src)
    except SyntaxError:
        out[c["id"]] = None
        continue
    a = A()
    a.visit(tree)
    out[c["id"]] = {"defs": sorted(a.defs), "refs": sorted(a.loads - a.defs - SKIP)}
print(json.dumps(out))
`

type cellDeps struct {
	Defs []string `json:"defs"`
	Refs []string `json:"refs"`
}

type depsMsg struct {
	seq  int
	srcs map[string]string // id -> source that was analysed
	deps map[string]*cellDeps
	err  error
}

// cell run bookkeeping for stale tracking
type runInfo struct {
	seq int    // the kernel's execution count, 0 = never ran this session
	src string // source as it was executed
}

type cellStatus int

const (
	statusFresh cellStatus = iota
	statusModified
	statusStale       // something upstream ran again after it
	statusStaleEdited // something upstream was edited but hasn't run yet
)

// depsCmd analyses cells whose source changed since the last analysis.
func (m *Model) depsCmd() tea.Cmd {
	if m.k == nil || m.depsBusy {
		return nil
	}
	type req struct {
		ID  string `json:"id"`
		Src string `json:"src"`
	}
	var cells []req
	srcs := map[string]string{}
	for _, c := range m.nb.Cells {
		if c.Type != notebook.Code {
			continue
		}
		src := c.Source
		if a, ok := m.depsSrc[c]; ok && a == src {
			continue
		}
		id := m.cellKey(c)
		cells = append(cells, req{id, src})
		srcs[id] = src
	}
	if len(cells) == 0 {
		return nil
	}
	m.depsBusy = true
	m.depsSeq++
	seq, k := m.depsSeq, m.k
	return func() tea.Msg {
		out, err := k.Eval(pyCall(depsPy, map[string]any{"cells": cells}), 30*time.Second)
		msg := depsMsg{seq: seq, srcs: srcs, err: err}
		if err == nil {
			msg.err = json.Unmarshal([]byte(lastLine(out)), &msg.deps)
		}
		return msg
	}
}

// cellKey gives each cell a stable id for the round trip.
func (m *Model) cellKey(c *notebook.Cell) string {
	if c.ID != "" {
		return c.ID
	}
	if id, ok := m.anonIDs[c]; ok {
		return id
	}
	id := "c" + itoa(len(m.anonIDs)+1)
	m.anonIDs[c] = id
	return id
}

func (m *Model) handleDeps(msg depsMsg) tea.Cmd {
	m.depsBusy = false
	if msg.err != nil || msg.seq != m.depsSeq {
		return nil
	}
	for _, c := range m.nb.Cells {
		id := m.cellKey(c)
		src, ok := msg.srcs[id]
		if !ok {
			continue
		}
		m.depsSrc[c] = src
		if d := msg.deps[id]; d != nil {
			m.deps[c] = *d
		} else {
			// syntax error: keep whatever we knew
			if _, had := m.deps[c]; !had {
				m.deps[c] = cellDeps{}
			}
		}
	}
	// sources may have changed while we were asking
	return tea.Batch(m.depsCmd(), m.reactiveRun())
}

// cellStatuses works out modified/stale for every cell, in notebook
// order: a cell depends on the nearest cell above it defining each name
// it uses.
func (m *Model) cellStatuses() map[*notebook.Cell]cellStatus {
	st := map[*notebook.Cell]cellStatus{}
	lastDef := map[string]*notebook.Cell{}
	for _, c := range m.nb.Cells {
		if c.Type != notebook.Code {
			continue
		}
		ri := m.ran[c]
		if ri.seq > 0 {
			src := c.Source
			if m.mode == editMode && m.ed != nil && c == m.cell() {
				src = m.ed.text()
			}
			switch {
			case src != ri.src:
				st[c] = statusModified
			default:
				for _, r := range m.deps[c].Refs {
					d := lastDef[r]
					if d == nil || d == c {
						continue
					}
					switch {
					case st[d] == statusStale || m.ran[d].seq > ri.seq:
						st[c] = statusStale
					case st[d] != statusFresh && st[c] == statusFresh:
						st[c] = statusStaleEdited
					}
				}
			}
		}
		for _, name := range m.deps[c].Defs {
			lastDef[name] = c
		}
	}
	return st
}

func (m *Model) staleCount() (stale, modified int) {
	for _, s := range m.cellStatuses() {
		switch s {
		case statusStale, statusStaleEdited:
			stale++
		case statusModified:
			modified++
		}
	}
	return
}

// runStale reruns every stale or modified cell, top to bottom.
func (m *Model) runStale() tea.Cmd {
	st := m.cellStatuses()
	var cmds []tea.Cmd
	n := 0
	for _, c := range m.nb.Cells {
		if st[c] != statusFresh {
			cmds = append(cmds, m.execute(c))
			n++
		}
	}
	if n == 0 {
		m.msg = "nothing is stale"
	} else {
		m.msg = "rerunning " + itoa(n) + " cells"
	}
	return tea.Batch(cmds...)
}

// reactiveRun runs cells whose upstream actually ran again, once
// nothing else is running, so a run ripples down the notebook. Cells
// that are only behind an *edit* wait: rerunning them can't make them
// fresh until the edited cell runs, so that would loop forever.
func (m *Model) reactiveRun() tea.Cmd {
	if !m.reactive || len(m.runs) > 0 || m.k == nil {
		return nil
	}
	st := m.cellStatuses()
	var cmds []tea.Cmd
	for _, c := range m.nb.Cells {
		if st[c] == statusStale {
			cmds = append(cmds, m.execute(c))
		}
	}
	return tea.Batch(cmds...)
}

// markRun records a finished run for stale tracking. The order comes
// from the kernel's execution count, not from when we hear about it:
// each cell's events arrive on their own channel, so completions can be
// handled out of order, which made reactive mode loop forever.
func (m *Model) markRun(c *notebook.Cell, execCount int) {
	if execCount <= 0 {
		return
	}
	src, ok := m.execSrc[c]
	if !ok {
		src = c.Source
	}
	m.ran[c] = runInfo{seq: execCount, src: src}
}
