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
	"time"
)

// AnthropicProvider calls Anthropic's native Messages API (/v1/messages) — the
// first-party dialect, not an OpenAI-compatible shim. It exists to demonstrate
// the ModelProvider seam: a second provider with a completely different wire
// format drops in with no change to the planner, validator, or generators. It
// uses only the standard library, preserving QueryForge's zero-dependency core.
type AnthropicProvider struct {
	BaseURL    string       // endpoint root; default https://api.anthropic.com
	Model      string       // model id, e.g. claude-opus-4-8
	APIKey     string       // sent as the x-api-key header
	MaxTokens  int          // response cap (Messages API requires max_tokens)
	Version    string       // anthropic-version header value
	HTTPClient *http.Client // injectable for tests/timeouts

	// MaxRetries / RetryBackoff mirror the OpenAI provider: zero means no
	// retrying, which is the behaviour a hand-constructed provider had before
	// these fields existed. NewAnthropicProvider applies the config defaults.
	MaxRetries   int
	RetryBackoff time.Duration

	ProviderID string   // provider label for events; defaults to "anthropic"
	Observe    Observer // optional; receives one EventModelCall per round trip

	// jitter overrides backoff randomisation in tests. nil = real jitter.
	jitter func(time.Duration) time.Duration
}

// SetObserver installs the Observer, satisfying observerSetter. See the twin
// method on OpenAIProvider for why the seam is a separate interface.
func (p *AnthropicProvider) SetObserver(o Observer) { p.Observe = o }

// NewAnthropicProvider builds the provider from a config model block. The key is
// read from the environment variable NAMED by apiKeyEnv, never stored in config.
func NewAnthropicProvider(m ModelConfig) *AnthropicProvider {
	key := ""
	if m.APIKeyEnv != "" {
		key = os.Getenv(m.APIKeyEnv)
	}
	baseURL, _ := resolveEndpoint(m)
	if baseURL == "" || strings.Contains(baseURL, "/openai") {
		baseURL = "https://api.anthropic.com" // native Messages API root
	}
	model := m.Model
	if model == "" {
		model = "claude-opus-4-8" // capable default per the Claude API guidance
	}
	maxTokens := m.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024 // small structured output; plenty for an AST
	}
	providerID := m.Provider
	if providerID == "" {
		providerID = "anthropic" // this provider is only selected for Anthropic
	}
	// The Messages API's historical default here was 60s rather than the OpenAI
	// side's 30s. EffectiveTimeout reports the shared 30s default, so preserve
	// the longer one unless the config states a timeout explicitly.
	timeout := 60 * time.Second
	if m.TimeoutSeconds > 0 {
		timeout = m.EffectiveTimeout()
	}
	return &AnthropicProvider{
		BaseURL:      baseURL,
		Model:        model,
		APIKey:       key,
		MaxTokens:    maxTokens,
		Version:      "2023-06-01",
		HTTPClient:   &http.Client{Timeout: timeout},
		MaxRetries:   m.EffectiveMaxRetries(),
		RetryBackoff: m.EffectiveRetryBackoff(),
		ProviderID:   providerID,
	}
}

// anthropicRequest is the Messages API request body. The system prompt is a
// top-level field (not a message), and temperature is deliberately omitted —
// current Opus models reject it with a 400.
type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
}

// anthropicMessage is one turn; content accepts a plain string for simple text.
type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the subset of the Messages API response we consume. The
// usage block names its counts differently from the OpenAI dialect
// (input/output rather than prompt/completion) and reports no total, so the
// mapping onto Event happens in Complete rather than being assumed here.
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete performs a Messages API call, retrying retryable round trips. See
// OpenAIProvider.Complete for why the retry loop belongs at this layer.
func (p *AnthropicProvider) Complete(ctx context.Context, system, user string) (string, error) {
	rp := retryPolicy{MaxRetries: p.MaxRetries, BaseBackoff: p.RetryBackoff, jitter: p.jitter}
	return rp.do(ctx, func(ctx context.Context, n int) (string, error) {
		return p.roundTrip(ctx, system, user, n)
	})
}

// roundTrip performs exactly one Messages API request, emitting one
// EventModelCall for it.
func (p *AnthropicProvider) roundTrip(ctx context.Context, system, user string, retry int) (out string, err error) {
	// One EventModelCall per round trip, on every return path — see the twin
	// block in OpenAIProvider.roundTrip for the reasoning.
	start := time.Now()
	ev := Event{Kind: EventModelCall, Provider: p.ProviderID, Model: p.Model, Outcome: OutcomeOK, Retry: retry}
	defer func() {
		ev.Latency = time.Since(start)
		if err != nil {
			ev.Outcome = OutcomeTransport
			ev.Err = err
			if pe, ok := asProviderError(err); ok {
				ev.ErrorKind = pe.Kind
			}
		}
		p.Observe.emit(ctx, ev)
	}()

	if p.APIKey == "" {
		return "", p.fail(KindAuth, 0, "no API key (set the env var named in the config's apiKeyEnv)", nil)
	}

	// Build the request body.
	body := anthropicRequest{
		Model:     p.Model,
		MaxTokens: p.MaxTokens,
		System:    system, // system prompt is a top-level field on the Messages API
		Messages:  []anthropicMessage{{Role: "user", Content: user}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", p.fail(KindInvalidRequest, 0, "marshal request: "+err.Error(), err)
	}

	// Construct the request with the Messages API's required headers.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return "", p.fail(KindInvalidRequest, 0, "build request: "+err.Error(), err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", p.APIKey)          // first-party auth header (not Bearer)
	req.Header.Set("anthropic-version", p.Version) // required API version pin

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
		return "", p.fail(classifyTransport(ctx, err), resp.StatusCode, "read response: "+err.Error(), err)
	}
	if resp.StatusCode/100 != 2 {
		body := snippet(data, 300)
		pe := p.fail(classifyStatus(resp.StatusCode, body), resp.StatusCode, body, nil)
		pe.RetryAfter = parseRetryAfter(resp.Header)
		return "", errModelUnreachable(pe)
	}

	var ar anthropicResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return "", p.fail(KindBadResponse, resp.StatusCode, "decode response: "+err.Error(), err)
	}
	// Map this dialect's usage block onto the common event shape. Anthropic
	// reports input/output separately and no total, so the total is derived;
	// hidden reasoning tokens are not broken out by this API, which is why
	// HiddenTokens stays zero here rather than being guessed.
	ev.PromptTokens = ar.Usage.InputTokens
	ev.CompletionTokens = ar.Usage.OutputTokens
	ev.TotalTokens = ar.Usage.InputTokens + ar.Usage.OutputTokens
	ev.FinishReason = ar.StopReason

	if ar.Error != nil && ar.Error.Message != "" {
		kind := KindBadResponse
		if looksLikeQuota(ar.Error.Message) {
			kind = KindQuota
		}
		return "", p.fail(kind, resp.StatusCode, "model error: "+ar.Error.Message, nil)
	}
	if ar.StopReason == "refusal" {
		return "", p.fail(KindBadResponse, resp.StatusCode, "request was refused by the model's safety classifier", nil)
	}
	// The Messages API's equivalent of finish_reason=length. Same reasoning as
	// the OpenAI side: the fragment looks like valid JSON with its tail missing,
	// so say what happened instead of letting the parser blame the model. Not
	// retryable — an identical request truncates identically.
	if ar.StopReason == "max_tokens" {
		return "", p.fail(KindBadResponse, resp.StatusCode, fmt.Sprintf(
			"reply truncated by the token budget (stop_reason=max_tokens; input=%d, output=%d): raise model.maxTokens in the config",
			ar.Usage.InputTokens, ar.Usage.OutputTokens), nil)
	}

	// Concatenate all text blocks (there is usually exactly one).
	var b strings.Builder
	for _, block := range ar.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	out = b.String()
	if out == "" {
		return "", p.fail(KindBadResponse, resp.StatusCode, "response contained no text content", nil)
	}
	return out, nil
}

// fail builds a classified error tagged with this provider's identity, with the
// API key scrubbed from the detail text.
func (p *AnthropicProvider) fail(kind ProviderErrorKind, status int, detail string, cause error) *ProviderError {
	return newProviderError(kind, status, p.ProviderID, p.Model, detail, p.APIKey, cause)
}

// ProviderFor selects the provider implementation for a config's model block.
//
// Selection is by PROTOCOL, resolved in resolveEndpoint from — in order — an
// explicit `protocol`, the provider name's preset, then the legacy URL sniff.
// Routing on the resolved dialect rather than re-inspecting the URL here is
// what lets an Anthropic-compatible gateway on a corporate hostname, or an
// OpenAI-compatible route containing "/openai", be stated rather than guessed.
//
// Model selection stays a config change, never a code change.
func ProviderFor(m ModelConfig) ModelProvider {
	if _, proto := resolveEndpoint(m); proto == ProtocolAnthropic {
		return NewAnthropicProvider(m)
	}
	return NewOpenAIProvider(m)
}
