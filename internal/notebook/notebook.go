// Package notebook reads and writes Jupyter .ipynb files (nbformat v4).
//
// Every object keeps its original JSON fields in a raw map so keys we
// don't model survive a load/save round trip untouched.
package notebook

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type CellType string

const (
	Code     CellType = "code"
	Markdown CellType = "markdown"
	Raw      CellType = "raw"
)

type Notebook struct {
	Cells []*Cell
	raw   map[string]json.RawMessage
}

type Cell struct {
	ID             string
	Type           CellType
	Source         string
	ExecutionCount *int
	Outputs        []*Output
	raw            map[string]json.RawMessage
}

// Output is one entry in a code cell's outputs list.
type Output struct {
	OutputType     string // stream, display_data, execute_result, error
	Name           string // stdout or stderr, for streams
	Text           string
	Data           map[string]json.RawMessage // mime type -> value
	ExecutionCount *int
	Ename          string
	Evalue         string
	Traceback      []string
	raw            map[string]json.RawMessage
}

func New() *Notebook {
	nb := &Notebook{raw: map[string]json.RawMessage{}}
	nb.raw["metadata"] = json.RawMessage(`{}`)
	nb.raw["nbformat"] = json.RawMessage(`4`)
	nb.raw["nbformat_minor"] = json.RawMessage(`5`)
	return nb
}

func Load(path string) (*Notebook, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Notebook, error) {
	nb := &Notebook{}
	if err := json.Unmarshal(b, &nb.raw); err != nil {
		return nil, fmt.Errorf("parse notebook: %w", err)
	}
	var major int
	if err := json.Unmarshal(nb.raw["nbformat"], &major); err != nil || major != 4 {
		return nil, fmt.Errorf("unsupported nbformat %s, only v4 is supported", nb.raw["nbformat"])
	}
	var rawCells []map[string]json.RawMessage
	if err := json.Unmarshal(nb.raw["cells"], &rawCells); err != nil {
		return nil, fmt.Errorf("parse cells: %w", err)
	}
	for i, rc := range rawCells {
		c, err := parseCell(rc)
		if err != nil {
			return nil, fmt.Errorf("cell %d: %w", i, err)
		}
		nb.Cells = append(nb.Cells, c)
	}
	return nb, nil
}

func parseCell(raw map[string]json.RawMessage) (*Cell, error) {
	c := &Cell{raw: raw}
	if err := decodeOpt(raw, "cell_type", &c.Type); err != nil {
		return nil, err
	}
	if err := decodeOpt(raw, "id", &c.ID); err != nil {
		return nil, err
	}
	if err := decodeOpt(raw, "execution_count", &c.ExecutionCount); err != nil {
		return nil, err
	}
	src, err := multiline(raw["source"])
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	c.Source = src
	if ro, ok := raw["outputs"]; ok {
		var outs []map[string]json.RawMessage
		if err := json.Unmarshal(ro, &outs); err != nil {
			return nil, fmt.Errorf("outputs: %w", err)
		}
		for _, o := range outs {
			out, err := parseOutput(o)
			if err != nil {
				return nil, err
			}
			c.Outputs = append(c.Outputs, out)
		}
	}
	return c, nil
}

func parseOutput(raw map[string]json.RawMessage) (*Output, error) {
	o := &Output{raw: raw}
	for _, f := range []struct {
		key string
		dst any
	}{
		{"output_type", &o.OutputType},
		{"name", &o.Name},
		{"execution_count", &o.ExecutionCount},
		{"ename", &o.Ename},
		{"evalue", &o.Evalue},
		{"traceback", &o.Traceback},
		{"data", &o.Data},
	} {
		if err := decodeOpt(raw, f.key, f.dst); err != nil {
			return nil, fmt.Errorf("output %s: %w", f.key, err)
		}
	}
	text, err := multiline(raw["text"])
	if err != nil {
		return nil, fmt.Errorf("output text: %w", err)
	}
	o.Text = text
	return o, nil
}

// Save writes atomically so a crash mid-write can't eat the notebook.
func (nb *Notebook) Save(path string) error {
	b, err := nb.Marshal()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".jupytui-*.ipynb")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil {
		os.Chmod(tmp.Name(), fi.Mode())
	}
	return os.Rename(tmp.Name(), path)
}

// Marshal matches nbformat's writer: sorted keys, 1-space indent,
// no HTML escaping, trailing newline.
func (nb *Notebook) Marshal() ([]byte, error) {
	cells := make([]map[string]json.RawMessage, len(nb.Cells))
	for i, c := range nb.Cells {
		rc, err := c.toRaw()
		if err != nil {
			return nil, err
		}
		cells[i] = rc
	}
	out := copyRaw(nb.raw)
	cb, err := encode(cells)
	if err != nil {
		return nil, err
	}
	out["cells"] = cb

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *Cell) toRaw() (map[string]json.RawMessage, error) {
	r := copyRaw(c.raw)
	set := func(k string, v any) error {
		b, err := encode(v)
		r[k] = b
		return err
	}
	if err := set("cell_type", c.Type); err != nil {
		return nil, err
	}
	if c.ID != "" {
		if err := set("id", c.ID); err != nil {
			return nil, err
		}
	}
	if _, ok := r["metadata"]; !ok {
		r["metadata"] = json.RawMessage(`{}`)
	}
	if err := set("source", splitLines(c.Source)); err != nil {
		return nil, err
	}
	if c.Type == Code {
		if err := set("execution_count", c.ExecutionCount); err != nil {
			return nil, err
		}
		outs := make([]map[string]json.RawMessage, 0, len(c.Outputs))
		for _, o := range c.Outputs {
			ro, err := o.toRaw()
			if err != nil {
				return nil, err
			}
			outs = append(outs, ro)
		}
		if err := set("outputs", outs); err != nil {
			return nil, err
		}
	} else {
		delete(r, "execution_count")
		delete(r, "outputs")
	}
	return r, nil
}

func (o *Output) toRaw() (map[string]json.RawMessage, error) {
	r := copyRaw(o.raw)
	var err error
	set := func(k string, v any) {
		if err != nil {
			return
		}
		r[k], err = encode(v)
	}
	set("output_type", o.OutputType)
	switch o.OutputType {
	case "stream":
		set("name", o.Name)
		set("text", splitLines(o.Text))
	case "error":
		set("ename", o.Ename)
		set("evalue", o.Evalue)
		set("traceback", nonNil(o.Traceback))
	case "execute_result", "display_data":
		set("data", splitMimeBundle(o.Data))
		if _, ok := r["metadata"]; !ok {
			r["metadata"] = json.RawMessage(`{}`)
		}
		if o.OutputType == "execute_result" {
			set("execution_count", o.ExecutionCount)
		}
	}
	return r, err
}

// DataText returns a mime bundle entry as a string. nbformat stores
// text types as either a string or a list of lines.
func (o *Output) DataText(mime string) (string, bool) {
	v, ok := o.Data[mime]
	if !ok {
		return "", false
	}
	s, err := multiline(v)
	if err != nil {
		// non-string payload like application/json
		return string(v), true
	}
	return s, true
}

// NewOutput builds an output from an iopub message's type and content.
func NewOutput(msgType string, content json.RawMessage) (*Output, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(content, &raw); err != nil {
		return nil, err
	}
	// transient (display_id etc) is runtime-only and never saved
	delete(raw, "transient")
	raw["output_type"], _ = encode(msgType)
	return parseOutput(raw)
}

// splitMimeBundle mirrors nbformat's _split_mimebundle.
func splitMimeBundle(data map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(data))
	for k, v := range data {
		out[k] = v
		if !strings.HasPrefix(k, "text/") && k != "image/svg+xml" && k != "application/javascript" {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			if b, err := encode(splitLines(s)); err == nil {
				out[k] = b
			}
		}
	}
	return out
}

func NewCell(t CellType) *Cell {
	c := &Cell{ID: newID(), Type: t, raw: map[string]json.RawMessage{}}
	c.raw["metadata"] = json.RawMessage(`{}`)
	return c
}

func (nb *Notebook) Insert(i int, c *Cell) {
	i = max(0, min(i, len(nb.Cells)))
	nb.Cells = append(nb.Cells[:i], append([]*Cell{c}, nb.Cells[i:]...)...)
}

func (nb *Notebook) Delete(i int) *Cell {
	if i < 0 || i >= len(nb.Cells) {
		return nil
	}
	c := nb.Cells[i]
	nb.Cells = append(nb.Cells[:i], nb.Cells[i+1:]...)
	return c
}

// Move swaps cell i with its neighbour at i+delta.
func (nb *Notebook) Move(i, delta int) bool {
	j := i + delta
	if i < 0 || j < 0 || i >= len(nb.Cells) || j >= len(nb.Cells) {
		return false
	}
	nb.Cells[i], nb.Cells[j] = nb.Cells[j], nb.Cells[i]
	return true
}

func (c *Cell) SetType(t CellType) {
	if c.Type == t {
		return
	}
	c.Type = t
	if t != Code {
		c.Outputs = nil
		c.ExecutionCount = nil
	}
}

func (c *Cell) ClearOutputs() {
	c.Outputs = nil
	c.ExecutionCount = nil
}

// Clone makes an independent copy with a fresh id, for yank/paste.
func (c *Cell) Clone() *Cell {
	cp := *c
	cp.ID = newID()
	cp.raw = copyRaw(c.raw)
	cp.Outputs = append([]*Output(nil), c.Outputs...)
	return &cp
}

// KernelName reads metadata.kernelspec.name, defaulting to python3.
func (nb *Notebook) KernelName() string {
	var md struct {
		Kernelspec struct {
			Name string `json:"name"`
		} `json:"kernelspec"`
	}
	json.Unmarshal(nb.raw["metadata"], &md)
	if md.Kernelspec.Name == "" {
		return "python3"
	}
	return md.Kernelspec.Name
}

func multiline(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var lines []string
	if err := json.Unmarshal(raw, &lines); err != nil {
		return "", err
	}
	return strings.Join(lines, ""), nil
}

// splitLines is Python's str.splitlines(True) for \n, \r and \r\n,
// which is what nbformat uses. Progress bars lean on bare \r.
func splitLines(s string) []string {
	lines := []string{}
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			lines = append(lines, s[start:i+1])
			start = i + 1
		case '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func decodeOpt(raw map[string]json.RawMessage, key string, dst any) error {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	return json.Unmarshal(v, dst)
}

func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func copyRaw(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
