package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/envs"
	"github.com/nkapila6/jupytui/internal/lsp"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// basedpyright sees the notebook as one python file: every code cell's
// lines in order, magics blanked out. lspDoc maps between that file and
// (cell, row).

type lspDoc struct {
	text   string
	cells  []*notebook.Cell // code cells, in order
	starts []int            // doc line where each of those cells begins
}

var (
	magicRe = regexp.MustCompile(`^\s*[%!?]|^\s*\w+\s*=\s*[%!]`)
	// cell magics whose body is still python
	pyCellMagic = map[string]bool{"time": true, "timeit": true, "capture": true, "prun": true, "debug": true}
)

func (m *Model) buildDoc() lspDoc {
	var d lspDoc
	var b strings.Builder
	line := 0
	for i, c := range m.nb.Cells {
		if c.Type != notebook.Code {
			continue
		}
		src := c.Source
		if m.mode == editMode && m.ed != nil && i == m.sel {
			src = m.ed.text()
		}
		lines := strings.Split(src, "\n")
		blankAll := false
		if first := strings.TrimSpace(lines[0]); strings.HasPrefix(first, "%%") {
			name := strings.Fields(strings.TrimPrefix(first, "%%") + " ")[0]
			blankAll = !pyCellMagic[name]
		}
		d.cells = append(d.cells, c)
		d.starts = append(d.starts, line)
		for _, l := range lines {
			if blankAll || magicRe.MatchString(l) {
				l = ""
			}
			b.WriteString(l)
			b.WriteByte('\n')
			line++
		}
	}
	d.text = b.String()
	return d
}

func (d lspDoc) lineOf(c *notebook.Cell, row int) (int, bool) {
	for i, dc := range d.cells {
		if dc == c {
			return d.starts[i] + row, true
		}
	}
	return 0, false
}

func (d lspDoc) cellAt(line int) (*notebook.Cell, int, bool) {
	i := sort.Search(len(d.starts), func(i int) bool { return d.starts[i] > line }) - 1
	if i < 0 {
		return nil, 0, false
	}
	return d.cells[i], line - d.starts[i], true
}

// utf16Col / runeCol convert between our rune columns and LSP's
// utf-16 code units.
func utf16Col(line []rune, col int) int {
	n := 0
	for _, r := range line[:min(col, len(line))] {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func runeCol(line []rune, u int) int {
	n := 0
	for i, r := range line {
		if n >= u {
			return i
		}
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return len(line)
}

type lspReadyMsg struct {
	c   *lsp.Client
	err error
}

type lspNotifyMsg struct {
	c *lsp.Client
	n lsp.Notification
}

type lspDeadMsg struct{ c *lsp.Client }

type hoverMsg struct{ text string }

type defMsg struct {
	locs []lsp.Location
	err  error
}

type sigMsg struct {
	seq int
	sig *lsp.Signature
}

type cellDiag struct {
	row, col, endRow, endCol int
	sev                      int
	msg                      string
}

// startLSP launches basedpyright once, the first time a code cell is
// opened. Failure is reported once and completion falls back to the
// kernel alone.
func (m *Model) startLSP() tea.Cmd {
	if !m.lspOn || m.lspState != "" {
		return nil
	}
	m.lspState = "starting"
	dir := filepath.Dir(m.path)
	env := m.env
	ctx := m.lspCtx
	return func() tea.Msg {
		c, err := lsp.Start(ctx, nil, dir, lspSettings(pythonFor(env, dir)))
		if err == nil && ctx.Err() != nil {
			// quit while it was starting
			c.Close()
			return nil
		}
		return lspReadyMsg{c, err}
	}
}

// pythonFor is the interpreter the LSP resolves imports against, the
// same one the kernel runs.
func pythonFor(e envs.Env, dir string) string {
	// a remote kernel's packages aren't here; the local project env is
	// the best the local language server can do
	if e.Kind != envs.Project && e.Kind != envs.Remote {
		return e.Python
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "uv", "python", "find")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return "python3"
}

func lspSettings(python string) map[string]any {
	analysis := map[string]any{
		"typeCheckingMode":       "basic",
		"autoSearchPaths":        true,
		"useLibraryCodeForTypes": true,
		"diagnosticMode":         "openFilesOnly",
		// notebook habits that aren't bugs: a bare expression at the end
		// of a cell, redefining things across cells
		"diagnosticSeverityOverrides": map[string]any{
			"reportUnusedExpression": "none",
			"reportRedeclaration":    "none",
		},
	}
	return map[string]any{
		"python":       map[string]any{"pythonPath": python, "analysis": analysis},
		"basedpyright": map[string]any{"analysis": analysis},
	}
}

func (m *Model) docURI() string {
	stem := strings.TrimSuffix(filepath.Base(m.path), filepath.Ext(m.path))
	return lsp.FileURI(filepath.Join(filepath.Dir(m.path), "__jupytui_"+stem+".py"))
}

func (m *Model) handleLSPReady(msg lspReadyMsg) tea.Cmd {
	if msg.err != nil {
		m.lspState = "failed"
		m.msg = "lsp: " + firstLine(msg.err.Error()) + " (kernel completions still work)"
		return nil
	}
	m.lsp = msg.c
	m.lspState = "ready"
	m.doc = m.buildDoc()
	m.docVer = 1
	m.lsp.DidOpen(m.docURI(), "python", m.docVer, m.doc.text)
	return waitLSP(m.lsp)
}

func waitLSP(c *lsp.Client) tea.Cmd {
	return func() tea.Msg {
		select {
		case n := <-c.Notifications():
			return lspNotifyMsg{c, n}
		case <-c.Done():
			return lspDeadMsg{c}
		}
	}
}

// lspSync sends the document if it changed since last time.
func (m *Model) lspSync() {
	if m.lsp == nil {
		return
	}
	d := m.buildDoc()
	if d.text == m.doc.text {
		m.doc = d
		return
	}
	m.doc = d
	m.docVer++
	m.lsp.DidChange(m.docURI(), m.docVer, d.text)
}

func (m *Model) handleDiagnostics(n lsp.Notification) {
	if n.URI != m.docURI() {
		return
	}
	// like nvim's update_in_insert=false: half-typed code is always
	// "wrong", so hold new diagnostics until insert mode ends
	if m.mode == editMode && m.ed != nil && m.ed.mode == vInsert {
		m.heldDiags = &n
		return
	}
	m.heldDiags = nil
	m.diags = map[*notebook.Cell][]cellDiag{}
	for _, d := range n.Diags {
		// errors and warnings only; hints are mostly "unused" noise
		if d.Severity == 0 || d.Severity > 2 {
			continue
		}
		c, row, ok := m.doc.cellAt(d.Range.Start.Line)
		if !ok {
			continue
		}
		endRow := row + d.Range.End.Line - d.Range.Start.Line
		cd := cellDiag{row: row, endRow: endRow, sev: d.Severity, msg: d.Message}
		lines := strings.Split(c.Source, "\n")
		if m.mode == editMode && m.ed != nil && m.cell() == c {
			lines = strings.Split(m.ed.text(), "\n")
		}
		if row < len(lines) {
			cd.col = runeCol([]rune(lines[row]), d.Range.Start.Character)
		}
		if endRow < len(lines) {
			cd.endCol = runeCol([]rune(lines[endRow]), d.Range.End.Character)
		}
		m.diags[c] = append(m.diags[c], cd)
	}
}

// releaseDiags applies diagnostics held back during insert mode.
func (m *Model) releaseDiags() {
	if n := m.heldDiags; n != nil && (m.ed == nil || m.ed.mode != vInsert) {
		m.handleDiagnostics(*n)
	}
}

// diagAt returns the most severe diagnostic on a cell row.
func (m *Model) diagAt(c *notebook.Cell, row int) (cellDiag, bool) {
	var best cellDiag
	found := false
	for _, d := range m.diags[c] {
		if row >= d.row && row <= d.endRow && (!found || d.sev < best.sev) {
			best, found = d, true
		}
	}
	return best, found
}

func (m *Model) diagCounts() (errs, warns int) {
	for _, ds := range m.diags {
		for _, d := range ds {
			if d.sev == 1 {
				errs++
			} else {
				warns++
			}
		}
	}
	return
}

// jumpDiag implements ]d / [d across the whole notebook.
func (m *Model) jumpDiag(dir int) {
	if !m.diagOn {
		return
	}
	starts := m.lineStarts()
	cur := starts[m.sel] - 1
	curCol := -1
	if m.mode == editMode && m.ed != nil {
		cur = starts[m.sel] + m.ed.cur.row
		curCol = m.ed.cur.col
	} else if dir < 0 {
		cur = starts[m.sel]
	}
	type hit struct {
		g, col int
		d      cellDiag
	}
	var hits []hit
	for i, c := range m.nb.Cells {
		for _, d := range m.diags[c] {
			hits = append(hits, hit{starts[i] + d.row, d.col, d})
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].g < hits[j].g || hits[i].g == hits[j].g && hits[i].col < hits[j].col
	})
	var target *hit
	for i := range hits {
		h := &hits[i]
		after := h.g > cur || h.g == cur && h.col > curCol
		before := h.g < cur || h.g == cur && h.col < curCol
		if dir > 0 && after {
			target = h
			break
		}
		if dir < 0 && before {
			target = h
		}
	}
	if target == nil {
		m.msg = "no more diagnostics"
		return
	}
	m.gotoLine(target.g, target.col)
	m.msg = diagText(target.d)
}

func diagText(d cellDiag) string {
	icon := "✗ "
	if d.sev == 2 {
		icon = "! "
	}
	return icon + firstLine(d.msg)
}

// lspPos is the cursor's position in the virtual document.
func (m *Model) lspPos() (lsp.Position, bool) {
	if m.lsp == nil || m.ed == nil {
		return lsp.Position{}, false
	}
	m.lspSync()
	line, ok := m.doc.lineOf(m.cell(), m.ed.cur.row)
	if !ok {
		return lsp.Position{}, false
	}
	return lsp.Position{Line: line, Character: utf16Col(m.ed.line(m.ed.cur.row), m.ed.cur.col)}, true
}

var lspKinds = map[int]string{
	2: "method", 3: "function", 4: "function", 5: "field", 6: "variable", 7: "class", 8: "class",
	9: "module", 10: "property", 12: "variable", 13: "class", 14: "keyword", 21: "constant", 22: "class",
}

func (m *Model) lspCompletion(seq, row int) tea.Cmd {
	p, ok := m.lspPos()
	if !ok {
		return nil
	}
	c, uri := m.lsp, m.docURI()
	start := m.ed.cur.col - identBefore(m.ed)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		items, err := c.Completion(ctx, uri, p)
		msg := compMsg{seq: seq, source: "lsp", row: row, start: start}
		if err != nil {
			return msg
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].SortText < items[j].SortText })
		for _, it := range items[:min(len(items), 200)] {
			text := it.Label
			switch {
			case it.TextEdit != nil:
				text = it.TextEdit.NewText
			case it.InsertText != "":
				text = it.InsertText
			}
			msg.items = append(msg.items, compItem{label: text, kind: lspKinds[it.Kind], source: "lsp"})
		}
		return msg
	}
}

func (m *Model) hover() tea.Cmd {
	p, ok := m.lspPos()
	if !ok {
		m.msg = "hover needs the language server"
		return nil
	}
	c, uri := m.lsp, m.docURI()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		text, _ := c.Hover(ctx, uri, p)
		return hoverMsg{text}
	}
}

func (m *Model) definition() tea.Cmd {
	p, ok := m.lspPos()
	if !ok {
		m.msg = "go to definition needs the language server"
		return nil
	}
	c, uri := m.lsp, m.docURI()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		locs, err := c.Definition(ctx, uri, p)
		return defMsg{locs, err}
	}
}

func (m *Model) handleDefinition(msg defMsg) tea.Cmd {
	if msg.err != nil || len(msg.locs) == 0 {
		m.msg = "no definition found"
		return nil
	}
	loc := msg.locs[0]
	if loc.URI == m.docURI() {
		c, row, ok := m.doc.cellAt(loc.Range.Start.Line)
		if !ok {
			return nil
		}
		for i, cc := range m.nb.Cells {
			if cc == c {
				lines := strings.Split(c.Source, "\n")
				col := 0
				if row < len(lines) {
					col = runeCol([]rune(lines[row]), loc.Range.Start.Character)
				}
				m.gotoLine(m.lineStarts()[i]+row, col)
				return nil
			}
		}
		return nil
	}
	path := lsp.URIPath(loc.URI)
	line := loc.Range.Start.Line + 1
	// library code: open it in the host nvim if we're inside one
	if sock := os.Getenv("NVIM"); sock != "" {
		lua := fmt.Sprintf(`(function(f) vim.cmd("tabedit +%d " .. vim.fn.fnameescape(f)) return "" end)(_A)`, line)
		expr := fmt.Sprintf("luaeval(%s, %s)", vimString(lua), vimString(path))
		if exec.Command("nvim", "--server", sock, "--remote-expr", expr).Run() == nil {
			m.msg = "opened " + filepath.Base(path) + " in nvim"
			return nil
		}
	}
	m.msg = fmt.Sprintf("definition: %s:%d", path, line)
	return nil
}

func (m *Model) signatureHelp() tea.Cmd {
	p, ok := m.lspPos()
	if !ok {
		return nil
	}
	m.sigSeq++
	seq := m.sigSeq
	c, uri := m.lsp, m.docURI()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		sig, _ := c.SignatureHelp(ctx, uri, p)
		return sigMsg{seq, sig}
	}
}

func (m *Model) setLSPPython() {
	if m.lsp != nil {
		m.lsp.SetSettings(lspSettings(pythonFor(m.env, filepath.Dir(m.path))))
	}
}
