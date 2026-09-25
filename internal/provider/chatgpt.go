package provider

import (
	"context"
	"fmt"
	"net/http"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"

	"github.com/attrition-tech/arkex/internal/chatgpt"
	"github.com/attrition-tech/arkex/internal/config"
)

// AuthStore is where subscription sign-ins are read from. Tests point it
// at a temp file.
var AuthStore = chatgpt.DefaultStore

// openChatGPT builds a model on the Codex backend using the ChatGPT
// sign-in saved for the connection. The saved tokens are loaded once here
// and live only in the transport, which refreshes and re-saves them.
func openChatGPT(ctx context.Context, ref config.ModelRef, client *http.Client) (*Model, error) {
	store, err := AuthStore()
	if err != nil {
		return nil, err
	}
	tok, ok, err := store.Get(ref.ConnID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("connection %q is not signed in; open /connections and sign in with ChatGPT", ref.ConnID)
	}
	base := ref.Conn.BaseURL
	if base == "" {
		base = chatgpt.BaseURL
	}
	tr := chatgpt.NewTransport(client.Transport, tok, store, ref.ConnID, UserAgent)
	tr.Endpoints = chatgpt.Endpoints{BaseURL: base}
	hc := &http.Client{Transport: tr, Timeout: client.Timeout}

	p, err := openai.New(
		openai.WithBaseURL(base),
		// The transport replaces this with the real bearer token; the SDK
		// only insists that something is set.
		openai.WithAPIKey("chatgpt"),
		openai.WithHTTPClient(hc),
		openai.WithUseResponsesAPI(),
		openai.WithResponsesAPIFunc(func(string) bool { return true }),
		openai.WithUserAgent(UserAgent),
	)
	if err != nil {
		return nil, err
	}
	lm, err := p.LanguageModel(ctx, ref.Model.ID)
	if err != nil {
		return nil, err
	}
	po, err := OpenAIResponsesOptions(ref.Thinking)
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", ref, err)
	}
	// No Limit: the Codex backend does not take max_output_tokens, and the
	// transport strips it anyway.
	return &Model{
		Ref:  ref,
		LM:   lm,
		Opts: fantasy.ProviderOptions{lm.Provider(): po},
	}, nil
}

// OpenAIResponsesOptions builds the per-call options for a Responses API
// request on the Codex backend: encrypted reasoning comes back so the
// next turn can carry it (nothing is stored server-side), reasoning
// summaries are requested, and the thinking level maps onto reasoning
// effort. "max" becomes "xhigh", the highest level the backend accepts.
func OpenAIResponsesOptions(level string) (*openai.ResponsesProviderOptions, error) {
	if level == "on" {
		return nil, fmt.Errorf("responses API reasoning requires an explicit effort level")
	}
	if !validLevel(level) {
		return nil, fmt.Errorf("unknown thinking level %q (want one of %v)", level, ThinkingLevels)
	}
	opts := &openai.ResponsesProviderOptions{
		Include:          []openai.IncludeType{openai.IncludeReasoningEncryptedContent},
		ReasoningSummary: new("auto"),
		Store:            new(false),
	}
	switch level {
	case "":
		// backend default
	case "off":
		opts.ReasoningEffort = new(openai.ReasoningEffortNone)
	case "max":
		opts.ReasoningEffort = new(openai.ReasoningEffortXHigh)
	default:
		opts.ReasoningEffort = effort(level)
	}
	return opts, nil
}
