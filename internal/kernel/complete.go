package kernel

import (
	"encoding/json"
	"fmt"
	"time"
)

// Completion is one suggestion from the kernel.
type Completion struct {
	Text string
	Type string // function, module, instance, class, keyword, ... when known
}

// Completions is a complete_reply: replace runes [Start, End) of the
// code with one of Items. Offsets are in unicode code points.
type Completions struct {
	Items      []Completion
	Start, End int
}

// Complete asks the kernel what could go at cursor (a rune offset into
// code). Requests queue behind running cells on the shell channel, so
// a busy kernel just times out and the caller shows nothing.
func (k *Kernel) Complete(code string, cursor int, timeout time.Duration) (Completions, error) {
	m, err := k.newMessage("complete_request", map[string]any{"code": code, "cursor_pos": cursor})
	if err != nil {
		return Completions{}, err
	}
	r, err := k.request(k.shell, m, timeout)
	if err != nil {
		return Completions{}, err
	}
	var c struct {
		Status      string   `json:"status"`
		Matches     []string `json:"matches"`
		CursorStart int      `json:"cursor_start"`
		CursorEnd   int      `json:"cursor_end"`
		Metadata    struct {
			// ipykernel's typed matches, when jedi is on
			Types []struct {
				Text string `json:"text"`
				Type string `json:"type"`
			} `json:"_jupyter_types_experimental"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(r.Content, &c); err != nil {
		return Completions{}, err
	}
	if c.Status != "ok" {
		return Completions{}, fmt.Errorf("complete: %s", c.Status)
	}
	types := map[string]string{}
	for _, t := range c.Metadata.Types {
		types[t.Text] = t.Type
	}
	out := Completions{Start: c.CursorStart, End: c.CursorEnd}
	for _, s := range c.Matches {
		t := types[s]
		if t == "<unknown>" {
			t = ""
		}
		out.Items = append(out.Items, Completion{Text: s, Type: t})
	}
	return out, nil
}
