package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/nkapila6/jupytui/internal/ai"
)

// Settings: one table drives the pane (gs, :settings), :set and the
// config file, so they can't drift apart. Anything changed either way is
// written to ~/.config/jupytui/config.json and applied on the next start.
// API keys never go in there, they stay in the environment.

type settingKind int

const (
	boolSetting settingKind = iota
	choiceSetting
	textSetting
	actionSetting // opens something, not saved
)

type setting struct {
	key     string // in the config file, and the :set name
	group   string
	label   string
	help    string
	kind    settingKind
	choices []string
	get     func(m *Model) string
	set     func(m *Model, v string) tea.Cmd
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// opt wraps the existing :set on/off options
func opt(name string, get func(m *Model) bool) (func(*Model) string, func(*Model, string) tea.Cmd) {
	return func(m *Model) string { return boolStr(get(m)) },
		func(m *Model, v string) tea.Cmd {
			if v == "true" {
				m.setOption(name)
			} else {
				m.setOption("no" + name)
			}
			return nil
		}
}

var settings = func() []setting {
	var s []setting
	add := func(key, group, label, help string, get func(*Model) bool) {
		g, st := opt(key, get)
		s = append(s, setting{key: key, group: group, label: label, help: help, kind: boolSetting, get: g, set: st})
	}
	add("vim", "Editor", "Vim keys in cells", "off: plain typing, arrows to move", func(m *Model) bool { return m.vimOn })
	add("number", "Editor", "Line numbers", "one numbering across the whole notebook", func(m *Model) bool { return m.number })
	add("relativenumber", "Editor", "Relative numbers", "count lines from the cursor, for 5j and friends", func(m *Model) bool { return m.relative })
	add("lsp", "Editor", "Language server", "basedpyright through uvx: completions, hover, go to definition", func(m *Model) bool { return m.lspOn })
	add("diag", "Editor", "Diagnostics", "errors and warnings from the language server", func(m *Model) bool { return m.diagOn })
	add("saveoutputs", "Notebook", "Save outputs", "off: outputs stay on screen but aren't written to the file", func(m *Model) bool { return !m.noSaveOutputs })
	add("reactive", "Notebook", "Reactive", "rerun dependent cells after a cell runs (always on for marimo files)", func(m *Model) bool { return m.reactive })
	s = append(s, setting{
		key: "images", group: "Notebook", label: "Images",
		help: "auto picks from the terminal; JUPYTUI_IMAGES still wins", kind: choiceSetting,
		choices: []string{"auto", "kitty", "sixel", "blocks"},
		get: func(m *Model) string {
			if m.imagesPref == "" {
				return "auto"
			}
			return m.imagesPref
		},
		set: func(m *Model, v string) tea.Cmd {
			m.imagesPref = v
			if v == "auto" {
				m.imagesPref = ""
				v = detectGraphics().String()
			}
			return m.setGraphics(v)
		},
	})
	s = append(s, setting{
		key: "env", group: "Kernel", label: "Python environment", kind: actionSetting,
		help: "enter opens the picker; the choice is saved in the notebook",
		get:  func(m *Model) string { return m.env.Label() },
		set:  func(m *Model, _ string) tea.Cmd { m.settings = nil; return m.openEnvPicker() },
	})
	s = append(s, setting{
		key: "ai", group: "AI", label: "Model", kind: textSetting,
		help: "provider/model: anthropic/claude-opus-5-5, openrouter/<vendor>/<model>, ollama/<model>, openai/<model>. Empty: JUPYTUI_AI or the default",
		get:  func(m *Model) string { return m.aiModel },
		set: func(m *Model, v string) tea.Cmd {
			v = strings.TrimSpace(v)
			if v != "" {
				if _, err := ai.Parse(v); err != nil {
					m.msg = err.Error()
					return nil
				}
			}
			m.aiModel = v
			m.resetAIProvider()
			return nil
		},
	})
	s = append(s, setting{
		key: "aieffort", group: "AI", label: "Effort", kind: choiceSetting,
		help:    "how hard the model thinks: low is quick and cheap, high for hard problems",
		choices: []string{"low", "medium", "high"},
		get:     func(m *Model) string { return m.aiEffort },
		set: func(m *Model, v string) tea.Cmd {
			m.aiEffort = v
			if m.ai != nil {
				m.ai.effort = v
			}
			return nil
		},
	})
	s = append(s, setting{
		key: "aiimages", group: "AI", label: "Send plots as images", kind: boolSetting,
		help: "off: plots go as [image WxH] placeholders, which costs fewer tokens",
		get:  func(m *Model) string { return boolStr(m.aiImages) },
		set: func(m *Model, v string) tea.Cmd {
			m.aiImages = v == "true"
			if m.ai != nil && m.ai.sess != nil {
				m.ai.sess.NoImages = !m.aiImages
			}
			return nil
		},
	})
	return s
}()

func findSetting(key string) *setting {
	for i := range settings {
		if settings[i].key == key {
			return &settings[i]
		}
	}
	return nil
}

// :set names that mean the same setting
var setAliases = map[string]string{"nu": "number", "rnu": "relativenumber", "diagnostics": "diag"}

// setFromCommand handles :set name, :set noname and :set name=value for
// anything in the table, and remembers it. false means not ours.
func (m *Model) setFromCommand(arg string) (tea.Cmd, bool) {
	name, value, hasValue := strings.Cut(arg, "=")
	if !hasValue {
		value = "true"
		if s := findSetting(resolveAlias(name)); s == nil || s.kind != boolSetting {
			if rest, ok := strings.CutPrefix(name, "no"); ok {
				name, value = rest, "false"
			}
		}
	}
	s := findSetting(resolveAlias(name))
	if s == nil || s.kind == actionSetting {
		return nil, false
	}
	if s.kind == boolSetting && hasValue && value != "true" && value != "false" {
		m.msg = s.key + " is on/off: :set " + s.key + " or :set no" + s.key
		return nil, true
	}
	if s.kind == choiceSetting && !contains(s.choices, value) {
		m.msg = s.key + "=" + strings.Join(s.choices, "|")
		return nil, true
	}
	return m.changeSetting(s, value), true
}

func resolveAlias(name string) string {
	if a, ok := setAliases[name]; ok {
		return a
	}
	return name
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// changeSetting applies a value and saves it.
func (m *Model) changeSetting(s *setting, v string) tea.Cmd {
	msg := m.msg
	cmd := s.set(m, v)
	if s.kind == actionSetting {
		return cmd
	}
	if s.get(m) != v && !(s.key == "images" && v == "auto") {
		return cmd // refused, message already set
	}
	if m.cfg == nil {
		m.cfg = map[string]string{}
	}
	m.cfg[s.key] = v
	if err := saveConfig(m.cfg); err != nil {
		m.msg = "couldn't save settings: " + err.Error()
	} else if m.msg == msg || m.msg == "" {
		m.msg = s.label + ": " + display(s, v)
	}
	return cmd
}

func display(s *setting, v string) string {
	switch {
	case s.kind == boolSetting && v == "true":
		return "on"
	case s.kind == boolSetting:
		return "off"
	case v == "":
		return "auto"
	}
	return v
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "jupytui", "config.json")
}

func loadConfig() map[string]string {
	cfg := map[string]string{}
	b, err := os.ReadFile(configPath())
	if err == nil {
		json.Unmarshal(b, &cfg)
	}
	return cfg
}

func saveConfig(cfg map[string]string) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// applyConfig sets saved values at startup, before anything is drawn.
func (m *Model) applyConfig() {
	m.cfg = loadConfig()
	for key, v := range m.cfg {
		s := findSetting(key)
		if s == nil || s.kind == actionSetting {
			continue
		}
		if key == "images" {
			// the terminal isn't drawn yet, and the env var wins
			if g, ok := parseGfx(v); ok && os.Getenv("JUPYTUI_IMAGES") == "" {
				m.imagesPref = v
				m.gfxMode = g
			}
			continue
		}
		s.set(m, v)
	}
	m.msg = ""
}

// the pane

type settingsPanel struct {
	sel  int
	edit *textinput.Model
}

func (m *Model) openSettings() {
	if m.mode == editMode {
		m.stopEdit()
	}
	m.settings = &settingsPanel{}
}

func (m *Model) settingsKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.settings
	s := &settings[p.sel]
	if p.edit != nil {
		switch msg.String() {
		case "esc":
			p.edit = nil
			return nil
		case "enter":
			v := p.edit.Value()
			p.edit = nil
			return m.changeSetting(s, v)
		}
		var cmd tea.Cmd
		*p.edit, cmd = p.edit.Update(msg)
		return cmd
	}
	switch msg.String() {
	case "esc", "q":
		m.settings = nil
	case "j", "down", "tab":
		p.sel = (p.sel + 1) % len(settings)
	case "k", "up", "shift+tab":
		p.sel = (p.sel - 1 + len(settings)) % len(settings)
	case "g", "home":
		p.sel = 0
	case "G", "end":
		p.sel = len(settings) - 1
	case "enter", "space", "l", "right":
		return m.stepSetting(s, 1)
	case "h", "left":
		return m.stepSetting(s, -1)
	}
	return nil
}

func (m *Model) stepSetting(s *setting, d int) tea.Cmd {
	switch s.kind {
	case boolSetting:
		v := "true"
		if s.get(m) == "true" {
			v = "false"
		}
		return m.changeSetting(s, v)
	case choiceSetting:
		i := 0
		for j, c := range s.choices {
			if c == s.get(m) {
				i = j
			}
		}
		return m.changeSetting(s, s.choices[(i+d+len(s.choices))%len(s.choices)])
	case textSetting:
		box := textinput.New()
		box.Prompt = ""
		box.SetVirtualCursor(false)
		box.SetValue(s.get(m))
		box.CursorEnd()
		m.settings.edit = &box
		return box.Focus()
	case actionSetting:
		return s.set(m, "")
	}
	return nil
}

func (m *Model) renderSettings() (string, *tea.Cursor) {
	p := m.settings
	width := max(m.width, minWidth)
	lines := []string{" " + m.st.accent.Bold(true).Render("Settings") + "  " + m.st.dim.Render("saved to "+tildePath(configPath())), ""}
	labelW := 24
	var cur *tea.Cursor
	group := ""
	for i := range settings {
		s := &settings[i]
		if s.group != group {
			if group != "" {
				lines = append(lines, "")
			}
			group = s.group
			lines = append(lines, " "+lipglossBold(group))
		}
		v := s.get(m)
		var val string
		switch s.kind {
		case boolSetting:
			if v == "true" {
				val = m.st.ok.Render("on")
			} else {
				val = m.st.dim.Render("off")
			}
		case choiceSetting:
			val = "‹ " + v + " ›"
		case textSetting:
			if v == "" {
				val = m.st.dim.Render("auto (" + m.aiAutoName() + ")")
			} else {
				val = v
			}
		case actionSetting:
			val = v + m.st.dim.Render("  ↵")
		}
		label := padRight("   "+s.label, labelW+3)
		if i == p.sel && p.edit != nil {
			if c := p.edit.Cursor(); c != nil {
				cc := *c
				cc.X += labelW + 3 + 1
				cc.Y = headerH + len(lines)
				cur = &cc
			}
			val = "[" + p.edit.View() + "]"
		}
		line := label + val
		if i == p.sel {
			line = m.st.selection.Render(padRight(ansi.Strip(label)+ansi.Strip(val), width-2))
			if p.edit != nil {
				line = label + val
			}
		}
		lines = append(lines, line)
	}
	lines = append(lines, "")
	for _, l := range strings.Split(ansi.Wordwrap(settings[p.sel].help, width-4, " "), "\n") {
		lines = append(lines, "  "+m.st.dim.Render(l))
	}
	lines = append(lines, "", "  "+m.st.dim.Render("j/k move · enter/space toggle or edit · h/l cycle · esc close"))
	for len(lines) < m.bodyHeight() {
		lines = append(lines, "")
	}
	return strings.Join(lines[:min(len(lines), m.bodyHeight())], "\n"), cur
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(p, home); ok {
			return "~" + rest
		}
	}
	return p
}

// aiAutoName is what an empty model setting resolves to.
func (m *Model) aiAutoName() string {
	p, err := ai.FromEnv()
	if err != nil {
		return "not configured"
	}
	return p.Name()
}
