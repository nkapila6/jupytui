package notebook

import "strings"

// AddOutput appends an output the way Jupyter does: consecutive chunks
// of the same stream merge into one, and text overwritten by a carriage
// return is dropped so progress bars keep only their latest frame.
func (c *Cell) AddOutput(o *Output) {
	if o.OutputType == "stream" && len(c.Outputs) > 0 {
		last := c.Outputs[len(c.Outputs)-1]
		if last.OutputType == "stream" && last.Name == o.Name {
			last.Text = CollapseCR(last.Text + o.Text)
			return
		}
	}
	if o.OutputType == "stream" {
		o.Text = CollapseCR(o.Text)
	}
	c.Outputs = append(c.Outputs, o)
}

// CollapseCR drops text a carriage return has overwritten. A trailing
// \r is kept since the next chunk is meant to overwrite the line.
func CollapseCR(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		trail := strings.HasSuffix(l, "\r")
		l = strings.TrimSuffix(l, "\r")
		if j := strings.LastIndex(l, "\r"); j >= 0 {
			l = l[j+1:]
		}
		if trail {
			l += "\r"
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// EnsureID gives a cell an id if it doesn't have one (pre-4.5 notebooks).
func (c *Cell) EnsureID() string {
	if c.ID == "" {
		c.ID = newID()
	}
	return c.ID
}

// CellByID finds a cell by its id.
func (nb *Notebook) CellByID(id string) *Cell {
	for _, c := range nb.Cells {
		if c.ID == id {
			return c
		}
	}
	return nil
}
