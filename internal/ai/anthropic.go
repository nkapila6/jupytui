package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
)

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	// a refused turn gets re-served by a fallback model picked by the API
	fallbackBeta = "server-side-fallback-2026-07-01"
)

type Anthropic struct {
	key, model, url string
	noFallback      bool // the account rejected the fallback beta
}

func NewAnthropic(key, model string) *Anthropic {
	url := anthropicURL
	// proxies and gateways, same variable the official SDKs read
	if base := os.Getenv("ANTHROPIC_BASE_URL"); base != "" {
		url = strings.TrimRight(base, "/") + "/v1/messages"
	}
	return &Anthropic{key: key, model: model, url: url}
}

func (a *Anthropic) Name() string                    { return "anthropic/" + a.model }
func (a *Anthropic) Model() string                   { return a.model }
func (a *Anthropic) Vision(context.Context) Tristate { return Yes }

func (a *Anthropic) body(req Request) map[string]any {
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = 64000
	}
	b := map[string]any{
		"model":      a.model,
		"max_tokens": maxTok,
		"stream":     true,
		"messages":   anthropicMessages(req.Messages),
		"thinking":   map[string]any{"type": "adaptive", "display": "summarized"},
		// caches everything up to the last block, so each turn only pays
		// for what's new
		"cache_control": map[string]any{"type": "ephemeral"},
	}
	if req.System != "" {
		b["system"] = []map[string]any{{"type": "text", "text": req.System}}
	}
	if req.Effort != "" {
		b["output_config"] = map[string]any{"effort": req.Effort}
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Schema})
		}
		b["tools"] = tools
	}
	if !a.noFallback {
		b["fallbacks"] = "default"
	}
	return b
}

func anthropicMessages(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		var content []any
		for _, p := range m.Parts {
			if b := anthropicBlock(p); b != nil {
				content = append(content, b)
			}
		}
		if len(content) > 0 {
			out = append(out, map[string]any{"role": m.Role, "content": content})
		}
	}
	return out
}

func anthropicBlock(p Part) any {
	switch {
	case p.Raw != nil:
		return p.Raw
	case p.Image != nil:
		return map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": p.Image.Mime, "data": base64.StdEncoding.EncodeToString(p.Image.Data),
		}}
	case p.ToolResult != nil:
		var content []any
		for _, q := range p.ToolResult.Parts {
			if b := anthropicBlock(q); b != nil {
				content = append(content, b)
			}
		}
		b := map[string]any{"type": "tool_result", "tool_use_id": p.ToolResult.CallID, "content": content}
		if p.ToolResult.IsError {
			b["is_error"] = true
		}
		return b
	case p.ToolCall != nil:
		return map[string]any{"type": "tool_use", "id": p.ToolCall.ID, "name": p.ToolCall.Name, "input": p.ToolCall.Input}
	case p.Text != "":
		return map[string]any{"type": "text", "text": p.Text}
	}
	return nil
}

func (a *Anthropic) Stream(ctx context.Context, req Request) (<-chan Chunk, error) {
	headers := map[string]string{"x-api-key": a.key, "anthropic-version": anthropicVersion}
	if !a.noFallback {
		headers["anthropic-beta"] = fallbackBeta
	}
	resp, err := post(ctx, a.url, headers, a.body(req))
	if api, ok := err.(*APIError); ok && !a.noFallback && api.Status == 400 && strings.Contains(strings.ToLower(api.Msg), "fallback") {
		// not every account or platform has the beta, go without
		a.noFallback = true
		return a.Stream(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	out := make(chan Chunk, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		s := &anthropicStream{out: out}
		err := readSSE(resp.Body, s.event)
		if !s.done {
			if err == nil {
				err = s.err
			}
			if err == nil {
				err = ctx.Err()
			}
			if err == nil {
				err = &APIError{Msg: "stream ended early"}
			}
			out <- Chunk{Done: true, Err: err}
		}
	}()
	return out, nil
}

// anthropicStream rebuilds content blocks from the deltas, keeping each
// one in the API's own shape so it can be sent back as is.
type anthropicStream struct {
	out    chan<- Chunk
	blocks []map[string]any
	json   map[int]*strings.Builder // tool input arriving in pieces
	usage  Usage
	stop   string
	done   bool
	err    error
}

func (s *anthropicStream) event(ev sseEvent) bool {
	var d struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		ContentBlock map[string]any `json:"content_block"`
		Delta        struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage anthropicUsage `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
		return true
	}
	switch d.Type {
	case "message_start":
		s.usage = d.Message.Usage.usage()
	case "content_block_start":
		for len(s.blocks) <= d.Index {
			s.blocks = append(s.blocks, nil)
		}
		s.blocks[d.Index] = d.ContentBlock
	case "content_block_delta":
		if d.Index >= len(s.blocks) || s.blocks[d.Index] == nil {
			return true
		}
		b := s.blocks[d.Index]
		switch d.Delta.Type {
		case "text_delta":
			b["text"] = str(b["text"]) + d.Delta.Text
			s.out <- Chunk{Text: d.Delta.Text}
		case "thinking_delta":
			b["thinking"] = str(b["thinking"]) + d.Delta.Thinking
			s.out <- Chunk{Thinking: d.Delta.Thinking}
		case "signature_delta":
			b["signature"] = str(b["signature"]) + d.Delta.Signature
		case "input_json_delta":
			if s.json == nil {
				s.json = map[int]*strings.Builder{}
			}
			if s.json[d.Index] == nil {
				s.json[d.Index] = &strings.Builder{}
			}
			s.json[d.Index].WriteString(d.Delta.PartialJSON)
		}
	case "content_block_stop":
		if j := s.json[d.Index]; j != nil && d.Index < len(s.blocks) {
			raw := json.RawMessage(j.String())
			if len(raw) == 0 {
				raw = json.RawMessage(`{}`)
			}
			s.blocks[d.Index]["input"] = raw
		}
	case "message_delta":
		if d.Delta.StopReason != "" {
			s.stop = d.Delta.StopReason
		}
		s.usage.Out = d.Usage.OutputTokens
	case "message_stop":
		s.done = true
		s.out <- Chunk{Done: true, Message: s.message(), Stop: s.stop, Usage: s.usage}
		return false
	case "error":
		s.err = &APIError{Msg: d.Error.Type + ": " + d.Error.Message}
		return false
	}
	return true
}

func (s *anthropicStream) message() *Message {
	m := &Message{Role: "assistant"}
	for _, b := range s.blocks {
		if b == nil {
			continue
		}
		raw, _ := json.Marshal(b)
		p := Part{Raw: raw}
		switch b["type"] {
		case "text":
			p.Text = str(b["text"])
		case "thinking":
			p.Thinking = str(b["thinking"])
		case "tool_use":
			input, _ := json.Marshal(b["input"])
			p.ToolCall = &ToolCall{ID: str(b["id"]), Name: str(b["name"]), Input: input}
		}
		m.Parts = append(m.Parts, p)
	}
	return m
}

type anthropicUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

func (u anthropicUsage) usage() Usage {
	return Usage{In: u.InputTokens, Out: u.OutputTokens, CacheRead: u.CacheReadTokens, CacheWrite: u.CacheCreationTokens}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
