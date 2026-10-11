// Package ai talks to language models: Anthropic's Messages API, or any
// OpenAI-compatible chat completions endpoint (OpenRouter, Ollama,
// llama.cpp, LM Studio). Plain net/http, streamed over SSE.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Part is one piece of a message. Assistant parts keep Raw, the block as
// the provider sent it, so it can go back unchanged on the next turn
// (Anthropic needs thinking blocks echoed exactly).
type Part struct {
	Text       string          `json:"text,omitempty"`
	Image      *Image          `json:"image,omitempty"`
	ToolCall   *ToolCall       `json:"tool_call,omitempty"`
	ToolResult *ToolResult     `json:"tool_result,omitempty"`
	Thinking   string          `json:"thinking,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

type Image struct {
	Mime string `json:"mime"` // image/png or image/jpeg
	Data []byte `json:"data"`
	// for the placeholder when the model can't see images
	W int `json:"w"`
	H int `json:"h"`
}

type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type ToolResult struct {
	CallID  string `json:"call_id"`
	Parts   []Part `json:"parts"` // text and images
	IsError bool   `json:"is_error,omitempty"`
}

type Message struct {
	Role  string `json:"role"` // user or assistant
	Parts []Part `json:"parts"`
}

type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // JSON schema of the input
}

type Request struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
	Effort    string // low, medium, high
}

type Usage struct {
	In, Out, CacheRead, CacheWrite int
}

// Chunk is one streamed piece of a reply. The last one has Done set and
// Message holding the whole assistant turn.
type Chunk struct {
	Text     string
	Thinking string
	Note     string // something to tell the user, not part of the reply
	Done     bool
	Message  *Message
	Stop     string // end_turn, tool_use, max_tokens, refusal...
	Usage    Usage
	Err      error
}

type Provider interface {
	Name() string  // anthropic/claude-opus-5-5
	Model() string // claude-opus-5-5
	Stream(ctx context.Context, req Request) (<-chan Chunk, error)
	// Vision says whether the model takes images: yes, no, or unknown
	// until a request with one is tried.
	Vision(ctx context.Context) Tristate
}

type Tristate int

const (
	Unknown Tristate = iota
	Yes
	No
)

// APIError is a non-200 answer.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Msg) }

// Session wraps a provider with what's been learnt about it, like
// whether the model rejected images.
type Session struct {
	P        Provider
	NoImages bool // the user turned images off: placeholders, no note
	textOnly bool
	noted    bool
}

// Send streams a request. Images go out unless the model can't take
// them, in which case they become placeholders and the user hears about
// it once. A model of unknown ability that rejects a request with images
// is retried without them.
func (s *Session) Send(ctx context.Context, req Request) (<-chan Chunk, error) {
	if !s.textOnly && s.P.Vision(ctx) == No {
		s.textOnly = true
	}
	if s.NoImages {
		return s.P.Stream(ctx, s.strip(req))
	}
	if s.textOnly || !hasImages(req.Messages) {
		ch, err := s.P.Stream(ctx, s.strip(req))
		return s.withNote(ch), err
	}
	ch, err := s.P.Stream(ctx, req)
	var api *APIError
	if err != nil && errors.As(err, &api) && api.Status >= 400 && api.Status < 500 && api.Status != http.StatusTooManyRequests && s.P.Vision(ctx) == Unknown {
		s.textOnly = true
		ch, err = s.P.Stream(ctx, s.strip(req))
		return s.withNote(ch), err
	}
	return ch, err
}

func (s *Session) strip(req Request) Request {
	if !hasImages(req.Messages) {
		return req
	}
	out := req
	out.Messages = make([]Message, len(req.Messages))
	for i, m := range req.Messages {
		out.Messages[i] = Message{Role: m.Role, Parts: stripParts(m.Parts)}
	}
	return out
}

func stripParts(parts []Part) []Part {
	out := make([]Part, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.Image != nil:
			out = append(out, Part{Text: fmt.Sprintf("[image %dx%d]", p.Image.W, p.Image.H)})
		case p.ToolResult != nil:
			r := *p.ToolResult
			r.Parts = stripParts(r.Parts)
			out = append(out, Part{ToolResult: &r})
		default:
			out = append(out, p)
		}
	}
	return out
}

// withNote adds the one-time "can't see images" note to a stream.
func (s *Session) withNote(ch <-chan Chunk) <-chan Chunk {
	if ch == nil || !s.textOnly || s.noted {
		return ch
	}
	s.noted = true
	out := make(chan Chunk, 64)
	go func() {
		defer close(out)
		out <- Chunk{Note: s.P.Model() + " can't see images, plots are sent as [image WxH] placeholders"}
		for c := range ch {
			out <- c
		}
	}()
	return out
}

func hasImages(msgs []Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Image != nil {
				return true
			}
			if p.ToolResult != nil {
				for _, q := range p.ToolResult.Parts {
					if q.Image != nil {
						return true
					}
				}
			}
		}
	}
	return false
}
