// Package marimo reads and writes marimo notebooks: plain .py files with
// an `app = marimo.App()` and one decorated function per cell, whose
// parameters are what the cell reads from other cells and whose return
// tuple is what it shares.
package marimo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/nkapila6/jupytui/internal/notebook"
)

// metadata keys we keep marimo-only details under
const (
	kindKey = "marimo_kind"      // cell (default), function, class_definition, setup
	decoKey = "marimo_decorator" // e.g. hide_code=True
	appKey  = "marimo_app"       // App(...) arguments
	verKey  = "marimo_generated_with"
)

// IsMarimo reports whether a .py file is a marimo app.
func IsMarimo(src []byte) bool {
	return bytes.Contains(src, []byte("marimo.App(")) && bytes.Contains(src, []byte("@app."))
}

var (
	decoRe  = regexp.MustCompile(`^@app\.(cell|function|class_definition)(?:\((.*)\))?\s*$`)
	appRe   = regexp.MustCompile(`^app\s*=\s*marimo\.App\((.*)\)\s*$`)
	verRe   = regexp.MustCompile(`^__generated_with\s*=\s*"([^"]*)"`)
	mdRe    = regexp.MustCompile(`(?s)^mo\.md\(\s*(r?)("""|''')(.*)("""|''')\s*\)\s*$`)
	retLine = regexp.MustCompile(`^return\b`)
)

// Load parses a marimo file into a notebook.
func Load(path string) (*notebook.Notebook, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(src []byte) (*notebook.Notebook, error) {
	if !IsMarimo(src) {
		return nil, errors.New("not a marimo notebook (no marimo.App)")
	}
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	nb := notebook.New()
	i := 0
	// indented block starting at line i, up to the next top-level line
	block := func(from int) (int, []string) {
		j := from
		for j < len(lines) && (strings.TrimSpace(lines[j]) == "" || strings.HasPrefix(lines[j], " ") || strings.HasPrefix(lines[j], "\t")) {
			j++
		}
		return j, lines[from:j]
	}
	for i < len(lines) {
		l := lines[i]
		switch {
		case verRe.MatchString(l):
			nb.SetMeta(verKey, verRe.FindStringSubmatch(l)[1])
			i++
		case appRe.MatchString(l):
			nb.SetMeta(appKey, appRe.FindStringSubmatch(l)[1])
			i++
		case strings.HasPrefix(l, "with app.setup"):
			end, body := block(i + 1)
			c := notebook.NewCell(notebook.Code)
			c.Source = dedent(trimBlank(body))
			c.SetMeta(kindKey, "setup")
			if args := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(l, "with app.setup")), ":"); args != "" {
				c.SetMeta(decoKey, strings.Trim(args, "()"))
			}
			nb.Cells = append(nb.Cells, c)
			i = end
		case decoRe.MatchString(l):
			d := decoRe.FindStringSubmatch(l)
			kind, args := d[1], d[2]
			i++
			if kind != "cell" {
				// a top-level def/class kept as is, minus the decorator
				start := i
				for i < len(lines) && !strings.HasPrefix(lines[i], "def ") && !strings.HasPrefix(lines[i], "async def ") && !strings.HasPrefix(lines[i], "class ") && !strings.HasPrefix(lines[i], "@") {
					i++
				}
				end, body := block(i + 1)
				c := notebook.NewCell(notebook.Code)
				c.Source = strings.Join(trimBlank(append(append([]string{}, lines[start:i+1]...), body...)), "\n")
				c.SetMeta(kindKey, kind)
				c.SetMeta(decoKey, args)
				nb.Cells = append(nb.Cells, c)
				i = end
				continue
			}
			// def name(params): possibly over several lines
			for i < len(lines) && !strings.HasSuffix(strings.TrimSpace(lines[i]), ":") {
				i++
			}
			end, body := block(i + 1)
			nb.Cells = append(nb.Cells, cellFromBody(body, args))
			i = end
		case strings.HasPrefix(l, "if __name__"):
			i = len(lines)
		default:
			i++
		}
	}
	if len(nb.Cells) == 0 {
		return nil, errors.New("no cells found")
	}
	return nb, nil
}

func cellFromBody(body []string, deco string) *notebook.Cell {
	body = trimBlank(body)
	// drop the trailing return, which may be a tuple spread over lines
	// with its closing paren back at the body's indent
	if len(body) > 0 {
		base := indentOf(body[0])
		for k := len(body) - 1; k >= 0; k-- {
			t := strings.TrimSpace(body[k])
			if t == "" || indentOf(body[k]) != base {
				continue
			}
			if retLine.MatchString(t) {
				body = body[:k]
			} else if strings.HasPrefix(t, ")") {
				continue
			}
			break
		}
	}
	src := dedent(trimBlank(body))
	if m := mdRe.FindStringSubmatch(src); m != nil && m[2] == m[4] {
		c := notebook.NewCell(notebook.Markdown)
		c.Source = strings.Trim(dedent(strings.Split(m[3], "\n")), "\n")
		c.SetMeta(decoKey, deco)
		return c
	}
	c := notebook.NewCell(notebook.Code)
	c.Source = src
	c.SetMeta(decoKey, deco)
	return c
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " \t")) }

func trimBlank(ls []string) []string {
	for len(ls) > 0 && strings.TrimSpace(ls[0]) == "" {
		ls = ls[1:]
	}
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	return ls
}

// dedent removes the common leading whitespace.
func dedent(ls []string) string {
	common := -1
	for _, l := range ls {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := indentOf(l); common < 0 || n < common {
			common = n
		}
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		if len(l) >= common && common > 0 {
			out[i] = l[common:]
		} else {
			out[i] = strings.TrimLeft(l, " \t")
		}
	}
	return strings.Join(out, "\n")
}

// ---- writing ----

type deps struct {
	Defs []string `json:"defs"`
	Refs []string `json:"refs"`
}

// analyzePy works out names defined and used per cell with ast, the same
// way the kernel-side analysis does, but runnable with any python (no
// IPython): magics are blanked first.
const analyzePy = `
import ast, builtins, json, sys
SKIP = set(dir(builtins))
class A(ast.NodeVisitor):
    def __init__(s): s.defs, s.loads, s.scopes = set(), set(), []
    def bind(s, n): (s.scopes[-1] if s.scopes else s.defs).add(n)
    def visit_Name(s, n):
        if isinstance(n.ctx, (ast.Store, ast.Del)): s.bind(n.id)
        elif not any(n.id in sc for sc in s.scopes): s.loads.add(n.id)
    def visit_Global(s, n): s.defs.update(n.names)
    def visit_Import(s, n):
        for a in n.names: s.bind((a.asname or a.name).split(".")[0])
    def visit_ImportFrom(s, n):
        for a in n.names:
            if a.name != "*": s.bind(a.asname or a.name)
    def func(s, n):
        if hasattr(n, "name"): s.bind(n.name)
        for d in getattr(n, "decorator_list", []): s.visit(d)
        a = n.args
        for d in a.defaults + [d for d in a.kw_defaults if d]: s.visit(d)
        loc = {x.arg for x in a.posonlyargs + a.args + a.kwonlyargs} | {x.arg for x in (a.vararg, a.kwarg) if x}
        s.scopes.append(loc)
        for st in (n.body if isinstance(n.body, list) else [n.body]): s.visit(st)
        s.scopes.pop()
    visit_FunctionDef = visit_AsyncFunctionDef = visit_Lambda = func
    def visit_ClassDef(s, n):
        s.bind(n.name)
        for e in n.bases + n.decorator_list + [k.value for k in n.keywords]: s.visit(e)
        s.scopes.append(set())
        for st in n.body: s.visit(st)
        s.scopes.pop()
    def comp(s, n):
        s.scopes.append(set())
        for g in n.generators:
            s.visit(g.iter); s.visit(g.target)
            for i in g.ifs: s.visit(i)
        for f in ("elt", "key", "value"):
            if hasattr(n, f): s.visit(getattr(n, f))
        s.scopes.pop()
    visit_ListComp = visit_SetComp = visit_GeneratorExp = visit_DictComp = comp
out = []
for src in json.load(sys.stdin):
    src = "\n".join("" if l.lstrip()[:1] in ("%", "!", "?") else l for l in src.split("\n"))
    try:
        t = ast.parse(src)
    except SyntaxError:
        out.append(None); continue
    a = A(); a.visit(t)
    out.append({"defs": sorted(a.defs), "refs": sorted(a.loads - a.defs - SKIP)})
print(json.dumps(out))
`

// Analyze runs analyzePy with uv's python, so it works without a kernel.
func Analyze(srcs []string) ([]*deps, error) {
	in, _ := json.Marshal(srcs)
	cmd := exec.Command("uv", "run", "--quiet", "--no-project", "python", "-c", analyzePy)
	cmd.Stdin = bytes.NewReader(in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("analysing cells: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	var res []*deps
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// Marshal writes the notebook in marimo's format. Cell signatures come
// from Analyze: a cell takes the names it uses that another cell
// defines, and returns the names it defines that another cell uses.
func Marshal(nb *notebook.Notebook) ([]byte, error) {
	cells := nb.Cells
	hasMd := false
	srcs := make([]string, len(cells))
	for i, c := range cells {
		if c.Type == notebook.Code {
			srcs[i] = c.Source
		} else if c.Type == notebook.Markdown {
			hasMd = true
		}
	}
	ds, err := Analyze(srcs)
	if err != nil {
		return nil, err
	}
	// markdown cells need mo; add the import cell marimo would have
	definesMo := false
	for _, d := range ds {
		if d != nil && contains(d.Defs, "mo") {
			definesMo = true
		}
	}
	definedBy := map[string][]int{}
	usedBy := map[string][]int{}
	for i, c := range cells {
		// markdown cells are mo.md(...) calls
		if c.Type == notebook.Markdown {
			usedBy["mo"] = append(usedBy["mo"], i)
		}
	}
	for i, d := range ds {
		if d == nil || cells[i].Type != notebook.Code {
			continue
		}
		// setup / @app.function / @app.class_definition names are module
		// globals in marimo, never passed in as parameters
		if cells[i].Meta(kindKey) != "" {
			continue
		}
		for _, n := range d.Defs {
			definedBy[n] = append(definedBy[n], i)
		}
		for _, n := range d.Refs {
			usedBy[n] = append(usedBy[n], i)
		}
	}
	others := func(idx []int, self int) bool {
		for _, i := range idx {
			if i != self {
				return true
			}
		}
		return false
	}

	var b strings.Builder
	b.WriteString("import marimo\n\n")
	ver := nb.Meta(verKey)
	if ver == "" {
		ver = "0.25.1"
	}
	fmt.Fprintf(&b, "__generated_with = %q\n", ver)
	fmt.Fprintf(&b, "app = marimo.App(%s)\n", nb.Meta(appKey))

	// setup cells go first, as marimo requires
	for _, c := range cells {
		if c.Meta(kindKey) == "setup" {
			args := ""
			if a := c.Meta(decoKey); a != "" {
				args = "(" + a + ")"
			}
			fmt.Fprintf(&b, "\nwith app.setup%s:\n%s\n", args, indent(orPass(c.Source)))
		}
	}
	if hasMd && !definesMo {
		b.WriteString("\n\n@app.cell\ndef _():\n    import marimo as mo\n    return (mo,)\n")
	}
	for i, c := range cells {
		kind := c.Meta(kindKey)
		deco := ""
		if a := c.Meta(decoKey); a != "" {
			deco = "(" + a + ")"
		}
		switch {
		case kind == "setup":
			continue
		case kind == "function" || kind == "class_definition":
			fmt.Fprintf(&b, "\n\n@app.%s%s\n%s\n", kind, deco, strings.TrimRight(c.Source, "\n"))
		case c.Type == notebook.Markdown:
			if deco == "" {
				deco = "(hide_code=True)"
			}
			q := `"""`
			if strings.Contains(c.Source, `"""`) {
				q = `'''`
			}
			fmt.Fprintf(&b, "\n\n@app.cell%s\ndef _(mo):\n    mo.md(r%s\n%s\n    %s)\n    return\n", deco, q, indent(c.Source), q)
		case c.Type == notebook.Code:
			var params, rets []string
			if d := ds[i]; d != nil {
				for _, n := range d.Refs {
					if others(definedBy[n], i) {
						params = append(params, n)
					}
				}
				for _, n := range d.Defs {
					if !strings.HasPrefix(n, "_") && others(usedBy[n], i) {
						rets = append(rets, n)
					}
				}
			}
			sort.Strings(params)
			sort.Strings(rets)
			ret := "return"
			switch len(rets) {
			case 0:
			case 1:
				ret += " (" + rets[0] + ",)"
			default:
				ret += " " + strings.Join(rets, ", ")
			}
			body := ""
			if strings.TrimSpace(c.Source) != "" {
				body = indent(commentMagics(c.Source)) + "\n"
			}
			fmt.Fprintf(&b, "\n\n@app.cell%s\ndef _(%s):\n%s    %s\n", deco, strings.Join(params, ", "), body, ret)
		}
	}
	b.WriteString("\n\nif __name__ == \"__main__\":\n    app.run()\n")
	return []byte(b.String()), nil
}

// Save writes a marimo file atomically.
func Save(nb *notebook.Notebook, path string) error {
	out, err := Marshal(nb)
	if err != nil {
		return err
	}
	tmp := path + ".jupytui.tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func indent(s string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		if l != "" {
			ls[i] = "    " + l
		}
	}
	return strings.Join(ls, "\n")
}

func orPass(s string) string {
	if strings.TrimSpace(s) == "" {
		return "pass"
	}
	return s
}

// commentMagics turns IPython-only lines into comments: marimo is plain
// python and would choke on them.
func commentMagics(s string) string {
	ls := strings.Split(s, "\n")
	for i, l := range ls {
		if t := strings.TrimLeft(l, " \t"); strings.HasPrefix(t, "%") || strings.HasPrefix(t, "!") {
			ls[i] = l[:len(l)-len(t)] + "# " + t
		}
	}
	return strings.Join(ls, "\n")
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
