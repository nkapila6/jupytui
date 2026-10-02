package kernel

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-zeromq/zmq4"
)

const protocolVersion = "5.3"

var delimiter = []byte("<IDS|MSG>")

type Header struct {
	MsgID    string `json:"msg_id"`
	Session  string `json:"session"`
	Username string `json:"username"`
	Date     string `json:"date"`
	MsgType  string `json:"msg_type"`
	Version  string `json:"version"`
}

// Message is one Jupyter wire protocol message.
type Message struct {
	Identities   [][]byte
	Header       Header
	ParentHeader Header
	Metadata     json.RawMessage
	Content      json.RawMessage
	Buffers      [][]byte
}

func (k *Kernel) newMessage(msgType string, content any) (*Message, error) {
	c, err := json.Marshal(content)
	if err != nil {
		return nil, err
	}
	return &Message{
		Header: Header{
			MsgID:    newUUID(),
			Session:  k.session,
			Username: username(),
			Date:     time.Now().UTC().Format(time.RFC3339Nano),
			MsgType:  msgType,
			Version:  protocolVersion,
		},
		Metadata: json.RawMessage(`{}`),
		Content:  c,
	}, nil
}

// encode builds the multipart frames:
// identities, <IDS|MSG>, hmac, header, parent_header, metadata, content, buffers.
func (m *Message) encode(key []byte) (zmq4.Msg, error) {
	header, err := json.Marshal(m.Header)
	if err != nil {
		return zmq4.Msg{}, err
	}
	// an empty parent header has to go out as {}, not a struct of blank strings
	parent := []byte(`{}`)
	if m.ParentHeader.MsgID != "" {
		if parent, err = json.Marshal(m.ParentHeader); err != nil {
			return zmq4.Msg{}, err
		}
	}
	parts := [][]byte{header, parent, orEmpty(m.Metadata), orEmpty(m.Content)}

	frames := append([][]byte{}, m.Identities...)
	frames = append(frames, delimiter, []byte(sign(key, parts)))
	frames = append(frames, parts...)
	frames = append(frames, m.Buffers...)
	return zmq4.NewMsgFrom(frames...), nil
}

func decode(key []byte, msg zmq4.Msg) (*Message, error) {
	frames := msg.Frames
	i := 0
	for i < len(frames) && !bytes.Equal(frames[i], delimiter) {
		i++
	}
	if len(frames)-i < 6 {
		return nil, errors.New("malformed message: missing frames")
	}
	sig := string(frames[i+1])
	parts := frames[i+2 : i+6]
	if len(key) > 0 && !hmac.Equal([]byte(sig), []byte(sign(key, parts))) {
		return nil, errors.New("bad message signature")
	}
	m := &Message{
		Identities: frames[:i],
		Metadata:   parts[2],
		Content:    parts[3],
		Buffers:    frames[i+6:],
	}
	if err := json.Unmarshal(parts[0], &m.Header); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	// parent header may be {} which leaves it zero, that's fine
	json.Unmarshal(parts[1], &m.ParentHeader)
	return m, nil
}

func sign(key []byte, parts [][]byte) string {
	if len(key) == 0 {
		return ""
	}
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func orEmpty(b json.RawMessage) []byte {
	if len(b) == 0 {
		return []byte(`{}`)
	}
	return b
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func username() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "jupytui"
}
