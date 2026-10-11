package ai

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is one server-sent event.
type sseEvent struct {
	Event string
	Data  string
}

// readSSE calls fn for each event until the body ends or fn returns
// false. Comment lines (": OPENROUTER PROCESSING") are skipped.
func readSSE(r io.Reader, fn func(sseEvent) bool) error {
	sc := bufio.NewScanner(r)
	// a single data line can carry a whole tool input
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var ev sseEvent
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 || ev.Event != "" {
				ev.Data = strings.Join(data, "\n")
				if !fn(ev) {
					return nil
				}
			}
			ev, data = sseEvent{}, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			ev.Event = value
		case "data":
			data = append(data, value)
		}
	}
	if len(data) > 0 {
		ev.Data = strings.Join(data, "\n")
		fn(ev)
	}
	return sc.Err()
}
