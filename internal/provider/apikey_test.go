package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/config"
)

const inlineKey = "AIzaSyLEAKCANARY-0123456789abcdef"

func TestOpenAIProviderUsesInlineAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(config.ModelConfig{BaseURL: srv.URL, Model: "m", APIKey: inlineKey})
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+inlineKey {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// A key pasted with a trailing newline would otherwise be rejected by net/http
// as an invalid header value, with an error that never mentions the cause.
func TestOpenAIProviderTrimsPastedWhitespace(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()
	p := NewOpenAIProvider(config.ModelConfig{BaseURL: srv.URL, Model: "m", APIKey: config.Secret(" " + inlineKey + "\n")})
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+inlineKey {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestOpenAIProviderEnvPathUnchanged(t *testing.T) {
	t.Setenv("QF_TEST_KEY", "env-key-value\n")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()
	p := NewOpenAIProvider(config.ModelConfig{BaseURL: srv.URL, Model: "m", APIKeyEnv: "QF_TEST_KEY"})
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer env-key-value" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

func TestOpenAIProviderKeylessSendsNoAuthorization(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()
	p := NewOpenAIProvider(config.ModelConfig{BaseURL: srv.URL, Model: "m"})
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Error("a keyless block must not send an Authorization header")
	}
}

// A misbehaving gateway that echoes the request header back in its error body
// must not put the inline key into the returned error.
func TestInlineKeyIsRedactedFromProviderErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `bad credentials: `+r.Header.Get("Authorization")+` / raw `+inlineKey)
	}))
	defer srv.Close()
	p := NewOpenAIProvider(config.ModelConfig{BaseURL: srv.URL, Model: "m", APIKey: inlineKey})
	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), inlineKey) || strings.Contains(err.Error(), "LEAKCANARY") {
		t.Errorf("error leaks the key: %v", err)
	}
	if !strings.Contains(err.Error(), "REDACTED") {
		t.Errorf("expected the redaction marker, got %v", err)
	}
}

func TestAnthropicProviderUsesInlineAPIKey(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		io.WriteString(w, `{"content":[{"type":"text","text":"{}"}],"stop_reason":"end_turn"}`)
	}))
	defer srv.Close()
	p := NewAnthropicProvider(config.ModelConfig{Provider: "anthropic", BaseURL: srv.URL, Model: "m", APIKey: config.Secret(inlineKey + "\n")})
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if gotKey != inlineKey {
		t.Errorf("x-api-key = %q", gotKey)
	}
}

func TestAnthropicInlineKeyRedactedFromErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid x-api-key `+r.Header.Get("x-api-key")+`"}}`)
	}))
	defer srv.Close()
	p := NewAnthropicProvider(config.ModelConfig{Provider: "anthropic", BaseURL: srv.URL, Model: "m", APIKey: inlineKey})
	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil || strings.Contains(err.Error(), inlineKey) {
		t.Errorf("want a redacted error, got %v", err)
	}
}

// The selector must hand the inline key to whichever provider the protocol picks.
func TestProviderForCarriesInlineKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":"{}"}}]}`)
	}))
	defer srv.Close()
	pr := ProviderFor(config.ModelConfig{Provider: "custom", BaseURL: srv.URL, Model: "m", APIKey: inlineKey})
	if _, err := pr.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer "+inlineKey {
		t.Errorf("Authorization = %q", gotAuth)
	}
}
