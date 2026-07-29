package queryforge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestFallbackFirstSucceeds: the first provider answers, later ones are untouched.
func TestFallbackFirstSucceeds(t *testing.T) {
	p1 := &StubProvider{Response: "from-1"}
	p2 := &StubProvider{Response: "from-2"}
	f := NewFallbackProvider(p1, p2)

	out, err := f.Complete(context.Background(), "s", "u")
	if err != nil || out != "from-1" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if p1.Calls != 1 || p2.Calls != 0 {
		t.Errorf("expected only p1 called: p1=%d p2=%d", p1.Calls, p2.Calls)
	}
	if f.LastUsed() != "model[0]" {
		t.Errorf("LastUsed=%q", f.LastUsed())
	}
}

// TestFallbackFallsThrough: the first fails (like a quota/billing block), the
// second answers — the exact scenario this feature exists for.
func TestFallbackFallsThrough(t *testing.T) {
	down := &StubProvider{Err: errors.New("endpoint returned 429: quota exceeded")}
	up := &StubProvider{Response: "recovered"}
	f := NewFallbackProvider(down, up)

	out, err := f.Complete(context.Background(), "s", "u")
	if err != nil || out != "recovered" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if down.Calls != 1 || up.Calls != 1 {
		t.Errorf("both should be tried: down=%d up=%d", down.Calls, up.Calls)
	}
	if f.LastUsed() != "model[1]" {
		t.Errorf("LastUsed=%q", f.LastUsed())
	}
}

// TestFallbackAllFail: the combined error names every provider's failure.
func TestFallbackAllFail(t *testing.T) {
	a := &StubProvider{Err: errors.New("boom-A")}
	b := &StubProvider{Err: errors.New("boom-B")}
	f := NewFallbackProvider(a, b)

	_, err := f.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("expected an error when all providers fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "boom-A") || !strings.Contains(msg, "boom-B") || !strings.Contains(msg, "all 2 model(s) failed") {
		t.Errorf("combined error missing detail: %s", msg)
	}
}

// TestFallbackEmptyChain is the worst case.
func TestFallbackEmptyChain(t *testing.T) {
	f := &FallbackProvider{}
	if _, err := f.Complete(context.Background(), "s", "u"); err == nil {
		t.Error("empty chain must error")
	}
}

// TestFallbackContextCancelled stops trying once the context is done.
func TestFallbackContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	p := &StubProvider{Err: errors.New("x")}
	f := NewFallbackProvider(p)
	if _, err := f.Complete(ctx, "s", "u"); err == nil {
		t.Error("expected error under cancelled context")
	}
	if p.Calls != 0 {
		t.Errorf("cancelled context should skip providers, got %d calls", p.Calls)
	}
}

// TestProvidersFromShapes verifies the chain assembly rules.
func TestProvidersFromShapes(t *testing.T) {
	// Single model -> a concrete provider, not a FallbackProvider.
	single := &Config{Model: ModelConfig{Provider: "gemini", BaseURL: "https://x/openai", Model: "m"}}
	if _, ok := ProvidersFrom(single).(*FallbackProvider); ok {
		t.Errorf("single model should not be a FallbackProvider")
	}

	// Primary + one fallback -> chain of 2.
	primaryPlus := &Config{
		Model:  ModelConfig{Provider: "gemini", BaseURL: "https://x/openai", Model: "m"},
		Models: []ModelConfig{{Provider: "anthropic", Model: "claude-opus-4-8"}},
	}
	fp, ok := ProvidersFrom(primaryPlus).(*FallbackProvider)
	if !ok || fp.Size() != 2 {
		t.Errorf("expected chain of 2, got %T size=%d", ProvidersFrom(primaryPlus), sizeOf(ProvidersFrom(primaryPlus)))
	}

	// Blank primary + two fallbacks -> chain of 2 (blank primary skipped).
	onlyModels := &Config{
		Models: []ModelConfig{
			{Provider: "groq", BaseURL: "https://api.groq.com/openai/v1", Model: "a"},
			{Provider: "anthropic", Model: "claude-opus-4-8"},
		},
	}
	fp2, ok := ProvidersFrom(onlyModels).(*FallbackProvider)
	if !ok || fp2.Size() != 2 {
		t.Errorf("blank primary should be skipped, expected size 2")
	}
}

func sizeOf(p ModelProvider) int {
	if f, ok := p.(*FallbackProvider); ok {
		return f.Size()
	}
	return 1
}

// TestModelsConfigParses confirms the `models` array round-trips through the
// strict loader.
func TestModelsConfigParses(t *testing.T) {
	c := mustParse(t, `{
      "entity":"Order",
      "model":{"provider":"gemini","baseURL":"https://x/openai","model":"g","apiKeyEnv":"QF_API_KEY"},
      "models":[
        {"provider":"groq","baseURL":"https://api.groq.com/openai/v1","model":"llama","apiKeyEnv":"GROQ_KEY"},
        {"provider":"anthropic","baseURL":"https://api.anthropic.com","model":"claude-opus-4-8","apiKeyEnv":"ANTHROPIC_API_KEY"}
      ],
      "fields":[{"name":"status","type":"enum","values":["A","B"]}]
    }`)
	if len(c.Models) != 2 {
		t.Fatalf("expected 2 fallback models, got %d", len(c.Models))
	}
	if c.Models[1].Label() != "anthropic/claude-opus-4-8" {
		t.Errorf("label = %q", c.Models[1].Label())
	}
}

// TestEngineTranslateFallsThrough proves the whole pipeline recovers when the
// primary model is down and a fallback answers.
func TestEngineTranslateFallsThrough(t *testing.T) {
	cfg := mustParse(t, genConfigJSON)
	down := &StubProvider{Err: errors.New("quota exceeded")}
	up := &StubProvider{Response: canonicalAST}
	e := NewWithProvider(cfg, NewFallbackProvider(down, up))
	e.Now = func() time.Time { return fixedNow }

	res, err := e.Translate(context.Background(), "delivered orders", "sql")
	if err != nil {
		t.Fatalf("Translate should have recovered via fallback: %v", err)
	}
	if res.ProviderUsed != "model[1]" {
		t.Errorf("ProviderUsed = %q, want model[1]", res.ProviderUsed)
	}
	if !strings.HasPrefix(res.Query.SQL, "SELECT") {
		t.Errorf("unexpected SQL: %s", res.Query.SQL)
	}
}
