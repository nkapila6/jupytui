// Package envs finds Python environments a kernel can run in: venvs
// near the notebook, conda envs and uv-installed interpreters.
package envs

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Kind string

const (
	Project Kind = "project" // uv run in the notebook dir, the default
	Venv    Kind = "venv"
	Conda   Kind = "conda"
	Python  Kind = "python"
)

type Env struct {
	Kind    Kind
	Name    string
	Python  string // interpreter path, empty for Project
	Version string
}

// Label is how the env shows up in the header and picker.
func (e Env) Label() string {
	if e.Version == "" {
		return e.Name
	}
	return e.Name + " · " + e.Version
}

// Discover lists environments for a notebook in dir, project first.
// Slow sources (uv, conda) are bounded by a timeout.
func Discover(dir string) []Env {
	out := []Env{{Kind: Project, Name: "project (uv)"}}
	seen := map[string]bool{}
	add := func(e Env) {
		key := e.Python
		if real, err := filepath.EvalSymlinks(e.Python); err == nil {
			key = real
		}
		// venvs symlink their python, so dedupe on the venv dir instead
		if e.Kind == Venv {
			key = filepath.Dir(filepath.Dir(e.Python))
		}
		if e.Python == "" || seen[key] {
			return
		}
		if _, err := os.Stat(e.Python); err != nil {
			return
		}
		seen[key] = true
		out = append(out, e)
	}

	for _, v := range findVenvs(dir) {
		add(v)
	}
	for _, c := range condaEnvs() {
		add(c)
	}
	for _, p := range uvPythons() {
		add(p)
	}
	return out
}

// findVenvs checks the notebook dir, its parents up to home (or /),
// its immediate subdirs, $VIRTUAL_ENV and ~/.virtualenvs.
func findVenvs(dir string) []Env {
	home, _ := os.UserHomeDir()
	var cands []string
	if v := os.Getenv("VIRTUAL_ENV"); v != "" {
		cands = append(cands, v)
	}
	for d := dir; ; d = filepath.Dir(d) {
		for _, name := range []string{".venv", "venv", "env", ".env"} {
			cands = append(cands, filepath.Join(d, name))
		}
		if d == home || d == filepath.Dir(d) {
			break
		}
	}
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				cands = append(cands, filepath.Join(dir, e.Name()))
			}
		}
	}
	if home != "" {
		if ents, err := os.ReadDir(filepath.Join(home, ".virtualenvs")); err == nil {
			for _, e := range ents {
				cands = append(cands, filepath.Join(home, ".virtualenvs", e.Name()))
			}
		}
	}

	var out []Env
	for _, c := range cands {
		cfg := filepath.Join(c, "pyvenv.cfg")
		if _, err := os.Stat(cfg); err != nil {
			continue
		}
		out = append(out, Env{
			Kind:    Venv,
			Name:    shortPath(c, dir, home),
			Python:  filepath.Join(c, "bin", "python"),
			Version: venvVersion(cfg),
		})
	}
	return out
}

func venvVersion(cfg string) string {
	f, err := os.Open(cfg)
	if err != nil {
		return ""
	}
	defer f.Close()
	var version string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "version_info", "version":
			version = strings.TrimSpace(v)
		}
	}
	return shortVersion(version)
}

func condaEnvs() []Env {
	var bin string
	for _, b := range []string{"conda", "mamba", "micromamba"} {
		if p, err := exec.LookPath(b); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		return nil
	}
	out, err := run(bin, "env", "list", "--json")
	if err != nil {
		return nil
	}
	var res struct {
		Envs []string `json:"envs"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	var envs []Env
	for _, p := range res.Envs {
		name := filepath.Base(p)
		if filepath.Base(filepath.Dir(p)) != "envs" {
			name = "base"
		}
		envs = append(envs, Env{Kind: Conda, Name: "conda: " + name, Python: filepath.Join(p, "bin", "python")})
	}
	return envs
}

func uvPythons() []Env {
	out, err := run("uv", "python", "list", "--only-installed", "--output-format", "json")
	if err != nil {
		return nil
	}
	var res []struct {
		Key     string `json:"key"`
		Version string `json:"version"`
		Path    string `json:"path"`
	}
	if json.Unmarshal(out, &res) != nil {
		return nil
	}
	var envs []Env
	for _, p := range res {
		if p.Path == "" {
			continue
		}
		envs = append(envs, Env{Kind: Python, Name: "python " + shortVersion(p.Version), Python: p.Path})
	}
	return envs
}

func run(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// shortVersion trims 3.12.13.final.0 style versions to 3.12.13.
func shortVersion(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	return strings.Join(parts, ".")
}

func shortPath(p, dir, home string) string {
	if rel, err := filepath.Rel(dir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	if home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// Resolve turns a saved python path (maybe relative to the notebook
// dir) back into an Env. ok is false if it no longer exists.
func Resolve(saved, dir string) (Env, bool) {
	if saved == "" {
		return Env{Kind: Project, Name: "project (uv)"}, true
	}
	p := saved
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	if _, err := os.Stat(p); err != nil {
		return Env{}, false
	}
	venv := filepath.Dir(filepath.Dir(p))
	cfg := filepath.Join(venv, "pyvenv.cfg")
	if _, err := os.Stat(cfg); err == nil {
		home, _ := os.UserHomeDir()
		return Env{Kind: Venv, Name: shortPath(venv, dir, home), Python: p, Version: venvVersion(cfg)}, true
	}
	return Env{Kind: Python, Name: filepath.Base(p), Python: p}, true
}

// SavePath is what goes in notebook metadata: relative when the
// interpreter lives under the notebook dir so the folder can move.
func SavePath(e Env, dir string) string {
	if e.Kind == Project {
		return ""
	}
	if rel, err := filepath.Rel(dir, e.Python); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return e.Python
}
