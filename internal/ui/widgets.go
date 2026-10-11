package ui

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// ipywidgets: the kernel opens a comm per widget model and keeps its
// state in sync over it. We hold the state, draw the common controls as
// text and send changes back, which is all the kernel side needs to run
// callbacks and interact().

const widgetMime = "application/vnd.jupyter.widget-view+json"

type widget struct {
	id    string
	model string // _model_name minus "Model": IntSlider, VBox, Output...
	state map[string]any

	// Output widgets: what got captured into them
	out       *notebook.Cell
	clearNext bool // clear_output(wait=True) is waiting for the next output
}

type widgetFocus struct {
	cell *notebook.Cell
	id   string
	edit *textinput.Model // typing into a text widget
}

type commMsg struct {
	k *kernel.Kernel
	c *kernel.Comm
}

// commEventMsg is one event from a comm_msg we sent: what the handler
// printed, or the widget updates it caused.
type commEventMsg struct {
	k    *kernel.Kernel
	cell *notebook.Cell // where the widget lives, stray prints go there
	id   string
	ch   <-chan kernel.Event
	ev   kernel.Event
	ok   bool
}

func waitComms(k *kernel.Kernel) tea.Cmd {
	return func() tea.Msg {
		select {
		case c := <-k.Comms():
			return commMsg{k, c}
		case <-k.Dead():
			return nil
		}
	}
}

func waitCommEvent(k *kernel.Kernel, c *notebook.Cell, id string, ch <-chan kernel.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		return commEventMsg{k: k, cell: c, id: id, ch: ch, ev: ev, ok: ok}
	}
}

func (m *Model) handleComm(c *kernel.Comm) {
	switch c.Type {
	case "comm_open":
		if c.Target != "jupyter.widget" {
			return
		}
		var d struct {
			State map[string]any `json:"state"`
		}
		json.Unmarshal(c.Data, &d)
		if d.State == nil {
			d.State = map[string]any{}
		}
		name, _ := d.State["_model_name"].(string)
		w := &widget{id: c.ID, model: strings.TrimSuffix(name, "Model"), state: d.State, out: &notebook.Cell{}}
		w.syncOutputs(d.State)
		m.widgets[c.ID] = w
	case "comm_msg":
		w := m.widgets[c.ID]
		if w == nil {
			return
		}
		var d struct {
			Method string         `json:"method"`
			State  map[string]any `json:"state"`
		}
		json.Unmarshal(c.Data, &d)
		if d.Method != "update" && d.Method != "echo_update" {
			return
		}
		for k, v := range d.State {
			w.state[k] = v
		}
		w.syncOutputs(d.State)
	case "comm_close":
		delete(m.widgets, c.ID)
		if m.wfocus != nil && m.wfocus.id == c.ID {
			m.wfocus = nil
		}
	}
}

// syncOutputs picks up an Output widget's outputs when the kernel sets
// them directly (append_stdout, clear_output on the widget).
func (w *widget) syncOutputs(state map[string]any) {
	raw, ok := state["outputs"].([]any)
	if !ok {
		return
	}
	w.out.Outputs = nil
	for _, r := range raw {
		b, _ := json.Marshal(r)
		if o, err := notebook.ParseOutput(b); err == nil {
			w.out.AddOutput(o)
		}
	}
}

// captureFor is the Output widget currently capturing a request's
// outputs (a `with out:` block), if any.
func (m *Model) captureFor(msgID string) *widget {
	if msgID == "" {
		return nil
	}
	for _, w := range m.widgets {
		if w.model == "Output" && w.str("msg_id") == msgID {
			return w
		}
	}
	return nil
}

// addOutput puts an output where it belongs: an Output widget that's
// capturing the request, or the cell.
func (m *Model) addOutput(c *notebook.Cell, msgID string, o *notebook.Output) {
	if w := m.captureFor(msgID); w != nil {
		if w.clearNext {
			w.out.Outputs = nil
			w.clearNext = false
		}
		w.out.AddOutput(o)
		return
	}
	if c != nil {
		c.AddOutput(o)
		m.dirty = true
	}
}

func (m *Model) clearOutput(c *notebook.Cell, msgID string, wait bool) {
	if w := m.captureFor(msgID); w != nil {
		if wait {
			w.clearNext = true
		} else {
			w.out.Outputs = nil
		}
		return
	}
	if c != nil {
		c.Outputs = nil
	}
}

func (m *Model) handleCommEvent(e commEventMsg) tea.Cmd {
	if !e.ok || e.k != m.k {
		return nil
	}
	switch e.ev.Kind {
	case kernel.EvOutput:
		m.addOutput(e.cell, e.id, e.ev.Output)
	case kernel.EvClear:
		m.clearOutput(e.cell, e.id, e.ev.Wait)
	case kernel.EvComm:
		m.handleComm(e.ev.Comm)
	case kernel.EvDone:
		return m.refreshVarsAfterRun(e.ev)
	}
	return tea.Batch(waitCommEvent(e.k, e.cell, e.id, e.ch), m.syncKitty())
}

// sendWidget sends a state change (or a custom event) to the kernel.
func (m *Model) sendWidget(w *widget, data map[string]any) tea.Cmd {
	if m.k == nil {
		return nil
	}
	cell := m.cell()
	if m.wfocus != nil {
		cell = m.wfocus.cell
	}
	id, ch, err := m.k.CommSend(w.id, data)
	if err != nil {
		m.msg = "widget: " + err.Error()
		return nil
	}
	return waitCommEvent(m.k, cell, id, ch)
}

func (m *Model) setWidget(w *widget, key string, v any) tea.Cmd {
	w.state[key] = v
	return m.sendWidget(w, map[string]any{"method": "update", "state": map[string]any{key: v}, "buffer_paths": []any{}})
}

func (w *widget) str(k string) string {
	switch v := w.state[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (w *widget) num(k string) float64 {
	f, _ := w.state[k].(float64)
	return f
}

func (w *widget) flag(k string) bool {
	b, _ := w.state[k].(bool)
	return b
}

func (w *widget) labels() []string {
	raw, _ := w.state["_options_labels"].([]any)
	out := make([]string, len(raw))
	for i, r := range raw {
		out[i] = fmt.Sprint(r)
	}
	return out
}

// index of the selected option, -1 for none
func (w *widget) index() int {
	if f, ok := w.state["index"].(float64); ok {
		return int(f)
	}
	return -1
}

// children are "IPY_MODEL_<id>" references
func (w *widget) children() []string {
	raw, _ := w.state["children"].([]any)
	var out []string
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, strings.TrimPrefix(s, "IPY_MODEL_"))
		}
	}
	return out
}

func isContainer(model string) bool {
	switch model {
	case "Box", "HBox", "VBox", "GridBox", "Accordion", "Tab", "Stack":
		return true
	}
	return false
}

// interactive widgets take focus with tab
func (w *widget) interactive() bool {
	if w.flag("disabled") {
		return false
	}
	switch w.model {
	case "IntSlider", "FloatSlider", "FloatLogSlider", "SelectionSlider",
		"Dropdown", "Select", "ToggleButtons", "RadioButtons",
		"Checkbox", "ToggleButton", "Button",
		"Text", "Textarea", "Password", "Combobox",
		"IntText", "FloatText", "BoundedIntText", "BoundedFloatText":
		return true
	}
	return false
}

func viewID(o *notebook.Output) (string, bool) {
	raw, ok := o.Data[widgetMime]
	if !ok {
		return "", false
	}
	var v struct {
		ModelID string `json:"model_id"`
	}
	json.Unmarshal(raw, &v)
	return v.ModelID, v.ModelID != ""
}

// widgetLeaves lists a cell's focusable widgets in drawing order.
func (m *Model) widgetLeaves(c *notebook.Cell) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(id string)
	walk = func(id string) {
		w := m.widgets[id]
		if w == nil || seen[id] {
			return
		}
		seen[id] = true
		switch {
		case isContainer(w.model):
			for _, ch := range w.children() {
				walk(ch)
			}
		case w.model == "Output":
			for _, o := range w.out.Outputs {
				if id, ok := viewID(o); ok {
					walk(id)
				}
			}
		case w.interactive():
			out = append(out, id)
		}
	}
	for _, o := range c.Outputs {
		if id, ok := viewID(o); ok {
			walk(id)
		}
	}
	return out
}

// focusWidgets moves keys into the selected cell's widgets (tab).
func (m *Model) focusWidgets() {
	leaves := m.widgetLeaves(m.cell())
	if len(leaves) == 0 {
		m.msg = "no live widgets in this cell"
		return
	}
	m.wfocus = &widgetFocus{cell: m.cell(), id: leaves[0]}
}

func (m *Model) widgetKey(msg tea.KeyPressMsg) tea.Cmd {
	f := m.wfocus
	w := m.widgets[f.id]
	if w == nil {
		m.wfocus = nil
		return nil
	}
	if f.edit != nil {
		switch msg.String() {
		case "esc":
			f.edit = nil
			return nil
		case "enter":
			return m.commitWidgetText(w)
		}
		var cmd tea.Cmd
		*f.edit, cmd = f.edit.Update(msg)
		return cmd
	}
	key := msg.String()
	step := 1
	if key == "H" || key == "L" {
		step = 10
	}
	switch key {
	case "esc", "q":
		m.wfocus = nil
	case "tab", "j", "down":
		m.moveWidgetFocus(1)
	case "shift+tab", "k", "up":
		m.moveWidgetFocus(-1)
	case "h", "left", "H":
		return m.nudgeWidget(w, -step)
	case "l", "right", "L":
		return m.nudgeWidget(w, step)
	case "enter", "space":
		return m.activateWidget(w)
	case "ctrl+c":
		return m.interrupt()
	}
	return nil
}

func (m *Model) moveWidgetFocus(d int) {
	leaves := m.widgetLeaves(m.wfocus.cell)
	if len(leaves) == 0 {
		m.wfocus = nil
		return
	}
	at := 0
	for i, id := range leaves {
		if id == m.wfocus.id {
			at = i
		}
	}
	m.wfocus.id = leaves[(at+d+len(leaves))%len(leaves)]
}

// nudgeWidget is h/l: move a slider or pick the next option.
func (m *Model) nudgeWidget(w *widget, d int) tea.Cmd {
	switch w.model {
	case "IntSlider", "FloatSlider", "IntText", "FloatText", "BoundedIntText", "BoundedFloatText":
		step := w.num("step")
		if step == 0 {
			step = 1
		}
		v := w.num("value") + float64(d)*step
		if _, bounded := w.state["max"]; bounded {
			v = math.Max(w.num("min"), math.Min(w.num("max"), v))
		}
		if strings.HasPrefix(w.model, "Float") || strings.HasPrefix(w.model, "BoundedFloat") {
			// keep 0.1 + 0.2 from turning into 0.30000000000000004
			v = math.Round(v/step) * step
			v, _ = strconv.ParseFloat(strconv.FormatFloat(v, 'f', decimals(step), 64), 64)
		}
		return m.setWidget(w, "value", v)
	case "FloatLogSlider":
		base, step := w.num("base"), w.num("step")
		if base == 0 {
			base = 10
		}
		if step == 0 {
			step = 0.1
		}
		exp := math.Log(w.num("value"))/math.Log(base) + float64(d)*step
		exp = math.Max(w.num("min"), math.Min(w.num("max"), exp))
		return m.setWidget(w, "value", math.Pow(base, exp))
	case "SelectionSlider", "Dropdown", "Select", "ToggleButtons", "RadioButtons":
		n := len(w.labels())
		if n == 0 {
			return nil
		}
		i := max(0, min(n-1, w.index()+d))
		if i == w.index() {
			return nil
		}
		return m.setWidget(w, "index", float64(i))
	case "Checkbox", "ToggleButton":
		return m.setWidget(w, "value", d > 0)
	}
	return nil
}

func (m *Model) activateWidget(w *widget) tea.Cmd {
	switch w.model {
	case "Checkbox", "ToggleButton":
		return m.setWidget(w, "value", !w.flag("value"))
	case "Button":
		return m.sendWidget(w, map[string]any{"method": "custom", "content": map[string]any{"event": "click"}})
	case "Dropdown", "Select", "ToggleButtons", "RadioButtons", "SelectionSlider":
		// enter cycles, handy when there's no room to show every option
		n := len(w.labels())
		if n == 0 {
			return nil
		}
		return m.setWidget(w, "index", float64((w.index()+1)%n))
	case "Text", "Textarea", "Password", "Combobox", "IntText", "FloatText", "BoundedIntText", "BoundedFloatText":
		box := textinput.New()
		box.Prompt = ""
		box.SetVirtualCursor(false)
		if w.model == "Password" {
			box.EchoMode = textinput.EchoPassword
		}
		box.SetValue(w.str("value"))
		box.CursorEnd()
		m.wfocus.edit = &box
		return box.Focus()
	}
	return nil
}

func (m *Model) commitWidgetText(w *widget) tea.Cmd {
	s := m.wfocus.edit.Value()
	m.wfocus.edit = nil
	var v any = s
	switch w.model {
	case "IntText", "BoundedIntText":
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			m.msg = "not an integer: " + s
			return nil
		}
		v = float64(n)
	case "FloatText", "BoundedFloatText":
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			m.msg = "not a number: " + s
			return nil
		}
		v = f
	}
	cmd := m.setWidget(w, "value", v)
	if w.model == "Text" || w.model == "Combobox" || w.model == "Password" {
		// what on_submit listens for
		cmd = tea.Batch(cmd, m.sendWidget(w, map[string]any{"method": "custom", "content": map[string]any{"event": "submit"}}))
	}
	return cmd
}

func decimals(step float64) int {
	s := strconv.FormatFloat(step, 'f', -1, 64)
	if _, frac, ok := strings.Cut(s, "."); ok {
		return len(frac)
	}
	return 0
}

// renderWidget draws a widget tree as lines, with any images in it.
func (m *Model) renderWidget(id string, width int, depth int) ([]string, []outImg) {
	w := m.widgets[id]
	if w == nil || depth > 20 {
		return nil, nil
	}
	switch {
	case isContainer(w.model):
		var parts [][]string
		var imgs []outImg
		line := 0
		oneLine := true
		total := 0
		for _, ch := range w.children() {
			l, im := m.renderWidget(ch, width, depth+1)
			if len(l) == 0 {
				continue
			}
			for _, p := range im {
				p.line += line
				imgs = append(imgs, p)
			}
			line += len(l)
			parts = append(parts, l)
			if len(l) != 1 {
				oneLine = false
			}
			total += lipgloss.Width(l[0]) + 3
		}
		// a row of controls side by side when it fits, stacked otherwise
		if w.model == "HBox" && oneLine && len(parts) > 1 && total <= width {
			row := make([]string, len(parts))
			for i, p := range parts {
				row[i] = p[0]
			}
			return []string{strings.Join(row, "   ")}, nil
		}
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		return out, imgs
	case w.model == "Output":
		return m.renderOutputs(w.out, width)
	}
	return []string{m.widgetLine(w, width)}, nil
}

// widgetLine draws one control.
func (m *Model) widgetLine(w *widget, width int) string {
	focused := m.wfocus != nil && m.wfocus.id == w.id
	hl := func(s string) string {
		if focused {
			return lipgloss.NewStyle().Reverse(true).Render(s)
		}
		return s
	}
	desc := w.str("description")
	label := ""
	if desc != "" {
		label = m.st.dim.Render(desc) + " "
	}
	rest := max(width-lipgloss.Width(label), 10)

	switch w.model {
	case "IntSlider", "FloatSlider", "FloatLogSlider", "SelectionSlider":
		lo, hi, v := w.num("min"), w.num("max"), w.num("value")
		readout := w.readout()
		if w.model == "FloatLogSlider" && v > 0 {
			base := w.num("base")
			if base == 0 {
				base = 10
			}
			v = math.Log(v) / math.Log(base)
		}
		if w.model == "SelectionSlider" {
			lo, hi, v = 0, float64(len(w.labels())-1), float64(w.index())
		}
		barW := max(min(rest-lipgloss.Width(readout)-2, 30), 6)
		pos := 0
		if hi > lo {
			pos = int(math.Round((v - lo) / (hi - lo) * float64(barW-1)))
		}
		pos = max(0, min(barW-1, pos))
		bar := m.st.accent.Render(strings.Repeat("━", pos)) + hl("●") + m.st.faint.Render(strings.Repeat("─", barW-1-pos))
		return label + bar + " " + readout
	case "Dropdown", "Select", "Combobox":
		cur := w.str("value")
		if labels := w.labels(); w.index() >= 0 && w.index() < len(labels) {
			cur = labels[w.index()]
		}
		if f := m.wfocus; focused && f.edit != nil {
			return label + "[" + f.edit.View() + "]"
		}
		return label + hl("[ "+cur+" ▾ ]")
	case "ToggleButtons", "RadioButtons":
		var opts []string
		for i, l := range w.labels() {
			if i == w.index() {
				opts = append(opts, hl(m.st.accent.Bold(true).Render("("+l+")")))
			} else {
				opts = append(opts, " "+l+" ")
			}
		}
		return label + strings.Join(opts, " ")
	case "Checkbox":
		box := "[ ]"
		if w.flag("value") {
			box = "[x]"
		}
		return hl(box) + " " + desc
	case "ToggleButton":
		s := "[ " + desc + " ]"
		if w.flag("value") {
			s = m.st.accent.Bold(true).Render(s)
		}
		return hl(s)
	case "Button":
		return hl(m.st.accent.Bold(true).Render("[ " + desc + " ]"))
	case "Text", "Textarea", "Password", "IntText", "FloatText", "BoundedIntText", "BoundedFloatText":
		if f := m.wfocus; focused && f.edit != nil {
			return label + "[" + f.edit.View() + "]"
		}
		v := w.str("value")
		if w.model == "Password" {
			v = strings.Repeat("·", len([]rune(v)))
		}
		if v == "" {
			// ipywidgets' default placeholder is a zero width space
			v = m.st.dim.Render(strings.Trim(w.str("placeholder"), "\u200b"))
		}
		v = strings.ReplaceAll(v, "\n", "⏎")
		return label + hl("["+padRight(" "+v, min(rest-2, 24))+"]")
	case "Label":
		return label + w.str("value")
	case "HTML", "HTMLMath":
		return label + strings.TrimSpace(htmlText(w.str("value")))
	case "IntProgress", "FloatProgress":
		lo, hi, v := w.num("min"), w.num("max"), w.num("value")
		barW := max(min(rest-6, 30), 6)
		fill := 0
		if hi > lo {
			fill = int(math.Round((v - lo) / (hi - lo) * float64(barW)))
		}
		fill = max(0, min(barW, fill))
		pct := 0.0
		if hi > lo {
			pct = (v - lo) / (hi - lo) * 100
		}
		return label + m.st.accent.Render(strings.Repeat("█", fill)) + m.st.faint.Render(strings.Repeat("░", barW-fill)) + fmt.Sprintf(" %3.0f%%", pct)
	case "Valid":
		if w.flag("value") {
			return label + m.st.ok.Render("✓")
		}
		return label + m.st.errText.Render("✗ "+w.str("readout"))
	}
	if v, ok := w.state["value"]; ok {
		return label + fmt.Sprint(v)
	}
	return m.st.dim.Render("[" + w.model + " widget]")
}

func (w *widget) readout() string {
	if !w.flag("readout") {
		if _, set := w.state["readout"]; set {
			return ""
		}
	}
	switch w.model {
	case "IntSlider":
		return strconv.Itoa(int(w.num("value")))
	case "SelectionSlider":
		if l := w.labels(); w.index() >= 0 && w.index() < len(l) {
			return l[w.index()]
		}
		return ""
	}
	f := w.str("readout_format")
	if f == "" {
		f = ".2f"
	}
	if strings.HasSuffix(f, "f") || strings.HasSuffix(f, "e") || strings.HasSuffix(f, "g") {
		if p, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(f, "."), f[len(f)-1:])); err == nil {
			return strconv.FormatFloat(w.num("value"), f[len(f)-1], p, 64)
		}
	}
	return strconv.FormatFloat(w.num("value"), 'g', -1, 64)
}
