package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/nkapila6/jupytui/internal/kernel"
)

// Variable explorer and dataframe viewer. Both ask the kernel through
// small python helpers run with kernel.Eval: no history, no [n] bump,
// and the helper runs in its own namespace (it only reads globals() via
// G), so nothing leaks into the user's variables.

const varsPy = `
import json, sys
SKIP = {"In", "Out", "exit", "quit", "get_ipython", "open"}
HIDE = {"module", "function", "builtin_function_or_method", "type", "method", "ABCMeta"}
out = []
for k, v in list(G.items()):
    if k.startswith("_") or k in SKIP:
        continue
    t = type(v)
    if t.__name__ in HIDE:
        continue
    mod = t.__module__.split(".")[0]
    tn = t.__name__
    kind = ""
    if (mod in ("pandas", "polars") and tn in ("DataFrame", "Series")) or (mod == "numpy" and tn == "ndarray" and getattr(v, "ndim", 0) in (1, 2)):
        kind = "frame"
    shape = ""
    try:
        if hasattr(v, "shape"):
            shape = " × ".join(map(str, v.shape))
        elif hasattr(v, "__len__") and not isinstance(v, (str, bytes)):
            shape = str(len(v))
    except Exception:
        pass
    size = 0
    try:
        if mod == "pandas":
            mu = v.memory_usage(deep=True)
            size = int(mu.sum()) if tn == "DataFrame" else int(mu)
        elif mod == "polars" and hasattr(v, "estimated_size"):
            size = int(v.estimated_size())
        elif hasattr(v, "nbytes"):
            size = int(v.nbytes)
        else:
            size = sys.getsizeof(v)
    except Exception:
        pass
    try:
        r = repr(v)
    except Exception as e:
        r = "<repr failed: %s>" % e
    out.append({"name": k, "type": tn, "module": mod, "shape": shape, "size": size, "kind": kind,
                "repr": " ".join(r[:400].split())[:200]})
print(json.dumps(out))
`

const reprPy = `
import json
v = G[P["name"]]
try:
    r = repr(v)
except Exception as e:
    r = "<repr failed: %s>" % e
print(json.dumps(r[:20000]))
`

// framePy returns one page of a table-like variable, sorted and
// filtered in the kernel so huge frames never cross the wire.
const framePy = `
import json
v = G[P["name"]]
t = type(v)
mod, tn = t.__module__.split(".")[0], t.__name__
o, n, q, sc, asc = P["offset"], P["limit"], (P["filter"] or "").lower(), P["sort"], P["asc"]

def s(x):
    try:
        r = str(x)
    except Exception:
        r = "?"
    return " ".join(r.split())[:200]

if mod == "numpy":
    a = v if v.ndim == 2 else v.reshape(-1, 1)
    cols = [str(i) for i in range(a.shape[1])]
    dtypes = [str(a.dtype)] * a.shape[1]
    idx = list(range(a.shape[0]))
    if q:
        idx = [i for i in idx if any(q in s(x).lower() for x in a[i])]
    if sc is not None:
        idx.sort(key=lambda i: a[i, sc], reverse=not asc)
    total = len(idx)
    rows = [[str(i)] + [s(x) for x in a[i]] for i in idx[o:o + n]]
    index = ""
elif mod == "pandas":
    df = v.to_frame() if tn == "Series" else v
    if q:
        mask = df.astype(str).apply(lambda c: c.str.lower().str.contains(q, regex=False)).any(axis=1)
        df = df[mask]
    if sc is not None:
        df = df.sort_values(df.columns[sc], ascending=asc, kind="stable", na_position="last")
    cols = [s(c) for c in df.columns]
    dtypes = [str(d) for d in df.dtypes]
    total = len(df)
    page = df.iloc[o:o + n]
    rows = [[s(i)] + [s(x) for x in r] for i, r in zip(page.index, page.itertuples(index=False))]
    index = s(df.index.name) if df.index.name is not None else ""
else:
    import polars as pl
    df = v.to_frame() if tn == "Series" else v
    names = df.columns
    df = df.with_row_index("__jt_row") if hasattr(df, "with_row_index") else df.with_row_count("__jt_row")
    if q:
        df = df.filter(pl.any_horizontal([pl.col(c).cast(pl.Utf8).str.to_lowercase().str.contains(q, literal=True) for c in names]))
    if sc is not None:
        df = df.sort(names[sc], descending=not asc, nulls_last=True)
    cols = list(names)
    dtypes = [str(d) for d in df.dtypes[1:]]
    total = df.height
    rows = [[s(r[0])] + [s(x) for x in r[1:]] for r in df.slice(o, n).rows()]
    index = ""
print(json.dumps({"type": tn, "cols": cols, "dtypes": dtypes, "total": total, "rows": rows, "index": index}))
`

// pyCall wraps a helper so it runs with its own globals: G is the
// user's namespace, P the parameters.
func pyCall(src string, params any) string {
	s, _ := json.Marshal(src)
	p, _ := json.Marshal(params)
	ps, _ := json.Marshal(string(p))
	return fmt.Sprintf(`exec(compile(%s, "<jupytui>", "exec"), {"G": globals(), "P": __import__("json").loads(%s)})`, s, ps)
}

type varInfo struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Module string `json:"module"`
	Shape  string `json:"shape"`
	Size   int64  `json:"size"`
	Kind   string `json:"kind"`
	Repr   string `json:"repr"`
}

type varsPanel struct {
	list    []varInfo
	sel     int
	loading bool
	err     string
	detail  string // full repr of a non-table variable
}

type varsMsg struct {
	list []varInfo
	err  error
}

type reprMsg struct {
	text string
	err  error
}

func (m *Model) openVars() tea.Cmd {
	if m.k == nil {
		m.msg = "kernel isn't running"
		return nil
	}
	if m.vars == nil {
		m.vars = &varsPanel{}
	}
	m.vars.loading = true
	return m.fetchVars()
}

func (m *Model) fetchVars() tea.Cmd {
	k := m.k
	if k == nil {
		return nil
	}
	return func() tea.Msg {
		out, err := k.Eval(pyCall(varsPy, map[string]any{}), 30*time.Second)
		if err != nil {
			return varsMsg{err: err}
		}
		var list []varInfo
		if err := json.Unmarshal([]byte(lastLine(out)), &list); err != nil {
			return varsMsg{err: err}
		}
		return varsMsg{list: list}
	}
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (m *Model) handleVars(msg varsMsg) {
	v := m.vars
	if v == nil {
		return
	}
	v.loading = false
	if msg.err != nil {
		v.err = msg.err.Error()
		return
	}
	v.err = ""
	prev := ""
	if v.sel < len(v.list) {
		prev = v.list[v.sel].Name
	}
	v.list = msg.list
	v.sel = 0
	for i, it := range v.list {
		if it.Name == prev {
			v.sel = i
		}
	}
}

func (m *Model) varsKey(msg tea.KeyPressMsg) tea.Cmd {
	v := m.vars
	if v.detail != "" {
		v.detail = ""
		return nil
	}
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.vars = nil
	case "j", "down":
		v.sel = min(v.sel+1, max(len(v.list)-1, 0))
	case "k", "up":
		v.sel = max(v.sel-1, 0)
	case "g", "home":
		v.sel = 0
	case "G", "end":
		v.sel = max(len(v.list)-1, 0)
	case "r":
		v.loading = true
		return m.fetchVars()
	case "enter":
		if v.sel >= len(v.list) {
			return nil
		}
		it := v.list[v.sel]
		if it.Kind == "frame" {
			return m.openFrame(it.Name)
		}
		k := m.k
		if k == nil {
			return nil
		}
		v.detail = "loading..."
		return func() tea.Msg {
			out, err := k.Eval(pyCall(reprPy, map[string]any{"name": it.Name}), 30*time.Second)
			var text string
			if err == nil {
				err = json.Unmarshal([]byte(lastLine(out)), &text)
			}
			return reprMsg{text, err}
		}
	}
	return nil
}

// refreshVarsAfterRun keeps an open panel current once cells finish.
func (m *Model) refreshVarsAfterRun(ev kernel.Event) tea.Cmd {
	if m.vars == nil || ev.Kind != kernel.EvDone || len(m.runs) > 0 {
		return nil
	}
	return m.fetchVars()
}

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
}
