package queryforge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAnthropicProviderHappyPath verifies the native Messages API request shape
// (path, headers, top-level system field) and response parsing — offline.
func TestAnthropicProviderHappyPath(t *testing.T) {
	var gotKey, gotVersion, gotPath string
	var gotBody anthropicRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		io.WriteString(w, `{"content":[{"type":"text","text":"{\"entity\":\"Order\"}"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()

	p := NewAnthropicProvider(ModelConfig{Provider: "anthropic", BaseURL: srv.URL, Model: "claude-opus-4-8"})
	p.APIKey = "sk-ant-test"

	out, err := p.Complete(context.Background(), "system prompt", "user request")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"entity":"Order"}` {
		t.Errorf("content = %q", out)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("path = %q", gotPath)
	}
	if gotKey != "sk-ant-test" || gotVersion != "2023-06-01" {
		t.Errorf("auth headers wrong: key=%q version=%q", gotKey, gotVersion)
	}
	if gotBody.System != "system prompt" { // system is a top-level field, not a message
		t.Errorf("system field wrong: %q", gotBody.System)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Role != "user" || gotBody.Messages[0].Content != "user request" {
		t.Errorf("messages wrong: %#v", gotBody.Messages)
	}
	if gotBody.MaxTokens <= 0 {
		t.Errorf("max_tokens must be set on the Messages API, got %d", gotBody.MaxTokens)
	}
}

// TestAnthropicProviderRefusal maps a safety refusal to an error.
func TestAnthropicProviderRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"content":[],"stop_reason":"refusal"}`)
	}))
	defer srv.Close()

	p := NewAnthropicProvider(ModelConfig{BaseURL: srv.URL})
	p.APIKey = "k"
	if _, err := p.Complete(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "refus") {
		t.Errorf("expected refusal error, got %v", err)
	}
}

// TestAnthropicProviderHTTPError surfaces non-2xx responses.
func TestAnthropicProviderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	p := NewAnthropicProvider(ModelConfig{BaseURL: srv.URL})
	p.APIKey = "bad"
	if _, err := p.Complete(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 error, got %v", err)
	}
}

// TestProviderForSelection checks config-driven provider selection.
func TestProviderForSelection(t *testing.T) {
	if _, ok := ProviderFor(ModelConfig{Provider: "anthropic"}).(*AnthropicProvider); !ok {
		t.Errorf("provider=anthropic should select AnthropicProvider")
	}
	if _, ok := ProviderFor(ModelConfig{BaseURL: "https://api.anthropic.com"}).(*AnthropicProvider); !ok {
		t.Errorf("api.anthropic.com should select AnthropicProvider")
	}
	if _, ok := ProviderFor(ModelConfig{BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"}).(*OpenAIProvider); !ok {
		t.Errorf("gemini openai endpoint should select OpenAIProvider")
	}
	// Anthropic's own OpenAI-compat path (if ever used) stays on the OpenAI provider.
	if _, ok := ProviderFor(ModelConfig{BaseURL: "https://api.anthropic.com/v1/openai"}).(*OpenAIProvider); !ok {
		t.Errorf("anthropic /openai path should select OpenAIProvider")
	}
}
