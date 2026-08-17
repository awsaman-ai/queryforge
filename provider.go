package queryforge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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

	// MaxRetries is the number of additional attempts a RETRYABLE failure gets
	// (rate limit, 5xx, dropped connection). Zero — the zero value, and so the
	// behaviour of any hand-constructed provider — means no retrying, exactly
	// as before this field existed. NewOpenAIProvider applies the config default.
	MaxRetries int

	// RetryBackoff is the first retry delay, doubling and jittered thereafter.
	// Zero uses the package default.
	RetryBackoff time.Duration

	ProviderID string   // provider label for events, e.g. "gemini"; cosmetic only
	Observe    Observer // optional; receives one EventModelCall per round trip

	// jitter overrides backoff randomisation in tests. nil = real jitter.
	jitter func(time.Duration) time.Duration
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
	// The endpoint comes from resolveEndpoint, so a known provider name alone
	// ("groq", "ollama") is a complete configuration; an explicit baseURL still
	// overrides it.
	baseURL, _ := resolveEndpoint(m)
	return &OpenAIProvider{
		BaseURL:      baseURL,
		Model:        m.Model,
		APIKey:       key,
		Temperature:  m.Temperature,
		MaxTokens:    m.MaxTokens,
		JSONMode:     m.EffectiveJSONMode(), // off unless the config opts in; see ModelConfig.JSONMode
		HTTPClient:   &http.Client{Timeout: m.EffectiveTimeout()},
		MaxRetries:   m.EffectiveMaxRetries(),
		RetryBackoff: m.EffectiveRetryBackoff(),
		ProviderID:   m.Provider, // may be empty; purely a label on events
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

// Complete performs a chat completion and returns the assistant's text,
// retrying round trips whose failure might be fixed by waiting.
//
// The retry loop lives here rather than in the engine because only this layer
// can tell a rate limit from a bad key. The engine's repair loop retries the
// MODEL's reasoning by changing the prompt; this retries the TRANSPORT with an
// identical request. Conflating them would let a throttled endpoint silently
// consume the repair budget, and a genuinely confused model silently consume
// the retry budget.
func (p *OpenAIProvider) Complete(ctx context.Context, system, user string) (string, error) {
	rp := retryPolicy{MaxRetries: p.MaxRetries, BaseBackoff: p.RetryBackoff, jitter: p.jitter}
	return rp.do(ctx, func(ctx context.Context, n int) (string, error) {
		return p.roundTrip(ctx, system, user, n)
	})
}

// roundTrip performs exactly one HTTP request and emits exactly one
// EventModelCall for it — including when it is a retry, so a chain of attempts
// is visible individually rather than averaged into a single event.
func (p *OpenAIProvider) roundTrip(ctx context.Context, system, user string, retry int) (out string, err error) {
	// This method is the only place that sees round-trip latency, token usage,
	// and finish_reason — the three facts worth watching, since the model call
	// is ~99.97% of a translation's wall time. Report them on EVERY return path
	// via defer, so no early exit can silently drop the event; the outcome is
	// derived from the named error return rather than repeated at each exit.
	start := time.Now()
	ev := Event{Kind: EventModelCall, Provider: p.ProviderID, Model: p.Model, Outcome: OutcomeOK, Retry: retry}
	defer func() {
		ev.Latency = time.Since(start)
		if err != nil {
			ev.Outcome = OutcomeTransport // no usable text came back, whatever the cause
			ev.Err = err
			if pe, ok := asProviderError(err); ok {
				ev.ErrorKind = pe.Kind
			}
		}
		p.Observe.emit(ctx, ev)
	}()

	if p.BaseURL == "" {
		return "", noEndpointError(ModelConfig{Provider: p.ProviderID})
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
		return "", p.fail(KindInvalidRequest, 0, "marshal request: "+err.Error(), err)
	}

	// Construct and send the HTTP request.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", p.fail(KindInvalidRequest, 0, "build request: "+err.Error(), err)
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
		return "", p.fail(classifyTransport(ctx, err), 0, "request failed: "+err.Error(), err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		// A body that dies mid-read is a broken connection, not a bad reply.
		return "", p.fail(classifyTransport(ctx, err), resp.StatusCode, "read response: "+err.Error(), err)
	}

	// Non-2xx: classify by status, and surface a bounded, redacted snippet.
	if resp.StatusCode/100 != 2 {
		body := snippet(data, 300)
		pe := p.fail(classifyStatus(resp.StatusCode, body), resp.StatusCode, body, nil)
		pe.RetryAfter = parseRetryAfter(resp.Header)
		return "", errModelUnreachable(pe)
	}

	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", p.fail(KindBadResponse, resp.StatusCode, "decode response: "+err.Error(), err)
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

	// Some endpoints report failure in a 200 body rather than the status line.
	// Classify from the message so a 200-wrapped rate limit still backs off
	// instead of being mistaken for a broken model.
	if cr.Error != nil && cr.Error.Message != "" {
		kind := KindBadResponse
		if looksLikeQuota(cr.Error.Message) {
			kind = KindQuota
		}
		return "", p.fail(kind, resp.StatusCode, "model error: "+cr.Error.Message, nil)
	}
	if len(cr.Choices) == 0 {
		return "", p.fail(KindBadResponse, resp.StatusCode, "response contained no choices", nil)
	}

	// A "length" finish means maxTokens ran out mid-reply. The content that came
	// back is a fragment — typically valid-looking JSON missing its closing brace
	// — so passing it on produces a baffling parse error that blames the model
	// for malformed output. Reasoning models make this easy to hit: their hidden
	// thinking tokens are charged against the same budget, so the visible answer
	// can be cut off even when it would have been short. Say what actually
	// happened, and name the knob that fixes it.
	//
	// Classified BAD_RESPONSE, which is deliberately not retryable: the request
	// is deterministic, so retrying truncates at exactly the same place while
	// billing for it again.
	if cr.Choices[0].FinishReason == "length" {
		hidden := cr.Usage.TotalTokens - cr.Usage.PromptTokens - cr.Usage.CompletionTokens
		return "", p.fail(KindBadResponse, resp.StatusCode, fmt.Sprintf(
			"reply truncated by the token budget (finish_reason=length; prompt=%d, completion=%d, hidden reasoning=%d, total=%d): raise model.maxTokens in the config",
			cr.Usage.PromptTokens, cr.Usage.CompletionTokens, hidden, cr.Usage.TotalTokens), nil)
	}

	return cr.Choices[0].Message.Content, nil
}

// fail builds a classified error tagged with this provider's identity, with the
// API key scrubbed from the detail text.
func (p *OpenAIProvider) fail(kind ProviderErrorKind, status int, detail string, cause error) *ProviderError {
	return newProviderError(kind, status, p.ProviderID, p.Model, detail, p.APIKey, cause)
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
