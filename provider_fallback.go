package queryforge

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// FallbackProvider tries an ordered list of providers and returns the first
// successful completion. Because it implements ModelProvider itself, it composes
// transparently: the planner and engine call it exactly like any single
// provider. This is how QueryForge stays flexible around models — an outage,
// rate-limit, or quota/billing block on one provider transparently falls through
// to the next, all chosen in config.
type FallbackProvider struct {
	entries []fallbackEntry // tried in order

	mu       sync.Mutex // guards lastUsed (an engine may be shared across goroutines)
	lastUsed string     // label of the provider that answered most recently
}

// fallbackEntry pairs a provider with a human-readable label for telemetry.
type fallbackEntry struct {
	label    string
	provider ModelProvider
}

// NewFallbackProvider builds a fallback chain from bare providers, labelling them
// by position. Prefer ProvidersFrom when building from a config (it labels each
// entry by provider/model).
func NewFallbackProvider(providers ...ModelProvider) *FallbackProvider {
	entries := make([]fallbackEntry, len(providers))
	for i, p := range providers {
		entries[i] = fallbackEntry{label: fmt.Sprintf("model[%d]", i), provider: p}
	}
	return &FallbackProvider{entries: entries}
}

// Complete tries each provider in order, returning the first success. It falls
// through on ANY error (transport failure, 4xx/5xx, quota/billing, refusal) —
// the point is resilience, and the combined error names every failure so the
// caller can see what happened. It stops early if the context is cancelled.
func (f *FallbackProvider) Complete(ctx context.Context, system, user string) (string, error) {
	if len(f.entries) == 0 {
		return "", fmt.Errorf("provider: fallback chain is empty")
	}
	var failures []string
	for _, e := range f.entries {
		if ctx.Err() != nil { // honor a cancelled/expired context between attempts
			break
		}
		out, err := e.provider.Complete(ctx, system, user)
		if err == nil {
			f.setLastUsed(e.label) // remember who answered
			return out, nil
		}
		failures = append(failures, fmt.Sprintf("%s: %v", e.label, err))
	}
	return "", fmt.Errorf("all %d model(s) failed: %s", len(f.entries), strings.Join(failures, "; "))
}

// Size returns the number of providers in the chain.
func (f *FallbackProvider) Size() int { return len(f.entries) }

// LastUsed returns the label of the provider that answered the most recent
// successful call (empty before any success).
func (f *FallbackProvider) LastUsed() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUsed
}

func (f *FallbackProvider) setLastUsed(label string) {
	f.mu.Lock()
	f.lastUsed = label
	f.mu.Unlock()
}

// providerNamer is implemented by providers that can report which model answered
// (currently FallbackProvider). The engine uses it to populate ProviderUsed.
type providerNamer interface {
	LastUsed() string
}

// ProvidersFrom builds the model provider for a config. It assembles the ordered
// chain — the primary Model (if non-empty) followed by each entry in Models —
// and returns a FallbackProvider when there is more than one, or the single
// provider directly when there is one. Selecting the right dialect per entry
// (OpenAI-compatible vs native Anthropic) is delegated to ProviderFor, so a
// chain can freely mix providers.
func ProvidersFrom(c *Config) ModelProvider {
	var entries []fallbackEntry
	add := func(m ModelConfig) {
		if m.isEmpty() {
			return // skip a blank block (e.g. no primary Model, only Models set)
		}
		entries = append(entries, fallbackEntry{label: m.Label(), provider: ProviderFor(m)})
	}
	add(c.Model)
	for _, m := range c.Models {
		add(m)
	}

	switch len(entries) {
	case 0:
		// Degenerate config with no model at all — return a provider anyway so
		// the deterministic path (GenerateFrom) still works; a Translate call
		// will surface a clear "no baseURL" error.
		return NewOpenAIProvider(c.Model)
	case 1:
		return entries[0].provider
	default:
		return &FallbackProvider{entries: entries}
	}
}
