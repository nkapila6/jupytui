package nbdiff

import (
	"fmt"
	"io"
	"strings"

	"github.com/nkapila6/jupytui/internal/notebook"
)

const (
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	cyan   = "\x1b[36m"
	dim    = "\x1b[2m"
	bold   = "\x1b[1m"
	reset  = "\x1b[0m"
	contxt = 2 // unchanged lines shown around a change
)

// Render writes a cell-level diff. outputs adds output changes in full
// instead of a one-line note.
func Render(w io.Writer, a, b *notebook.Notebook, color, outputs bool) (changed bool) {
	c := func(code, s string) string {
		if !color {
			return s
		}
		return code + s + reset
	}
	unchanged := 0
	flushSame := func() {
		if unchanged > 0 {
			fmt.Fprintln(w, c(dim, fmt.Sprintf("  … %d unchanged cell%s", unchanged, plural(unchanged))))
			unchanged = 0
		}
	}
	bi := 0
	for _, op := range Align(a.Cells, b.Cells) {
		if op.B != nil {
			bi++
		}
		outChanged := op.A != nil && op.B != nil && outputsDiffer(op.A, op.B)
		switch op.Kind {
		case '=':
			if !outChanged {
				unchanged++
				continue
			}
		}
		flushSame()
		changed = true
		cell := op.B
		if cell == nil {
			cell = op.A
		}
		head := fmt.Sprintf("cell %d (%s)", bi, cell.Type)
		switch op.Kind {
		case '+':
			fmt.Fprintln(w, c(bold+green, head+" added"))
			for _, l := range strings.Split(op.B.Source, "\n") {
				fmt.Fprintln(w, c(green, "  +"+l))
			}
		case '-':
			fmt.Fprintln(w, c(bold+red, fmt.Sprintf("cell (%s) removed", op.A.Type)))
			for _, l := range strings.Split(op.A.Source, "\n") {
				fmt.Fprintln(w, c(red, "  -"+l))
			}
		case '~', '=':
			what := "changed"
			if op.Kind == '=' {
				what = "outputs changed"
			} else if op.A.Type != op.B.Type {
				what = fmt.Sprintf("changed from %s", op.A.Type)
			}
			fmt.Fprintln(w, c(bold+cyan, head+" "+what))
			if op.Kind == '~' {
				writeHunks(w, LineDiff(op.A.Source, op.B.Source), c)
			}
			if outChanged && op.Kind == '~' && !outputs {
				fmt.Fprintln(w, c(dim, "  outputs changed"))
			}
			if outChanged && outputs {
				writeHunks(w, LineDiff(strings.Join(OutputSummary(op.A), "\n"), strings.Join(OutputSummary(op.B), "\n")), c)
			}
		}
	}
	flushSame()
	if !changed {
		fmt.Fprintln(w, "no changes")
	}
	return changed
}

// writeHunks prints changed lines with a little context, eliding the rest.
func writeHunks(w io.Writer, lines []string, c func(string, string) string) {
	show := make([]bool, len(lines))
	for i, l := range lines {
		if l[0] != ' ' {
			for k := max(0, i-contxt); k <= min(len(lines)-1, i+contxt); k++ {
				show[k] = true
			}
		}
	}
	skipped := false
	for i, l := range lines {
		if !show[i] {
			skipped = true
			continue
		}
		if skipped {
			fmt.Fprintln(w, c(dim, "  …"))
			skipped = false
		}
		switch l[0] {
		case '-':
			fmt.Fprintln(w, c(red, "  "+l))
		case '+':
			fmt.Fprintln(w, c(green, "  "+l))
		default:
			fmt.Fprintln(w, c(dim, "  "+l))
		}
	}
}

func outputsDiffer(a, b *notebook.Cell) bool {
	return strings.Join(OutputSummary(a), "\n") != strings.Join(OutputSummary(b), "\n")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
