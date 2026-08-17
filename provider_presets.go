package queryforge

import (
	"fmt"
	"sort"
	"strings"
)

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

// knownProtocols is the validation set for the config's `protocol` key.
var knownProtocols = map[Protocol]bool{
	ProtocolOpenAI:    true,
	ProtocolAnthropic: true,
}

// providerPreset is the small amount of data that turns a provider NAME into a
// reachable endpoint.
type providerPreset struct {
	BaseURL  string   // the endpoint root to use when the config names none
	Protocol Protocol // the dialect this provider speaks
}

// providerPresets maps a provider name to its endpoint and dialect.
//
// This is NOT the "hardcoded list of supported models" that would make
// QueryForge release-bound — it holds no model ids at all, and nothing here
// gates anything. It exists purely so that a user configuring Groq does not
// have to go and look up "https://api.groq.com/openai/v1", which is the single
// most common piece of friction in configuring an OpenAI-compatible provider.
//
// Three properties keep it from becoming a maintenance burden or a limitation:
//
//   - An explicit baseURL ALWAYS wins. A preset is a default, never a rule, so
//     a private mirror or a regional endpoint overrides it.
//   - An absent provider costs nothing. A name that is not in this map is still
//     a legal provider — set baseURL and it works exactly as before. Being
//     unlisted is not being unsupported.
//   - No entry can go stale in a way that blocks a user, because of the two
//     properties above.
//
// Endpoints only; deliberately no model ids, no pricing, no capability flags.
var providerPresets = map[string]providerPreset{
	// Native dialect.
	"anthropic": {BaseURL: "https://api.anthropic.com", Protocol: ProtocolAnthropic},

	// First-party OpenAI.
	"openai": {BaseURL: "https://api.openai.com/v1", Protocol: ProtocolOpenAI},

	// OpenAI-compatible hosted providers.
	"gemini":     {BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Protocol: ProtocolOpenAI},
	"google":     {BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Protocol: ProtocolOpenAI},
	"groq":       {BaseURL: "https://api.groq.com/openai/v1", Protocol: ProtocolOpenAI},
	"deepseek":   {BaseURL: "https://api.deepseek.com/v1", Protocol: ProtocolOpenAI},
	"together":   {BaseURL: "https://api.together.xyz/v1", Protocol: ProtocolOpenAI},
	"fireworks":  {BaseURL: "https://api.fireworks.ai/inference/v1", Protocol: ProtocolOpenAI},
	"mistral":    {BaseURL: "https://api.mistral.ai/v1", Protocol: ProtocolOpenAI},
	"cerebras":   {BaseURL: "https://api.cerebras.ai/v1", Protocol: ProtocolOpenAI},
	"xai":        {BaseURL: "https://api.x.ai/v1", Protocol: ProtocolOpenAI},
	"perplexity": {BaseURL: "https://api.perplexity.ai", Protocol: ProtocolOpenAI},
	"nvidia":     {BaseURL: "https://integrate.api.nvidia.com/v1", Protocol: ProtocolOpenAI},

	// Aggregator. Optional in every sense: it is one row in this map, nothing
	// depends on it, and removing it would change no behaviour for anyone who
	// does not name it.
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", Protocol: ProtocolOpenAI},

	// Local inference servers. Their defaults are the documented localhost
	// ports, so "provider": "ollama" alone is a working config on a stock
	// install — no key, no URL.
	"ollama":   {BaseURL: "http://localhost:11434/v1", Protocol: ProtocolOpenAI},
	"vllm":     {BaseURL: "http://localhost:8000/v1", Protocol: ProtocolOpenAI},
	"lmstudio": {BaseURL: "http://localhost:1234/v1", Protocol: ProtocolOpenAI},
}

// KnownProviders returns the provider names that have a built-in endpoint
// preset, sorted. Naming one lets you omit baseURL; any other name is still
// valid with an explicit baseURL.
func KnownProviders() []string {
	out := make([]string, 0, len(providerPresets))
	for name := range providerPresets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// lookupPreset finds a preset by provider name, case-insensitively.
func lookupPreset(provider string) (providerPreset, bool) {
	p, ok := providerPresets[strings.ToLower(strings.TrimSpace(provider))]
	return p, ok
}

// resolveEndpoint decides the base URL and protocol for a model block.
//
// Precedence is strictly most-explicit-first, and every step is a fallback for
// the step above it:
//
//  1. protocol / baseURL written in the config — the user said so.
//  2. the provider name's preset — the convenience path.
//  3. URL sniffing — the pre-existing behaviour, retained ONLY so that configs
//     written before `protocol` existed keep resolving the way they did.
//
// Step 3 is the wart this function exists to retire. Matching on a substring of
// the URL misroutes real deployments: an Anthropic-compatible gateway at
// anthropic.mycorp.com, or an OpenAI-compatible route whose path happens to
// contain "/openai", both resolve to the wrong dialect. It cannot simply be
// deleted — that would break existing configs — so instead it is demoted to
// last resort, where setting `protocol` explicitly always overrides it.
func resolveEndpoint(m ModelConfig) (baseURL string, proto Protocol) {
	preset, hasPreset := lookupPreset(m.Provider)

	// Protocol.
	switch {
	case m.Protocol != "":
		proto = Protocol(strings.ToLower(strings.TrimSpace(m.Protocol)))
	case hasPreset:
		proto = preset.Protocol
	default:
		proto = sniffProtocol(m.BaseURL)
	}

	// Base URL.
	baseURL = strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
	if baseURL == "" && hasPreset {
		baseURL = preset.BaseURL
	}
	return baseURL, proto
}

// sniffProtocol is the legacy URL-substring heuristic, preserved verbatim in
// behaviour so that a config relying on it before `protocol` existed resolves
// identically. New configs should set `protocol` (or use a preset provider
// name) and never reach this.
func sniffProtocol(baseURL string) Protocol {
	if strings.Contains(baseURL, "api.anthropic.com") && !strings.Contains(baseURL, "/openai") {
		return ProtocolAnthropic
	}
	return ProtocolOpenAI
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
	if knownProtocols[Protocol(strings.ToLower(raw))] {
		return nil
	}
	names := make([]string, 0, len(knownProtocols))
	for p := range knownProtocols {
		names = append(names, string(p))
	}
	sort.Strings(names)
	return fmt.Errorf("config: %s.protocol %q is not a wire dialect QueryForge implements (available: %s). "+
		"Most providers speak %q — including every OpenAI-compatible endpoint, local servers, and aggregators. "+
		"Leave protocol unset to infer it from the provider name",
		where, raw, strings.Join(names, ", "), ProtocolOpenAI)
}

// noEndpointError explains a model block that names nowhere to send the request.
// It is the error a user is most likely to meet first, so it names both fixes.
func noEndpointError(m ModelConfig) error {
	if p := strings.TrimSpace(m.Provider); p != "" {
		return fmt.Errorf("provider: %q has no built-in endpoint, so model.baseURL is required. "+
			"Either set baseURL to the provider's OpenAI-compatible root (e.g. https://api.example.com/v1), "+
			"or use one of the providers with a built-in endpoint: %s",
			p, strings.Join(KnownProviders(), ", "))
	}
	return fmt.Errorf("provider: no endpoint configured — set model.baseURL to an OpenAI-compatible root, "+
		"or set model.provider to one with a built-in endpoint: %s", strings.Join(KnownProviders(), ", "))
}
