package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	oai "github.com/openai/openai-go/v3"

	"github.com/dantearo/arkex/internal/config"
)

// UserAgent is sent on every request so server operators can identify arkex.
var UserAgent = "arkex/dev"

// Model is a ready-to-call language model plus the per-call options that
// express the configured compat settings.
type Model struct {
	Ref   config.ModelRef
	LM    fantasy.LanguageModel
	Opts  fantasy.ProviderOptions
	Limit *int64 // max output tokens, nil when unset
}

// Call builds a fantasy.Call for prompt and tools with this model's options.
func (m *Model) Call(prompt fantasy.Prompt, tools []fantasy.Tool) fantasy.Call {
	return fantasy.Call{
		Prompt:          prompt,
		Tools:           tools,
		MaxOutputTokens: m.Limit,
		ProviderOptions: m.Opts,
	}
}

// Open resolves credentials and constructs the language model for ref.
// Credentials are resolved here, once, and never stored on the returned
// value beyond what the HTTP client needs.
func Open(ctx context.Context, ref config.ModelRef, client *http.Client) (*Model, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	if ref.Conn.Kind == config.KindSubscription {
		switch ref.Conn.Subscription {
		case "", "chatgpt":
			return openChatGPT(ctx, ref, client)
		default:
			return nil, fmt.Errorf("unknown subscription %q", ref.Conn.Subscription)
		}
	}
	switch ref.Conn.API {
	case config.APIOpenAICompat, "":
		return openOpenAICompat(ctx, ref, client)
	case config.APIOpenAI, config.APIAnthropic, config.APIGoogle:
		return nil, fmt.Errorf("provider api %q is not supported yet; use %q", ref.Conn.API, config.APIOpenAICompat)
	default:
		return nil, fmt.Errorf("unknown provider api %q", ref.Conn.API)
	}
}

func openOpenAICompat(ctx context.Context, ref config.ModelRef, client *http.Client) (*Model, error) {
	if ref.Conn.BaseURL == "" {
		return nil, fmt.Errorf("provider %q: baseUrl is required", ref.ConnID)
	}
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(ref.Conn.BaseURL),
		openaicompat.WithHTTPClient(client),
		openaicompat.WithUserAgent(UserAgent),
		openaicompat.WithLanguageModelOptions(openai.WithLanguageModelStreamUsageFunc(preserveStreamUsage)),
	}
	if ref.Conn.APIKey != "" {
		key, err := config.ResolveValue(ctx, ref.Conn.APIKey)
		if err != nil {
			return nil, fmt.Errorf("provider %q apiKey: %w", ref.ConnID, err)
		}
		opts = append(opts, openaicompat.WithAPIKey(key))
	}
	if len(ref.Conn.Headers) > 0 {
		hdrs := make(map[string]string, len(ref.Conn.Headers))
		for k, v := range ref.Conn.Headers {
			rv, err := config.ResolveValue(ctx, v)
			if err != nil {
				return nil, fmt.Errorf("provider %q header %s: %w", ref.ConnID, k, err)
			}
			hdrs[k] = rv
		}
		opts = append(opts, openaicompat.WithHeaders(hdrs))
	}

	p, err := openaicompat.New(opts...)
	if err != nil {
		return nil, err
	}
	lm, err := p.LanguageModel(ctx, ref.Model.ID)
	if err != nil {
		return nil, err
	}

	po, err := OpenAICompatOptions(ref.Compat(), ref.Thinking)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", ref, err)
	}
	m := &Model{
		Ref:  ref,
		LM:   lm,
		Opts: fantasy.ProviderOptions{lm.Provider(): po},
	}
	if ref.Model.MaxTokens > 0 {
		m.Limit = new(int64(ref.Model.MaxTokens))
	}
	return m, nil
}

// Some gateways append performance-only usage chunks. Keep the last complete
// accounting snapshot in the request-local context, never across requests.
func preserveStreamUsage(chunk oai.ChatCompletionChunk, ctx map[string]any, metadata fantasy.ProviderMetadata) (fantasy.Usage, fantasy.ProviderMetadata) {
	const key = "arkex.lastUsage"
	if chunk.Usage.TotalTokens == 0 {
		if usage, ok := ctx[key].(fantasy.Usage); ok {
			return usage, metadata
		}
		return fantasy.Usage{}, metadata
	}
	usage, metadata := openai.DefaultStreamUsageFunc(chunk, ctx, metadata)
	ctx[key] = usage
	return usage, metadata
}
