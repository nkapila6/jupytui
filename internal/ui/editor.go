package ui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/notebook"
)

// extEdit is a cell being edited in an outside editor via a temp file.
type extEdit struct {
	cell *notebook.Cell
	dir  string
	path string
	mod  time.Time
}

type editorDoneMsg struct{ err error }

type editorPollMsg struct{}

// hostOpen runs inside the nvim that hosts our terminal. It opens the
// cell in a new tab (which hides the float, since floats belong to a
// tab) and, once the buffer is gone, writes a .done marker and jumps
// back to our terminal in insert mode.
const hostOpen = `(function(f)
  local tab, win = vim.api.nvim_get_current_tabpage(), vim.api.nvim_get_current_win()
  vim.cmd("tabedit " .. vim.fn.fnameescape(f))
  local buf = vim.api.nvim_get_current_buf()
  vim.bo[buf].bufhidden = "wipe"
  vim.api.nvim_create_autocmd("BufWipeout", { buffer = buf, once = true, callback = function()
    vim.fn.writefile({}, f .. ".done")
    vim.schedule(function()
      if vim.api.nvim_tabpage_is_valid(tab) then vim.api.nvim_set_current_tabpage(tab) end
      if vim.api.nvim_win_is_valid(win) then
        vim.api.nvim_set_current_win(win)
        vim.cmd("startinsert")
      end
    end)
  end })
  return ""
end)(_A)`

func (m *Model) openEditor(c *notebook.Cell) tea.Cmd {
	if m.ext != nil {
		m.msg = "already editing a cell outside"
		return nil
	}
	dir, err := os.MkdirTemp("", "jupytui-edit-")
	if err != nil {
		m.msg = "editor: " + err.Error()
		return nil
	}
	path := filepath.Join(dir, "cell"+m.extFor(c))
	if err := os.WriteFile(path, []byte(c.Source), 0o600); err != nil {
		os.RemoveAll(dir)
		m.msg = "editor: " + err.Error()
		return nil
	}
	m.ext = &extEdit{cell: c, dir: dir, path: path, mod: modTime(path)}

	// inside nvim's :terminal, edit in the host nvim instead of nesting one
	if sock := os.Getenv("NVIM"); sock != "" {
		lua := strings.ReplaceAll(hostOpen, "\n", " ")
		expr := fmt.Sprintf("luaeval(%s, %s)", vimString(lua), vimString(path))
		if err := exec.Command("nvim", "--server", sock, "--remote-expr", expr).Run(); err == nil {
			m.msg = "editing in nvim, :wq to come back"
			return pollEditor()
		}
	}

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	// through sh so EDITOR="code --wait" style values work
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "sh", path)
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return editorDoneMsg{err} })
}

func (m *Model) handleEditor(msg tea.Msg) tea.Cmd {
	e := m.ext
	if e == nil {
		return nil
	}
	switch msg := msg.(type) {
	case editorDoneMsg:
		if msg.err != nil {
			m.msg = "editor: " + msg.err.Error()
		}
		m.syncFromEditor()
		m.ext.cleanup()
		m.ext = nil
		return nil
	case editorPollMsg:
		if mt := modTime(e.path); !mt.Equal(e.mod) {
			e.mod = mt
			m.syncFromEditor()
		}
		if _, err := os.Stat(e.path + ".done"); err == nil {
			m.syncFromEditor()
			m.ext.cleanup()
			m.ext = nil
			m.msg = ""
			return nil
		}
		return pollEditor()
	}
	return nil
}

func (m *Model) syncFromEditor() {
	b, err := os.ReadFile(m.ext.path)
	if err != nil {
		return
	}
	// editors add a final newline that notebook cells don't have
	src := strings.TrimSuffix(string(b), "\n")
	if c := m.ext.cell; c.Source != src {
		c.Source = src
		m.dirty = true
	}
}

func (e *extEdit) cleanup() {
	if e != nil {
		os.RemoveAll(e.dir)
	}
}

// extFor picks a file extension so the editor's filetype, LSP and
// treesitter kick in.
func (m *Model) extFor(c *notebook.Cell) string {
	switch c.Type {
	case notebook.Markdown:
		return ".md"
	case notebook.Raw:
		return ".txt"
	}
	switch strings.ToLower(m.lang) {
	case "python":
		return ".py"
	case "r":
		return ".R"
	case "julia":
		return ".jl"
	case "javascript", "typescript":
		return ".ts"
	case "go":
		return ".go"
	case "rust":
		return ".rs"
	}
	return ".txt"
}

func pollEditor() tea.Cmd {
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return editorPollMsg{} })
}

func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// vimString quotes s as a Vim single-quoted string literal.
func vimString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
