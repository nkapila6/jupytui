// Package nbdiff diffs and merges notebooks cell by cell, and turns them
// into text git can diff.
package nbdiff

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/nkapila6/jupytui/internal/notebook"
)

// Op is one step of a cell alignment.
type Op struct {
	Kind byte // '=' same, '~' changed, '+' added, '-' removed
	A, B *notebook.Cell
}

// key identifies a cell across versions: its id when it has one,
// otherwise its type and source.
func key(c *notebook.Cell) string {
	if c.ID != "" {
		return "id:" + c.ID
	}
	return "src:" + string(c.Type) + "\x00" + c.Source
}

func sameContent(a, b *notebook.Cell) bool {
	return a.Type == b.Type && a.Source == b.Source
}

// Align matches cells of a and b: an LCS on cell keys, then unmatched
// cells in the same gap are paired up as changed when they look alike.
func Align(a, b []*notebook.Cell) []Op {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if key(a[i]) == key(b[j]) {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var ops []Op
	var gapA, gapB []*notebook.Cell
	flush := func() {
		// pair leftovers in order when they're similar enough
		i, j := 0, 0
		for i < len(gapA) && j < len(gapB) {
			if gapA[i].Type == gapB[j].Type && similarity(gapA[i].Source, gapB[j].Source) >= 0.4 {
				ops = append(ops, Op{Kind: '~', A: gapA[i], B: gapB[j]})
				i++
				j++
			} else {
				ops = append(ops, Op{Kind: '-', A: gapA[i]})
				i++
			}
		}
		for ; i < len(gapA); i++ {
			ops = append(ops, Op{Kind: '-', A: gapA[i]})
		}
		for ; j < len(gapB); j++ {
			ops = append(ops, Op{Kind: '+', B: gapB[j]})
		}
		gapA, gapB = nil, nil
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case key(a[i]) == key(b[j]):
			flush()
			kind := byte('=')
			if !sameContent(a[i], b[j]) {
				kind = '~'
			}
			ops = append(ops, Op{Kind: kind, A: a[i], B: b[j]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			gapA = append(gapA, a[i])
			i++
		default:
			gapB = append(gapB, b[j])
			j++
		}
	}
	gapA = append(gapA, a[i:]...)
	gapB = append(gapB, b[j:]...)
	flush()
	return ops
}

// similarity is the share of lines two texts have in common.
func similarity(a, b string) float64 {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	common := len(lineLCS(la, lb))
	return float64(2*common) / float64(len(la)+len(lb))
}

// lineLCS returns the indexes pairs of a longest common subsequence.
func lineLCS(a, b []string) [][2]int {
	n, m := len(a), len(b)
	t := make([][]int, n+1)
	for i := range t {
		t[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				t[i][j] = t[i+1][j+1] + 1
			} else {
				t[i][j] = max(t[i+1][j], t[i][j+1])
			}
		}
	}
	var out [][2]int
	for i, j := 0, 0; i < n && j < m; {
		switch {
		case a[i] == b[j]:
			out = append(out, [2]int{i, j})
			i++
			j++
		case t[i+1][j] >= t[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}

// LineDiff is a unified-style diff of two texts: lines prefixed with
// ' ', '-' or '+'.
func LineDiff(a, b string) []string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	i, j := 0, 0
	for _, p := range lineLCS(la, lb) {
		for ; i < p[0]; i++ {
			out = append(out, "-"+la[i])
		}
		for ; j < p[1]; j++ {
			out = append(out, "+"+lb[j])
		}
		out = append(out, " "+la[i])
		i++
		j++
	}
	for ; i < len(la); i++ {
		out = append(out, "-"+la[i])
	}
	for ; j < len(lb); j++ {
		out = append(out, "+"+lb[j])
	}
	return out
}

// OutputSummary describes a cell's outputs in a few short lines, for
// diffs: text outputs as text, images as type, size and a hash.
func OutputSummary(c *notebook.Cell) []string {
	var out []string
	for _, o := range c.Outputs {
		switch o.OutputType {
		case "stream":
			for _, l := range strings.Split(strings.TrimRight(notebook.CollapseCR(o.Text), "\n"), "\n") {
				out = append(out, o.Name+": "+l)
			}
		case "error":
			out = append(out, "error: "+o.Ename+": "+o.Evalue)
		default:
			if s, ok := o.DataText("text/plain"); ok {
				for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
					out = append(out, "result: "+l)
				}
			}
			for _, mt := range []string{"image/png", "image/jpeg", "image/svg+xml", "text/html"} {
				if raw, ok := o.Data[mt]; ok {
					h := sha1.Sum(raw)
					out = append(out, fmt.Sprintf("%s (%d bytes, %s)", mt, len(raw), hex.EncodeToString(h[:4])))
				}
			}
		}
	}
	return out
}

// Text is the notebook as git-diffable text: percent format, with each
// code cell's outputs summarised as comments below it.
func Text(nb *notebook.Notebook) string {
	var b strings.Builder
	for i, c := range nb.Cells {
		if i > 0 {
			b.WriteString("\n")
		}
		one := notebook.New()
		one.Cells = []*notebook.Cell{c}
		b.WriteString(one.Percent())
		if c.Type == notebook.Code {
			if c.ExecutionCount != nil {
				fmt.Fprintf(&b, "# [%d]\n", *c.ExecutionCount)
			}
			for _, l := range OutputSummary(c) {
				b.WriteString("#> " + l + "\n")
			}
		}
	}
	return b.String()
}
