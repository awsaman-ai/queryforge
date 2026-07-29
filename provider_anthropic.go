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
}

// NewAnthropicProvider builds the provider from a config model block. The key is
// read from the environment variable NAMED by apiKeyEnv, never stored in config.
func NewAnthropicProvider(m ModelConfig) *AnthropicProvider {
	key := ""
	if m.APIKeyEnv != "" {
		key = os.Getenv(m.APIKeyEnv)
	}
	baseURL := strings.TrimRight(m.BaseURL, "/")
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
	return &AnthropicProvider{
		BaseURL:    baseURL,
		Model:      model,
		APIKey:     key,
		MaxTokens:  maxTokens,
		Version:    "2023-06-01",
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
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

// anthropicResponse is the subset of the Messages API response we consume.
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete performs one Messages API call and returns the concatenated text.
func (p *AnthropicProvider) Complete(ctx context.Context, system, user string) (string, error) {
	if p.APIKey == "" {
		return "", fmt.Errorf("provider: no API key (set the env var named in the config's apiKeyEnv)")
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
		return "", fmt.Errorf("provider: marshal request: %w", err)
	}

	// Construct the request with the Messages API's required headers.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("provider: build request: %w", err)
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
		return "", fmt.Errorf("provider: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("provider: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("provider: endpoint returned %d: %s", resp.StatusCode, snippet(data, 300))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return "", fmt.Errorf("provider: decode response: %w", err)
	}
	if ar.Error != nil && ar.Error.Message != "" {
		return "", fmt.Errorf("provider: model error: %s", ar.Error.Message)
	}
	if ar.StopReason == "refusal" {
		return "", fmt.Errorf("provider: request was refused by the model's safety classifier")
	}

	// Concatenate all text blocks (there is usually exactly one).
	var b strings.Builder
	for _, block := range ar.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	out := b.String()
	if out == "" {
		return "", fmt.Errorf("provider: response contained no text content")
	}
	return out, nil
}

// ProviderFor selects the right provider from a config's model block: the native
// Anthropic provider when the block names Anthropic, otherwise the default
// OpenAI-compatible provider. This keeps model selection a config change, never
// a code change.
func ProviderFor(m ModelConfig) ModelProvider {
	if strings.EqualFold(m.Provider, "anthropic") ||
		(strings.Contains(m.BaseURL, "api.anthropic.com") && !strings.Contains(m.BaseURL, "/openai")) {
		return NewAnthropicProvider(m)
	}
	return NewOpenAIProvider(m)
}
