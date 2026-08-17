package queryforge

import (
	"strings"
	"testing"
	"time"
)

// minimalConfig wraps a model block in the smallest legal config, so these
// tests exercise the real loader rather than a hand-built struct.
func minimalConfig(modelJSON string) string {
	return `{
	  "entity": "Order",
	  "model": ` + modelJSON + `,
	  "fields": [{"name":"status","type":"string","operators":["equals"]}]
	}`
}

// The rules these tests encode:
//
//   - Every new key is OPTIONAL. A config written before they existed must load
//     and behave identically.
//   - An explicit value must be distinguishable from an absent one, so that
//     "never retry" is expressible.
//   - A nonsensical value fails at LOAD, not on the first user question.

// TestNewModelKeysAreOptional is the backward-compatibility guarantee. The
// loader rejects unknown keys, so the risk runs the other way: these tests
// confirm an untouched config still loads and gets the documented defaults.
func TestNewModelKeysAreOptional(t *testing.T) {
	c, err := ParseConfig([]byte(minimalConfig(`{
	  "provider": "gemini",
	  "baseURL": "https://generativelanguage.googleapis.com/v1beta/openai",
	  "model": "gemini-3.5-flash",
	  "apiKeyEnv": "QF_API_KEY"
	}`)))
	if err != nil {
		t.Fatalf("a config with none of the new keys must still load: %v", err)
	}

	m := c.Model
	if got := m.EffectiveTimeout(); got != defaultTimeout {
		t.Errorf("timeout = %v, want the previous hardcoded %v", got, defaultTimeout)
	}
	if got := m.EffectiveMaxRetries(); got != defaultMaxRetries {
		t.Errorf("maxRetries = %d, want %d", got, defaultMaxRetries)
	}
	if got := m.EffectiveRetryBackoff(); got != defaultRetryBackoff {
		t.Errorf("retryBackoff = %v, want %v", got, defaultRetryBackoff)
	}
	if m.Protocol != "" {
		t.Errorf("protocol = %q, want it absent", m.Protocol)
	}
}

// TestExplicitZeroRetriesIsDistinctFromUnset: "never retry" is a real choice —
// a caller with a fallback chain may prefer instant failover to waiting. It has
// to be expressible, which is why MaxRetries is a pointer.
func TestExplicitZeroRetriesIsDistinctFromUnset(t *testing.T) {
	c, err := ParseConfig([]byte(minimalConfig(`{"model":"m","maxRetries":0}`)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Model.EffectiveMaxRetries(); got != 0 {
		t.Errorf("an explicit maxRetries:0 gave %d, want 0 — "+
			"if this defaults instead, disabling retries is impossible", got)
	}
}

// TestNonsensicalTuningIsTreatedAsUnset: a negative timeout is a typo, and
// honouring it literally would fail every call before it started with an error
// pointing nowhere near the config.
func TestNonsensicalTuningIsTreatedAsUnset(t *testing.T) {
	cases := []struct {
		name  string
		json  string
		check func(*testing.T, ModelConfig)
	}{
		{
			name: "negative timeout falls back to the default",
			json: `{"model":"m","timeoutSeconds":-5}`,
			check: func(t *testing.T, m ModelConfig) {
				if got := m.EffectiveTimeout(); got != defaultTimeout {
					t.Errorf("timeout = %v, want %v", got, defaultTimeout)
				}
			},
		},
		{
			name: "negative retries clamps to none",
			json: `{"model":"m","maxRetries":-3}`,
			check: func(t *testing.T, m ModelConfig) {
				if got := m.EffectiveMaxRetries(); got != 0 {
					t.Errorf("maxRetries = %d, want 0", got)
				}
			},
		},
		{
			name: "negative backoff falls back to the default",
			json: `{"model":"m","retryBackoffMs":-100}`,
			check: func(t *testing.T, m ModelConfig) {
				if got := m.EffectiveRetryBackoff(); got != defaultRetryBackoff {
					t.Errorf("backoff = %v, want %v", got, defaultRetryBackoff)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(minimalConfig(tc.json)))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			tc.check(t, c.Model)
		})
	}
}

// TestTuningValuesAreApplied: the settings must actually reach the provider,
// or they are decoration.
func TestTuningValuesAreApplied(t *testing.T) {
	c, err := ParseConfig([]byte(minimalConfig(`{
	  "provider": "groq",
	  "model": "m",
	  "timeoutSeconds": 7,
	  "maxRetries": 4,
	  "retryBackoffMs": 900
	}`)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := c.Model.EffectiveTimeout(); got != 7*time.Second {
		t.Errorf("timeout = %v, want 7s", got)
	}

	p, ok := ProviderFor(c.Model).(*OpenAIProvider)
	if !ok {
		t.Fatal("expected an OpenAI-compatible provider")
	}
	if p.HTTPClient.Timeout != 7*time.Second {
		t.Errorf("the HTTP client got %v, want the configured 7s", p.HTTPClient.Timeout)
	}
	if p.MaxRetries != 4 {
		t.Errorf("provider MaxRetries = %d, want 4", p.MaxRetries)
	}
	if p.RetryBackoff != 900*time.Millisecond {
		t.Errorf("provider RetryBackoff = %v, want 900ms", p.RetryBackoff)
	}
	if p.BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("baseURL = %q, want the groq preset", p.BaseURL)
	}
}

// TestAnUnimplementedProtocolIsRejectedAtLoad: a dialect the library cannot
// speak is a permanent property of the config. Discovering it on the first user
// question — in production, after startup succeeded — is strictly worse.
func TestAnUnimplementedProtocolIsRejectedAtLoad(t *testing.T) {
	_, err := ParseConfig([]byte(minimalConfig(`{"protocol":"grpc","model":"m"}`)))
	if err == nil {
		t.Fatal("an unknown protocol must fail at load")
	}

	msg := err.Error()
	if !strings.Contains(msg, "grpc") {
		t.Error("the message should name the value that was rejected")
	}
	if !strings.Contains(msg, "openai") || !strings.Contains(msg, "anthropic") {
		t.Errorf("the message should list what IS available, since the set is small and closed: %s", msg)
	}
}

// TestImplementedProtocolsAreAccepted, including odd casing.
func TestImplementedProtocolsAreAccepted(t *testing.T) {
	for _, p := range []string{"openai", "anthropic", "OpenAI", "ANTHROPIC", " openai "} {
		if _, err := ParseConfig([]byte(minimalConfig(`{"protocol":"` + p + `","model":"m"}`))); err != nil {
			t.Errorf("protocol %q should be accepted: %v", p, err)
		}
	}
}

// TestFallbackEntriesAreValidatedLikeThePrimary: a typo in models[2] that only
// surfaces when the primary is already down is the worst possible time to find
// it. Every entry must be held to identical rules at load.
func TestFallbackEntriesAreValidatedLikeThePrimary(t *testing.T) {
	js := `{
	  "entity": "Order",
	  "model": {"provider":"groq","model":"a"},
	  "models": [
	    {"provider":"openai","model":"b"},
	    {"protocol":"telepathy","model":"c"}
	  ],
	  "fields": [{"name":"status","type":"string","operators":["equals"]}]
	}`

	_, err := ParseConfig([]byte(js))
	if err == nil {
		t.Fatal("a bad protocol in a fallback entry must fail at load")
	}
	if !strings.Contains(err.Error(), "models[1]") {
		t.Errorf("the error should say WHICH entry is wrong, got: %v", err)
	}
}

// TestAPastedKeyIsStillRejectedInFallbackEntries: the pre-existing apiKeyEnv
// guard must not have been weakened by moving it behind validateModelBlock.
func TestAPastedKeyIsStillRejectedInFallbackEntries(t *testing.T) {
	js := `{
	  "entity": "Order",
	  "model": {"provider":"groq","model":"a","apiKeyEnv":"QF_API_KEY"},
	  "models": [{"provider":"openai","model":"b","apiKeyEnv":"sk-ant-realkeyhere"}],
	  "fields": [{"name":"status","type":"string","operators":["equals"]}]
	}`

	err := func() error { _, e := ParseConfig([]byte(js)); return e }()
	if err == nil {
		t.Fatal("a pasted API key in a fallback entry must be rejected")
	}
	if strings.Contains(err.Error(), "sk-ant-realkeyhere") {
		t.Error("the error must not echo the pasted secret")
	}
	if !strings.Contains(err.Error(), "models[0]") {
		t.Errorf("the error should locate the offending entry, got: %v", err)
	}
}

// TestAProviderNameAloneIsAWorkingConfig is the "hassle-free" claim, checked
// end to end through the loader: no URL to look up, no key for a local server.
func TestAProviderNameAloneIsAWorkingConfig(t *testing.T) {
	c, err := ParseConfig([]byte(minimalConfig(`{"provider":"ollama","model":"llama3"}`)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	p, ok := ProvidersFrom(c).(*OpenAIProvider)
	if !ok {
		t.Fatal("expected a single OpenAI-compatible provider")
	}
	if p.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("baseURL = %q, want the ollama default", p.BaseURL)
	}
	if p.Model != "llama3" {
		t.Errorf("model = %q, want llama3", p.Model)
	}
}
