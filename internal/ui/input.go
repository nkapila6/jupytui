package ui

import (
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/kernel"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// input() / getpass(): the kernel asks for a line, we show a prompt
// under the cell and send back what's typed.

type inputState struct {
	cell *notebook.Cell
	k    *kernel.Kernel
	req  *kernel.InputRequest
	box  textinput.Model
	line int // body line of the prompt, set while rendering
}

func (m *Model) startInput(k *kernel.Kernel, c *notebook.Cell, req *kernel.InputRequest) tea.Cmd {
	if m.mode == editMode {
		m.stopEdit()
	}
	box := textinput.New()
	box.Prompt = ""
	box.SetVirtualCursor(false)
	if req.Password {
		box.EchoMode = textinput.EchoPassword
	}
	m.input = &inputState{cell: c, k: k, req: req, box: box, line: -1}
	for i, cc := range m.nb.Cells {
		if cc == c {
			m.sel = i
		}
	}
	return m.input.box.Focus()
}

func (m *Model) inputKey(msg tea.KeyPressMsg) tea.Cmd {
	in := m.input
	switch msg.String() {
	case "enter":
		value := in.box.Value()
		shown := value
		if in.req.Password {
			shown = strings.Repeat("·", len([]rune(value)))
		}
		// what Jupyter shows: the prompt and the answer, in the output
		out, err := notebook.NewOutput("stream", []byte(`{"name":"stdout","text":`+jsonString(in.req.Prompt+shown+"\n")+`}`))
		if err == nil {
			in.cell.AddOutput(out)
		}
		if err := in.k.Reply(in.req, value); err != nil {
			m.msg = "input: " + err.Error()
		}
		m.input = nil
		return nil
	case "ctrl+c":
		m.input = nil
		return m.interrupt()
	}
	var cmd tea.Cmd
	in.box, cmd = in.box.Update(msg)
	return cmd
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[r>>4])
				b.WriteByte("0123456789abcdef"[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// inputLine is the prompt line drawn under the waiting cell.
func (m *Model) inputLine() string {
	in := m.input
	return m.st.busy.Render(in.req.Prompt) + in.box.View()
}
