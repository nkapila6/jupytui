package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
)

const openRouterURL = "https://openrouter.ai/api/v1"

// OpenAI speaks chat completions, which OpenRouter and the local servers
// (Ollama, llama.cpp, LM Studio, vLLM) all serve.
type OpenAI struct {
	name, key, model, base string
	openRouter             bool

	once   sync.Once
	vision Tristate
}

func NewOpenRouter(key, model string) *OpenAI {
	return &OpenAI{name: "openrouter", key: key, model: model, base: openRouterURL, openRouter: true}
}

func NewOpenAI(key, model, base string) *OpenAI {
	return &OpenAI{name: "openai", key: key, model: model, base: strings.TrimRight(base, "/")}
}

func (o *OpenAI) Name() string  { return o.name + "/" + o.model }
func (o *OpenAI) Model() string { return o.model }

// Vision asks OpenRouter's model list. Local servers have no standard
// way to say, so they stay unknown until a request with an image is tried.
func (o *OpenAI) Vision(ctx context.Context) Tristate {
	if !o.openRouter {
		return Unknown
	}
	o.once.Do(func() {
		req, err := http.NewRequestWithContext(ctx, "GET", o.base+"/models", nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		var list struct {
			Data []struct {
				ID           string `json:"id"`
				Architecture struct {
					InputModalities []string `json:"input_modalities"`
				} `json:"architecture"`
			} `json:"data"`
		}
		if json.NewDecoder(resp.Body).Decode(&list) != nil {
			return
		}
		for _, m := range list.Data {
			if m.ID != o.model {
				continue
			}
			o.vision = No
			for _, x := range m.Architecture.InputModalities {
				if x == "image" {
					o.vision = Yes
				}
			}
		}
	})
	return o.vision
}

func (o *OpenAI) body(req Request) map[string]any {
	var msgs []map[string]any
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, openAIMessages(m)...)
	}
	b := map[string]any{
		"model":          o.model,
		"stream":         true,
		"messages":       msgs,
		"stream_options": map[string]any{"include_usage": true},
	}
	if req.MaxTokens > 0 {
		b["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		var tools []map[string]any
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": t.Schema,
			}})
		}
		b["tools"] = tools
	}
	if o.openRouter && req.Effort != "" {
		// OpenRouter maps this onto whatever the model calls it
		b["reasoning"] = map[string]any{"effort": req.Effort}
	}
	return b
}

// openAIMessages turns one message into chat completions messages: tool
// results become their own role:tool messages.
func openAIMessages(m Message) []map[string]any {
	var out []map[string]any
	if m.Role == "assistant" {
		msg := map[string]any{"role": "assistant"}
		var text strings.Builder
		var calls []map[string]any
		for _, p := range m.Parts {
			switch {
			case p.ToolCall != nil:
				calls = append(calls, map[string]any{"id": p.ToolCall.ID, "type": "function", "function": map[string]any{
					"name": p.ToolCall.Name, "arguments": string(p.ToolCall.Input),
				}})
			case p.Text != "":
				text.WriteString(p.Text)
			}
		}
		msg["content"] = text.String()
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		return append(out, msg)
	}
	var content []map[string]any
	for _, p := range m.Parts {
		switch {
		case p.ToolResult != nil:
			// chat completions tool messages are text only; images in a
			// result follow as a user message
			var text []string
			var imgs []map[string]any
			for _, q := range p.ToolResult.Parts {
				if q.Image != nil {
					imgs = append(imgs, imageURL(q.Image))
				} else if q.Text != "" {
					text = append(text, q.Text)
				}
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": p.ToolResult.CallID, "content": strings.Join(text, "\n")})
			if len(imgs) > 0 {
				out = append(out, map[string]any{"role": "user", "content": imgs})
			}
		case p.Image != nil:
			content = append(content, imageURL(p.Image))
		case p.Text != "":
			content = append(content, map[string]any{"type": "text", "text": p.Text})
		}
	}
	if len(content) > 0 {
		out = append(out, map[string]any{"role": "user", "content": content})
	}
	return out
}

func imageURL(img *Image) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{
		"url": "data:" + img.Mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
	}}
}

func (o *OpenAI) Stream(ctx context.Context, req Request) (<-chan Chunk, error) {
	headers := map[string]string{}
	if o.key != "" {
		headers["authorization"] = "Bearer " + o.key
	}
	if o.openRouter {
		headers["X-Title"] = "jupytui"
	}
	resp, err := post(ctx, o.base+"/chat/completions", headers, o.body(req))
	if err != nil {
		return nil, err
	}
	out := make(chan Chunk, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		s := &openAIStream{out: out, calls: map[int]*openAICall{}}
		err := readSSE(resp.Body, s.event)
		if s.err != nil {
			out <- Chunk{Done: true, Err: s.err}
			return
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			out <- Chunk{Done: true, Err: err}
			return
		}
		// some servers end without [DONE]
		out <- s.final()
	}()
	return out, nil
}

type openAICall struct {
	id, name string
	args     strings.Builder
}

type openAIStream struct {
	out   chan<- Chunk
	text  strings.Builder
	calls map[int]*openAICall
	stop  string
	usage Usage
	err   error
}

func (s *openAIStream) event(ev sseEvent) bool {
	if strings.TrimSpace(ev.Data) == "[DONE]" {
		return false
	}
	var d struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			Details          struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
		return true
	}
	if d.Error != nil {
		s.err = &APIError{Msg: d.Error.Message}
		return false
	}
	if d.Usage != nil {
		s.usage = Usage{In: d.Usage.PromptTokens, Out: d.Usage.CompletionTokens, CacheRead: d.Usage.Details.CachedTokens}
	}
	for _, c := range d.Choices {
		if t := c.Delta.Content; t != "" {
			s.text.WriteString(t)
			s.out <- Chunk{Text: t}
		}
		if t := c.Delta.Reasoning + c.Delta.ReasoningContent; t != "" {
			s.out <- Chunk{Thinking: t}
		}
		for _, tc := range c.Delta.ToolCalls {
			call := s.calls[tc.Index]
			if call == nil {
				call = &openAICall{}
				s.calls[tc.Index] = call
			}
			if tc.ID != "" {
				call.id = tc.ID
			}
			if tc.Function.Name != "" {
				call.name = tc.Function.Name
			}
			call.args.WriteString(tc.Function.Arguments)
		}
		if c.FinishReason != "" {
			s.stop = c.FinishReason
		}
	}
	return true
}

func (s *openAIStream) final() Chunk {
	m := &Message{Role: "assistant"}
	if s.text.Len() > 0 {
		m.Parts = append(m.Parts, Part{Text: s.text.String()})
	}
	idx := make([]int, 0, len(s.calls))
	for i := range s.calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		c := s.calls[i]
		args := json.RawMessage(c.args.String())
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		m.Parts = append(m.Parts, Part{ToolCall: &ToolCall{ID: c.id, Name: c.name, Input: args}})
	}
	// map onto the Anthropic names the rest of the code uses
	stop := map[string]string{"stop": "end_turn", "tool_calls": "tool_use", "length": "max_tokens", "content_filter": "refusal"}[s.stop]
	if stop == "" {
		stop = s.stop
	}
	return Chunk{Done: true, Message: m, Stop: stop, Usage: s.usage}
}
