package kernel

import "encoding/json"

// Comm is one comm_open / comm_msg / comm_close from the kernel. This is
// how ipywidgets keeps a widget's state in sync with the frontend.
type Comm struct {
	Type    string // comm_open, comm_msg or comm_close
	ID      string
	Target  string // comm_open only, jupyter.widget for widgets
	Data    json.RawMessage
	Meta    json.RawMessage
	Buffers [][]byte
}

func isComm(t string) bool {
	return t == "comm_open" || t == "comm_msg" || t == "comm_close"
}

func newComm(m *Message) *Comm {
	var c struct {
		ID     string          `json:"comm_id"`
		Target string          `json:"target_name"`
		Data   json.RawMessage `json:"data"`
	}
	json.Unmarshal(m.Content, &c)
	return &Comm{Type: m.Header.MsgType, ID: c.ID, Target: c.Target, Data: c.Data, Meta: m.Metadata, Buffers: m.Buffers}
}

// Comms delivers comm traffic that isn't part of a request we follow,
// like a widget updated from a background thread.
func (k *Kernel) Comms() <-chan *Comm { return k.comms }

// CommSend sends a comm_msg to the kernel. Whatever the handler prints
// or displays, and any widget updates it causes, come back on the
// returned channel, which closes once the kernel goes idle.
func (k *Kernel) CommSend(commID string, data any) (string, <-chan Event, error) {
	m, err := k.newMessage("comm_msg", map[string]any{"comm_id": commID, "data": data})
	if err != nil {
		return "", nil, err
	}
	id := m.Header.MsgID
	iopub := k.watch(id)
	frames, err := m.encode(k.key)
	if err == nil {
		err = k.shell.send(frames)
	}
	if err != nil {
		k.unwatch(id)
		return "", nil, err
	}
	return id, k.follow(id, iopub, nil, nil), nil
}
