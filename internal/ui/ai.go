package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/ai"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// The AI pane (ga): a chat on the right that sees the notebook. This is
// the first step, no tools yet: it reads, it doesn't edit or run.

const aiSystem = `You are the assistant inside jupytui, a terminal Jupyter notebook editor.

The user's notebook arrives in <notebook> blocks: every cell with its id, type, source and outputs, the selected cell marked. A newer block replaces older ones. Images of the selected cell's plots may follow it.

Be direct and brief, this is a narrow terminal pane. Put code in fenced python blocks. You can't edit or run cells yet, so give code the user can paste into a cell.`

const (
	aiMaxOutput = 2000 // characters of output kept per cell
	aiMaxImages = 3
	aiImageSide = 1568 // longest side sent; bigger costs tokens for nothing
)

type aiEntry struct {
	Kind string `json:"kind"` // user, assistant, thinking, note, error
	Text string `json:"text"`
}

type aiPane struct {
	sess    *ai.Session
	cfgErr  string
	open    bool
	typing  bool
	input   textinput.Model
	hist    []ai.Message
	entries []aiEntry
	snap    string // hash of the last notebook snapshot sent

	streaming bool
	cancel    context.CancelFunc
	seq       int
	scroll    int // lines up from the bottom
	last      ai.Usage
	total     ai.Usage
	effort    string
}

// saved with a detached session
type aiSaved struct {
	Model   string       `json:"model"`
	Hist    []ai.Message `json:"hist"`
	Entries []aiEntry    `json:"entries"`
	Snap    string       `json:"snap"`
	Total   ai.Usage     `json:"total"`
}

type aiStartMsg struct {
	seq int
	ch  <-chan ai.Chunk
	err error
}

type aiChunkMsg struct {
	seq int
	ch  <-chan ai.Chunk
	c   ai.Chunk
	ok  bool
}

func newAIPane() *aiPane {
	p := &aiPane{effort: "medium"}
	p.input = textinput.New()
	p.input.Prompt = "› "
	p.input.Placeholder = "ask about this notebook"
	p.input.SetVirtualCursor(false)
	prov, err := ai.FromEnv()
	if err != nil {
		p.cfgErr = err.Error()
	} else {
		p.sess = &ai.Session{P: prov}
	}
	return p
}

// openAI is ga: open the pane and start typing.
func (m *Model) openAI() tea.Cmd {
	if m.ai == nil {
		m.ai = newAIPane()
	}
	if m.mode == editMode {
		m.stopEdit()
	}
	m.ai.open = true
	m.ai.typing = true
	return m.ai.input.Focus()
}

func (m *Model) closeAI() {
	if m.ai == nil {
		return
	}
	m.ai.open = false
	m.ai.typing = false
	m.ai.input.Blur()
}

func (m *Model) aiKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.ai
	switch msg.String() {
	case "esc":
		// back to the notebook; the pane stays unless it's covering it
		if _, nbW := m.aiWidths(); nbW == 0 {
			m.closeAI()
			return nil
		}
		p.typing = false
		p.input.Blur()
		return nil
	case "ctrl+c":
		if p.streaming {
			m.stopAI("stopped")
			return nil
		}
		p.typing = false
		p.input.Blur()
		return nil
	case "pgup", "ctrl+up":
		p.scroll += max(m.bodyHeight()/2, 1)
		return nil
	case "pgdown", "ctrl+down":
		p.scroll = max(p.scroll-max(m.bodyHeight()/2, 1), 0)
		return nil
	case "enter":
		text := strings.TrimSpace(p.input.Value())
		if text == "" || p.streaming {
			return nil
		}
		p.input.SetValue("")
		return m.askAI(text)
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	return cmd
}

func (m *Model) askAI(text string) tea.Cmd {
	p := m.ai
	if text == "/clear" {
		p.hist, p.entries, p.snap, p.scroll = nil, nil, "", 0
		return nil
	}
	p.entries = append(p.entries, aiEntry{Kind: "user", Text: text})
	p.scroll = 0
	if p.sess == nil {
		p.entries = append(p.entries, aiEntry{Kind: "error", Text: p.cfgErr})
		return nil
	}
	var parts []ai.Part
	snap, imgs := m.aiSnapshot()
	if h := hash(snap); h != p.snap {
		parts = append(parts, ai.Part{Text: snap})
		parts = append(parts, imgs...)
		p.snap = h
	}
	parts = append(parts, ai.Part{Text: text})
	p.hist = append(p.hist, ai.Message{Role: "user", Parts: parts})

	req := ai.Request{System: aiSystem, Messages: append([]ai.Message{}, p.hist...), Effort: p.effort}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.streaming = true
	p.seq++
	seq, sess := p.seq, p.sess
	return tea.Batch(m.startTick(), func() tea.Msg {
		ch, err := sess.Send(ctx, req)
		return aiStartMsg{seq: seq, ch: ch, err: err}
	})
}

func waitAI(seq int, ch <-chan ai.Chunk) tea.Cmd {
	return func() tea.Msg {
		c, ok := <-ch
		return aiChunkMsg{seq: seq, ch: ch, c: c, ok: ok}
	}
}

func (m *Model) handleAIStart(msg aiStartMsg) tea.Cmd {
	p := m.ai
	if p == nil || msg.seq != p.seq {
		return nil
	}
	if msg.err != nil {
		m.failAI(msg.err)
		return nil
	}
	return waitAI(msg.seq, msg.ch)
}

func (m *Model) handleAIChunk(msg aiChunkMsg) tea.Cmd {
	p := m.ai
	if p == nil || msg.seq != p.seq || !msg.ok {
		return nil
	}
	c := msg.c
	switch {
	case c.Note != "":
		p.entries = append(p.entries, aiEntry{Kind: "note", Text: c.Note})
	case c.Thinking != "":
		p.appendEntry("thinking", c.Thinking)
	case c.Text != "":
		p.appendEntry("assistant", c.Text)
	}
	if !c.Done {
		return waitAI(msg.seq, msg.ch)
	}
	if c.Err != nil {
		m.failAI(c.Err)
		return nil
	}
	p.streaming = false
	p.cancel = nil
	p.hist = append(p.hist, *c.Message)
	p.last = c.Usage
	p.total.In += c.Usage.In
	p.total.Out += c.Usage.Out
	p.total.CacheRead += c.Usage.CacheRead
	p.total.CacheWrite += c.Usage.CacheWrite
	switch c.Stop {
	case "refusal":
		p.entries = append(p.entries, aiEntry{Kind: "note", Text: "the model declined this one"})
	case "max_tokens":
		p.entries = append(p.entries, aiEntry{Kind: "note", Text: "reply hit the length limit"})
	}
	return nil
}

// appendEntry grows the last entry while the same kind keeps streaming.
func (p *aiPane) appendEntry(kind, text string) {
	if n := len(p.entries); n > 0 && p.entries[n-1].Kind == kind {
		p.entries[n-1].Text += text
		return
	}
	p.entries = append(p.entries, aiEntry{Kind: kind, Text: text})
}

func (m *Model) stopAI(why string) {
	p := m.ai
	if p.cancel != nil {
		p.cancel()
	}
	p.seq++ // drop whatever is still in flight
	m.endFailedTurn()
	p.entries = append(p.entries, aiEntry{Kind: "note", Text: why})
}

func (m *Model) failAI(err error) {
	m.endFailedTurn()
	m.ai.entries = append(m.ai.entries, aiEntry{Kind: "error", Text: err.Error()})
}

// endFailedTurn takes back the question that got no answer, so history
// keeps alternating user/assistant. The snapshot goes with it, so the
// next question sends it again.
func (m *Model) endFailedTurn() {
	p := m.ai
	p.streaming = false
	p.cancel = nil
	if n := len(p.hist); n > 0 && p.hist[n-1].Role == "user" {
		p.hist = p.hist[:n-1]
		p.snap = ""
	}
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:8])
}

// aiSnapshot is the notebook as the model sees it, plus the selected
// cell's plots.
func (m *Model) aiSnapshot() (string, []ai.Part) {
	var b strings.Builder
	fmt.Fprintf(&b, "<notebook name=%q>\n", filepath.Base(m.path))
	for i, c := range m.nb.Cells {
		head := fmt.Sprintf("## cell %d id=%s %s", i+1, c.EnsureID(), c.Type)
		if c.ExecutionCount != nil {
			head += fmt.Sprintf(" [%d]", *c.ExecutionCount)
		}
		if i == m.sel {
			head += " (selected)"
		}
		b.WriteString(head + "\n")
		lang := ""
		if c.Type == notebook.Code {
			lang = "python"
		}
		fmt.Fprintf(&b, "```%s\n%s\n```\n", lang, c.Source)
		if out := aiOutputs(c); out != "" {
			fmt.Fprintf(&b, "output:\n```\n%s\n```\n", out)
		}
	}
	b.WriteString("</notebook>")
	var imgs []ai.Part
	for _, o := range m.cell().Outputs {
		if len(imgs) >= aiMaxImages {
			break
		}
		if img := aiImage(o); img != nil {
			imgs = append(imgs, ai.Part{Image: img})
		}
	}
	return b.String(), imgs
}

// aiOutputs is a cell's outputs as plain text, trimmed.
func aiOutputs(c *notebook.Cell) string {
	var parts []string
	for _, o := range c.Outputs {
		switch o.OutputType {
		case "stream":
			parts = append(parts, termText(o.Text))
		case "error":
			tb := ansi.Strip(strings.Join(o.Traceback, "\n"))
			if tb == "" {
				tb = o.Ename + ": " + o.Evalue
			}
			parts = append(parts, tb)
		default:
			if mt, _, ok := imageData(o); ok {
				parts = append(parts, "["+mt+" image]")
				continue
			}
			if kind, ok := browserKind(o); ok {
				parts = append(parts, "["+kind+"]")
				continue
			}
			if s, ok := o.DataText("text/plain"); ok {
				parts = append(parts, s)
			} else if s, ok := o.DataText("text/markdown"); ok {
				parts = append(parts, s)
			}
		}
	}
	s := strings.TrimSpace(strings.Join(parts, "\n"))
	if len(s) > aiMaxOutput {
		s = s[:aiMaxOutput] + fmt.Sprintf("\n... (%d more characters)", len(s)-aiMaxOutput)
	}
	return s
}

// aiImage turns a png/jpeg output into an image part, shrunk if huge.
func aiImage(o *notebook.Output) *ai.Image {
	mt, data, ok := imageData(o)
	if !ok || (mt != "image/png" && mt != "image/jpeg") {
		return nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	img := &ai.Image{Mime: mt, Data: data, W: cfg.Width, H: cfg.Height}
	if max(cfg.Width, cfg.Height) <= aiImageSide {
		return img
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return img
	}
	scale := float64(aiImageSide) / float64(max(cfg.Width, cfg.Height))
	tw, th := max(int(float64(cfg.Width)*scale), 1), max(int(float64(cfg.Height)*scale), 1)
	px := boxResize(src, tw, th, color.RGBA{255, 255, 255, 255})
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for i, c := range px {
		dst.Set(i%tw, i/tw, c)
	}
	var buf bytes.Buffer
	if png.Encode(&buf, dst) != nil {
		return img
	}
	return &ai.Image{Mime: "image/png", Data: buf.Bytes(), W: tw, H: th}
}

// aiWidths splits the screen: the pane takes the right side, or all of
// it when the terminal is too narrow for both.
func (m *Model) aiWidths() (pane, nb int) {
	if m.width < 100 {
		return m.width, 0
	}
	pane = max(40, m.width*2/5)
	return pane, m.width - pane - 1
}

// bodyWithAI draws the notebook with the AI pane beside it.
func (m *Model) bodyWithAI() (string, *tea.Cursor) {
	if m.ai == nil || !m.ai.open {
		return m.renderBody()
	}
	pw, nbW := m.aiWidths()
	pane, pcur := m.renderAI(pw, m.bodyHeight())
	if nbW == 0 {
		return pane, pcur
	}
	saved := m.width
	m.width = nbW
	body, cur := m.renderBody()
	m.width = saved
	left := strings.Split(body, "\n")
	right := strings.Split(pane, "\n")
	sep := m.st.faint.Render("│")
	out := make([]string, max(len(left), len(right)))
	for i := range out {
		l, r := "", ""
		if i < len(left) {
			l = ansi.Truncate(left[i], nbW, "")
		}
		if i < len(right) {
			r = right[i]
		}
		out[i] = padRight(l, nbW) + sep + r
	}
	if pcur != nil {
		pcur.X += nbW + 1
		cur = pcur
	}
	return strings.Join(out, "\n"), cur
}

func (m *Model) renderAI(width, height int) (string, *tea.Cursor) {
	p := m.ai
	inner := max(width-2, 10)
	title := " AI"
	if p.sess != nil {
		title += " · " + p.sess.P.Name()
	}
	head := m.st.accent.Bold(true).Render(ansi.Truncate(title, width, "…"))

	var lines []string
	for i, e := range p.entries {
		lines = append(lines, m.aiEntryLines(e, inner, p.streaming && i == len(p.entries)-1)...)
		lines = append(lines, "")
	}
	if p.streaming {
		spin := spinner[m.frame%len(spinner)]
		lines = append(lines, " "+m.st.busy.Render(spin+" thinking"))
	}
	if len(p.entries) == 0 && !p.streaming {
		hint := "Ask about the notebook. It sees every cell, the outputs and the selected cell's plots.\n\nenter send · esc back to the notebook · ga come back · gA close · pgup/pgdown scroll · ctrl+c stop · /clear forget the chat"
		if p.cfgErr != "" {
			hint = p.cfgErr
		}
		for _, l := range strings.Split(ansi.Wordwrap(hint, inner, " "), "\n") {
			lines = append(lines, " "+m.st.dim.Render(l))
		}
	}

	foot := m.st.dim.Render(" " + p.usageLine())
	p.input.SetWidth(max(inner-2, 5))
	input := " " + p.input.View()

	// head, transcript, foot, input
	room := max(height-3, 1)
	maxScroll := max(len(lines)-room, 0)
	p.scroll = min(p.scroll, maxScroll)
	end := len(lines) - p.scroll
	start := max(end-room, 0)
	view := lines[start:end]
	for len(view) < room {
		view = append(view, "")
	}
	out := append([]string{head}, view...)
	out = append(out, foot, input)

	var cur *tea.Cursor
	if p.typing {
		if c := p.input.Cursor(); c != nil {
			cc := *c
			cc.X += 1
			cc.Y = headerH + len(out) - 1
			cur = &cc
		}
	}
	for i, l := range out {
		out[i] = padRight(ansi.Truncate(l, width, ""), width)
	}
	return strings.Join(out, "\n"), cur
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *Model) aiEntryLines(e aiEntry, width int, live bool) []string {
	wrap := func(s string, st lipgloss.Style, prefix string) []string {
		var out []string
		for _, l := range strings.Split(ansi.Wordwrap(s, width-len([]rune(prefix)), " "), "\n") {
			out = append(out, prefix+st.Render(l))
		}
		return out
	}
	switch e.Kind {
	case "user":
		return wrap(e.Text, m.st.accent.Bold(true), " › ")
	case "thinking":
		return wrap(strings.TrimSpace(e.Text), m.st.dim.Italic(true), " ")
	case "note":
		return wrap(e.Text, m.st.dim, " · ")
	case "error":
		return wrap(e.Text, m.st.errText, " ✗ ")
	}
	// rendered markdown once it's done; plain while it streams so each
	// delta doesn't go through glamour
	if live {
		return wrap(e.Text, lipgloss.NewStyle(), " ")
	}
	return strings.Split(m.markdown(e.Text, width), "\n")
}

func (p *aiPane) usageLine() string {
	if p.total.In == 0 && p.total.Out == 0 {
		return "effort " + p.effort
	}
	s := fmt.Sprintf("last %s in, %s out", short(p.last.In+p.last.CacheRead+p.last.CacheWrite), short(p.last.Out))
	if p.last.CacheRead > 0 {
		s += fmt.Sprintf(", %s cached", short(p.last.CacheRead))
	}
	s += fmt.Sprintf(" · total %s in, %s out", short(p.total.In+p.total.CacheRead+p.total.CacheWrite), short(p.total.Out))
	return s
}

func short(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

func (m *Model) aiState() json.RawMessage {
	p := m.ai
	if p == nil || len(p.hist) == 0 {
		return nil
	}
	s := aiSaved{Hist: p.hist, Entries: p.entries, Snap: p.snap, Total: p.total}
	if p.sess != nil {
		s.Model = p.sess.P.Name()
	}
	b, _ := json.Marshal(s)
	return b
}

// restoreAI picks a chat back up after reattaching. A different model
// than before starts fresh, since its history wouldn't be valid there.
func (m *Model) restoreAI(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var s aiSaved
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	p := newAIPane()
	if p.sess == nil || p.sess.P.Name() != s.Model {
		return
	}
	p.hist, p.entries, p.snap, p.total = s.Hist, s.Entries, s.Snap, s.Total
	m.ai = p
}
