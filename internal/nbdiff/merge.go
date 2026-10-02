package nbdiff

import (
	"github.com/nkapila6/jupytui/internal/notebook"
)

// Merge does a three-way merge of notebooks cell by cell. Cells are
// matched by id (or content when they have none). A cell changed on only
// one side takes that side; changed on both differently is a conflict,
// written into the cell with git-style markers. Metadata comes from ours.
func Merge(base, ours, theirs *notebook.Notebook) (*notebook.Notebook, int) {
	baseBy := index(base)
	oursBy := index(ours)
	theirsBy := index(theirs)
	conflicts := 0

	// ours' order, with cells only theirs added slotted in after the
	// nearest earlier cell they share with us
	order := make([]*notebook.Cell, 0, len(ours.Cells))
	order = append(order, ours.Cells...)
	for i, t := range theirs.Cells {
		k := key(t)
		if _, ok := oursBy[k]; ok {
			continue
		}
		if _, ok := baseBy[k]; ok {
			continue // ours deleted it; handled below
		}
		at := 0
		for p := i - 1; p >= 0; p-- {
			if idx := indexOf(order, oursBy[key(theirs.Cells[p])]); idx >= 0 {
				at = idx + 1
				break
			}
		}
		order = append(order[:at], append([]*notebook.Cell{t}, order[at:]...)...)
	}

	var cells []*notebook.Cell
	for _, c := range order {
		k := key(c)
		b, inBase := baseBy[k]
		o, inOurs := oursBy[k]
		t, inTheirs := theirsBy[k]
		switch {
		case inOurs && inTheirs:
			switch {
			case sameContent(o, t), inBase && sameContent(o, b) && sameContent(t, b):
				cells = append(cells, o)
			case inBase && sameContent(o, b):
				cells = append(cells, t)
			case inBase && sameContent(t, b):
				cells = append(cells, o)
			default:
				conflicts++
				cells = append(cells, conflictCell(o, o.Source, t.Source))
			}
		case inOurs:
			// theirs deleted it: fine unless we changed it
			if inBase && !sameContent(o, b) {
				conflicts++
				cells = append(cells, conflictCell(o, o.Source, "# (deleted on their side)"))
			} else if !inBase {
				cells = append(cells, o)
			}
		case inTheirs:
			cells = append(cells, t)
		}
	}
	// cells ours deleted that theirs changed are conflicts too
	for _, t := range theirs.Cells {
		k := key(t)
		b, inBase := baseBy[k]
		if _, inOurs := oursBy[k]; inOurs || !inBase || sameContent(t, b) {
			continue
		}
		conflicts++
		cells = append(cells, conflictCell(t, "# (deleted on our side)", t.Source))
	}

	out := ours
	out.Cells = cells
	return out, conflicts
}

func conflictCell(like *notebook.Cell, ours, theirs string) *notebook.Cell {
	c := like.Clone()
	c.ID = like.ID
	c.Source = "<<<<<<< ours\n" + ours + "\n=======\n" + theirs + "\n>>>>>>> theirs"
	c.ClearOutputs()
	return c
}

func index(nb *notebook.Notebook) map[string]*notebook.Cell {
	m := map[string]*notebook.Cell{}
	for _, c := range nb.Cells {
		m[key(c)] = c
	}
	return m
}

func indexOf(cells []*notebook.Cell, c *notebook.Cell) int {
	if c == nil {
		return -1
	}
	for i, x := range cells {
		if x == c {
			return i
		}
	}
	return -1
}
