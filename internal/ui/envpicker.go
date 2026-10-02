package ui

import (
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/nkapila6/jupytui/internal/envs"
	"github.com/nkapila6/jupytui/internal/kernel"
)

// :env lists Python environments; picking one restarts the kernel in it
// and remembers the choice in the notebook's metadata.

type envsMsg []envs.Env

type envPicker struct {
	list    []envs.Env
	sel     int
	loading bool
}

func (m *Model) openEnvPicker() tea.Cmd {
	m.picker = &envPicker{loading: true}
	dir := filepath.Dir(m.path)
	// uv and conda can be slow, don't block the UI on them
	return func() tea.Msg { return envsMsg(envs.Discover(dir)) }
}

func (m *Model) handleEnvs(list envsMsg) {
	p := m.picker
	if p == nil {
		return
	}
	p.list, p.loading = list, false
	for i, e := range p.list {
		if e.Kind == m.env.Kind && e.Python == m.env.Python {
			p.sel = i
		}
	}
}

func (m *Model) pickerKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.picker
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.picker = nil
	case "j", "down", "ctrl+n":
		p.sel = min(p.sel+1, max(len(p.list)-1, 0))
	case "k", "up", "ctrl+p":
		p.sel = max(p.sel-1, 0)
	case "g", "home":
		p.sel = 0
	case "G", "end":
		p.sel = max(len(p.list)-1, 0)
	case "enter":
		if p.loading || len(p.list) == 0 {
			return nil
		}
		m.picker = nil
		return m.useEnv(p.list[p.sel])
	}
	return nil
}

// useEnv switches the kernel to e and saves the choice.
func (m *Model) useEnv(e envs.Env) tea.Cmd {
	if e.Kind == m.env.Kind && e.Python == m.env.Python && m.k != nil {
		m.msg = "already using " + e.Label()
		return nil
	}
	m.env = e
	m.host.opts.Cmd = kernelCmd(e)
	m.host.opts.Remote = remoteFor(e)
	m.remote = nil
	if e.Kind == envs.Remote {
		m.remote = &remoteInfo{host: e.Host}
	}
	saved := envs.SavePath(e, filepath.Dir(m.path))
	if saved != m.nb.JupytuiPython() {
		m.nb.SetJupytuiPython(saved)
		m.dirty = true
	}
	cmd := m.restart()
	m.setLSPPython()
	m.msg = "switching kernel to " + e.Label()
	return cmd
}

func kernelCmd(e envs.Env) []string {
	if e.Kind == envs.Project || e.Kind == envs.Remote {
		return nil // kernel.DefaultCmd, or the remote's own uv
	}
	return kernel.CmdForPython(e.Python)
}

func remoteFor(e envs.Env) *kernel.Remote {
	if e.Kind != envs.Remote {
		return nil
	}
	return &kernel.Remote{Host: e.Host, Dir: e.Dir}
}

func (m *Model) renderPicker() string {
	p := m.picker
	var b strings.Builder
	b.WriteString(m.st.accent.Bold(true).Render("Python environment") + "\n\n")
	if p.loading {
		b.WriteString(m.st.dim.Render("looking for environments..."))
	}
	// mark(2) + name(24) + space + path, inside padding(4) and border(2)
	pathW := max(min(m.width-36, 60), 10)
	for i, e := range p.list {
		current := e.Kind == m.env.Kind && e.Python == m.env.Python
		mark := "  "
		if current {
			mark = m.st.ok.Render("● ")
		}
		name := padRight(e.Label(), 24)
		path := e.Python
		switch e.Kind {
		case envs.Project:
			path = "uv run in " + filepath.Base(filepath.Dir(m.path))
		case envs.Remote:
			path = e.Host + ":" + e.Dir
		}
		if len(path) > pathW {
			path = "…" + path[len(path)-pathW+1:]
		}
		line := mark + name + " " + m.st.dim.Render(path)
		if i == p.sel {
			// plain text inside the highlight, a styled mark would reset its background
			plain := "  "
			if current {
				plain = "● "
			}
			line = m.st.selection.Render(padRight(plain+name+" "+path, 26+pathW))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n" + m.st.dim.Render(fmt.Sprintf("j/k move · enter switch kernel · esc close · %d found", len(p.list))))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.st.accent.GetForeground()).
		Padding(1, 2).
		Render(b.String())
	return lipgloss.Place(max(m.width, minWidth), m.bodyHeight(), lipgloss.Center, lipgloss.Center, box)
}
