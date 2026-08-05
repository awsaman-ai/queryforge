package queryforge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ModelProvider is the seam between QueryForge and any language model. It is a
// single method: given a system prompt and a user prompt, return the model's
// raw text. The default implementation speaks the OpenAI-compatible dialect;
// callers may supply their own (a different dialect, or a deterministic stub
// for tests) without touching the rest of the library.
type ModelProvider interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// OpenAIProvider calls any OpenAI-compatible /chat/completions endpoint. The
// same implementation therefore covers hosted APIs (OpenAI, Groq, Gemini's
// compatibility endpoint, OpenRouter) and self-hosted servers (Ollama, vLLM) —
// the only difference is baseURL/model/key, all chosen in config.
type OpenAIProvider struct {
	BaseURL     string       // endpoint root, e.g. https://api.groq.com/openai/v1
	Model       string       // model id, e.g. gemini-2.0-flash
	APIKey      string       // bearer token; empty is allowed for keyless local servers
	Temperature float64      // sampling temperature (0 = deterministic)
	MaxTokens   int          // response cap; 0 lets the server decide
	JSONMode    bool         // request response_format=json_object when supported
	HTTPClient  *http.Client // injectable for tests/timeouts

	ProviderID string   // provider label for events, e.g. "gemini"; cosmetic only
	Observe    Observer // optional; receives one EventModelCall per round trip
}

// SetObserver installs the Observer. It satisfies observerSetter, which is how
// Engine.SetObserver reaches into a provider without ModelProvider — a public
// one-method interface that third parties implement — having to grow a second
// method.
func (p *OpenAIProvider) SetObserver(o Observer) { p.Observe = o }

// NewOpenAIProvider builds a provider from the config's model block. The API key
// is read from the environment variable NAMED by apiKeyEnv — the key value
// itself is never stored in config.
func NewOpenAIProvider(m ModelConfig) *OpenAIProvider {
	key := ""
	if m.APIKeyEnv != "" {
		key = os.Getenv(m.APIKeyEnv) // resolve the env var name to its value
	}
	return &OpenAIProvider{
		BaseURL:     strings.TrimRight(m.BaseURL, "/"), // normalize trailing slash
		Model:       m.Model,
		APIKey:      key,
		Temperature: m.Temperature,
		MaxTokens:   m.MaxTokens,
		JSONMode:    m.EffectiveJSONMode(), // off unless the config opts in; see ModelConfig.JSONMode
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
		ProviderID:  m.Provider, // may be empty; purely a label on events
	}
}

// chatRequest is the OpenAI-compatible request body.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

// chatMessage is one role/content turn.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// responseFormat requests structured JSON output where the provider supports it.
type responseFormat struct {
	Type string `json:"type"` // "json_object"
}

// chatResponse is the subset of the response we consume. finish_reason matters
// as much as the content: a truncated reply is still a 200 with plausible-looking
// text, and only finish_reason distinguishes it from a complete one.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"` // "stop" = complete, "length" = truncated
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete performs one chat completion and returns the assistant's text.
func (p *OpenAIProvider) Complete(ctx context.Context, system, user string) (out string, err error) {
	// This method is the only place that sees round-trip latency, token usage,
	// and finish_reason — the three facts worth watching, since the model call
	// is ~99.97% of a translation's wall time. Report them on EVERY return path
	// via defer, so no early exit can silently drop the event; the outcome is
	// derived from the named error return rather than repeated at each exit.
	start := time.Now()
	ev := Event{Kind: EventModelCall, Provider: p.ProviderID, Model: p.Model, Outcome: OutcomeOK}
	defer func() {
		ev.Latency = time.Since(start)
		if err != nil {
			ev.Outcome = OutcomeTransport // no usable text came back, whatever the cause
			ev.Err = err
		}
		p.Observe.emit(ctx, ev)
	}()

	if p.BaseURL == "" {
		return "", fmt.Errorf("provider: no baseURL configured")
	}

	// Build the request body.
	reqBody := chatRequest{
		Model: p.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: p.Temperature,
		MaxTokens:   p.MaxTokens,
	}
	if p.JSONMode {
		reqBody.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("provider: marshal request: %w", err)
	}

	// Construct and send the HTTP request.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("provider: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey) // bearer auth for hosted APIs
	}

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("provider: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("provider: read response: %w", err)
	}

	// Non-2xx: surface a bounded snippet of the body for debugging.
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("provider: endpoint returned %d: %s", resp.StatusCode, snippet(data, 300))
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", fmt.Errorf("provider: decode response: %w", err)
	}
	// Record what the reply cost as soon as it is decoded, so the event carries
	// the numbers even on the failure paths below — a truncated or empty reply
	// still burned tokens, and those are exactly the cases worth costing.
	ev.PromptTokens = cr.Usage.PromptTokens
	ev.CompletionTokens = cr.Usage.CompletionTokens
	ev.TotalTokens = cr.Usage.TotalTokens
	ev.HiddenTokens = hiddenTokens(cr.Usage.TotalTokens, cr.Usage.PromptTokens, cr.Usage.CompletionTokens)
	if len(cr.Choices) > 0 {
		ev.FinishReason = cr.Choices[0].FinishReason
	}

	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("provider: model error: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("provider: response contained no choices")
	}

	// A "length" finish means maxTokens ran out mid-reply. The content that came
	// back is a fragment — typically valid-looking JSON missing its closing brace
	// — so passing it on produces a baffling parse error that blames the model
	// for malformed output. Reasoning models make this easy to hit: their hidden
	// thinking tokens are charged against the same budget, so the visible answer
	// can be cut off even when it would have been short. Say what actually
	// happened, and name the knob that fixes it.
	if cr.Choices[0].FinishReason == "length" {
		hidden := cr.Usage.TotalTokens - cr.Usage.PromptTokens - cr.Usage.CompletionTokens
		return "", fmt.Errorf("provider: reply truncated by the token budget (finish_reason=length; prompt=%d, completion=%d, hidden reasoning=%d, total=%d): raise model.maxTokens in the config",
			cr.Usage.PromptTokens, cr.Usage.CompletionTokens, hidden, cr.Usage.TotalTokens)
	}

	return cr.Choices[0].Message.Content, nil
}

// snippet returns at most n bytes of s, for safe inclusion in error messages.
func snippet(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// StubProvider is a deterministic ModelProvider for tests and offline use: it
// returns a preset response (or error) and records the last prompts it saw, so
// the planner and engine can be exercised with no network and no API key.
//
// An Engine is safe for concurrent use, so a stub standing in for a real
// provider has to be too — otherwise wiring one in turns any concurrency test of
// the surrounding code into a race report about the stub. The bookkeeping
// fields are therefore written under mu; read them with Snapshot when more than
// one goroutine is in play, or directly in the ordinary sequential case.
type StubProvider struct {
	Response string // canned assistant text to return
	Err      error  // when set, Complete returns this error

	mu         sync.Mutex // guards the three fields below
	LastSystem string     // captured system prompt from the most recent call
	LastUser   string     // captured user prompt from the most recent call
	Calls      int        // number of times Complete was invoked
}

// Complete records the prompts and returns the preset response/error.
func (s *StubProvider) Complete(_ context.Context, system, user string) (string, error) {
	s.mu.Lock()
	s.LastSystem = system
	s.LastUser = user
	s.Calls++
	s.mu.Unlock()

	if s.Err != nil {
		return "", s.Err
	}
	return s.Response, nil
}

// Snapshot returns the recorded prompts and call count consistently. Use it
// instead of reading the fields when Complete may be running concurrently.
func (s *StubProvider) Snapshot() (lastSystem, lastUser string, calls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.LastSystem, s.LastUser, s.Calls
}
