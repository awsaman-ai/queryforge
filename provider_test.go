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

// TestOpenAIProviderHappyPath spins up a fake OpenAI-compatible endpoint and
// verifies the request shape (path, auth, body) and response parsing — all
// offline.
func TestOpenAIProviderHappyPath(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody chatRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "test-model", Temperature: 0})
	p.APIKey = "secret-key"

	out, err := p.Complete(context.Background(), "sys", "user text")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != `{"ok":true}` {
		t.Errorf("content = %q", out)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.Model != "test-model" || len(gotBody.Messages) != 2 ||
		gotBody.Messages[0].Role != "system" || gotBody.Messages[1].Content != "user text" {
		t.Errorf("request body wrong: %#v", gotBody)
	}
}

// TestOpenAIProviderHTTPError checks non-2xx handling.
func TestOpenAIProviderHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `rate limited`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m"})
	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("expected 429 error, got %v", err)
	}
}

// TestOpenAIProviderModelError checks the error field in a 200 body.
func TestOpenAIProviderModelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":{"message":"context length exceeded"}}`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m"})
	_, err := p.Complete(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "context length") {
		t.Errorf("expected model error, got %v", err)
	}
}

// TestOpenAIProviderNoChoices checks the empty-choices case.
func TestOpenAIProviderNoChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()

	p := NewOpenAIProvider(ModelConfig{BaseURL: srv.URL, Model: "m"})
	if _, err := p.Complete(context.Background(), "s", "u"); err == nil {
		t.Error("expected error on empty choices")
	}
}

// TestStubProvider covers the deterministic test double.
func TestStubProvider(t *testing.T) {
	s := &StubProvider{Response: "hello"}
	out, err := s.Complete(context.Background(), "sys", "usr")
	if err != nil || out != "hello" {
		t.Fatalf("stub happy: out=%q err=%v", out, err)
	}
	if s.LastSystem != "sys" || s.LastUser != "usr" || s.Calls != 1 {
		t.Errorf("stub did not record call: %+v", s)
	}
}
