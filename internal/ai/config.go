package ai

import (
	"errors"
	"os"
	"strings"
)

const DefaultModel = "claude-opus-5-5"

// Help is what to set when nothing is configured.
const Help = "set ANTHROPIC_API_KEY, or JUPYTUI_AI=openrouter/<model> with OPENROUTER_API_KEY, or JUPYTUI_AI=openai/<model> with JUPYTUI_AI_BASE_URL for a local server"

// FromEnv picks the provider from JUPYTUI_AI (provider/model), falling
// back to Anthropic when its key is set. Keys only ever come from the
// environment.
func FromEnv() (Provider, error) {
	return Parse(os.Getenv("JUPYTUI_AI"))
}

// Parse builds a provider from "provider/model" (or "" for the default).
// OpenRouter model names have a slash of their own, so only the first
// one splits.
func Parse(spec string) (Provider, error) {
	provider, model, _ := strings.Cut(strings.TrimSpace(spec), "/")
	if provider == "" {
		switch {
		case os.Getenv("ANTHROPIC_API_KEY") != "":
			provider = "anthropic"
		case os.Getenv("JUPYTUI_AI_BASE_URL") != "":
			provider = "openai"
		default:
			return nil, errors.New(Help)
		}
	}
	switch provider {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, errors.New("ANTHROPIC_API_KEY isn't set")
		}
		if model == "" {
			model = DefaultModel
		}
		return NewAnthropic(key, model), nil
	case "openrouter":
		key := os.Getenv("OPENROUTER_API_KEY")
		if key == "" {
			return nil, errors.New("OPENROUTER_API_KEY isn't set")
		}
		if model == "" {
			return nil, errors.New("pick an OpenRouter model: JUPYTUI_AI=openrouter/<vendor>/<model>")
		}
		return NewOpenRouter(key, model), nil
	case "openai", "local", "ollama":
		base := os.Getenv("JUPYTUI_AI_BASE_URL")
		if base == "" {
			if provider == "ollama" {
				base = "http://localhost:11434/v1"
			} else {
				return nil, errors.New("JUPYTUI_AI_BASE_URL isn't set (Ollama is http://localhost:11434/v1)")
			}
		}
		if model == "" {
			return nil, errors.New("pick a model: JUPYTUI_AI=" + provider + "/<model>")
		}
		return NewOpenAI(os.Getenv("OPENAI_API_KEY"), model, base), nil
	}
	return nil, errors.New("unknown AI provider " + provider + ": use anthropic, openrouter, openai or ollama")
}
