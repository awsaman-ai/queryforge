package config

import (
	"fmt"
	"sort"
	"strings"
)

// The wire-dialect vocabulary.
//
// Protocol and its validation live in the config package rather than next to the
// provider presets that consume them for one structural reason: validateProtocol
// is a method on ModelConfig, which is declared here, and Go requires a method to
// share a package with its receiver. Everything else about provider selection —
// the preset table, endpoint resolution, protocol sniffing — stays in
// internal/provider, which imports these names.

// Protocol names the wire dialect a provider speaks. It is deliberately a tiny,
// closed set: a protocol is a piece of code QueryForge has to implement, unlike
// a provider or a model, both of which are just configuration data.
//
// This is the distinction that keeps QueryForge model-agnostic. Adding a model
// is a config edit. Adding a provider is a config edit. Only a genuinely new
// wire format is a code change — and almost nobody ships one, because the
// OpenAI dialect won.
type Protocol string

const (
	// ProtocolOpenAI is the OpenAI /chat/completions dialect. It covers OpenAI
	// itself and every provider that chose compatibility over invention:
	// Groq, DeepSeek, Together, Fireworks, Mistral, Cerebras, xAI, OpenRouter,
	// NVIDIA NIM, Ollama, vLLM, LM Studio, and any private endpoint that
	// implements the same route.
	ProtocolOpenAI Protocol = "openai"

	// ProtocolAnthropic is Anthropic's native Messages API (/v1/messages).
	ProtocolAnthropic Protocol = "anthropic"
)

// KnownProtocols is the validation set for the config's `protocol` key. It is
// exported so internal/provider — which owns the preset table that names a
// protocol per provider — can assert that every preset points at one this
// library actually implements.
var KnownProtocols = map[Protocol]bool{
	ProtocolOpenAI:    true,
	ProtocolAnthropic: true,
}

// validateProtocol rejects a protocol the library cannot speak.
//
// Rejected at LOAD rather than at the first call: an unknown dialect is a
// permanent property of the config, and discovering it on the first user
// question — after the process is up and serving — is strictly worse than
// discovering it at startup. The message lists what is available, because the
// set is small and closed, which is exactly when enumerating is helpful rather
// than a promise to maintain.
func (m ModelConfig) validateProtocol(where string) error {
	raw := strings.TrimSpace(m.Protocol)
	if raw == "" {
		return nil // absent is the norm: inferred from provider, or sniffed
	}
	if KnownProtocols[Protocol(strings.ToLower(raw))] {
		return nil
	}
	names := make([]string, 0, len(KnownProtocols))
	for p := range KnownProtocols {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return fmt.Errorf("config: %s.protocol %q is not a wire dialect QueryForge implements (available: %s). "+
		"Most providers speak %q — including every OpenAI-compatible endpoint, local servers, and aggregators. "+
		"Leave protocol unset to infer it from the provider name",
		where, raw, strings.Join(names, ", "), ProtocolOpenAI)
}
