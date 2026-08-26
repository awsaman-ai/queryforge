package provider

import (
	"fmt"
	"sort"
	"strings"

	"github.com/awsaman-ai/queryforge/internal/config"
)

// providerPreset is the small amount of data that turns a provider NAME into a
// reachable endpoint.
type providerPreset struct {
	BaseURL  string          // the endpoint root to use when the config names none
	Protocol config.Protocol // the dialect this provider speaks
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
	"anthropic": {BaseURL: "https://api.anthropic.com", Protocol: config.ProtocolAnthropic},

	// First-party OpenAI.
	"openai": {BaseURL: "https://api.openai.com/v1", Protocol: config.ProtocolOpenAI},

	// OpenAI-compatible hosted providers.
	"gemini":     {BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Protocol: config.ProtocolOpenAI},
	"google":     {BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", Protocol: config.ProtocolOpenAI},
	"groq":       {BaseURL: "https://api.groq.com/openai/v1", Protocol: config.ProtocolOpenAI},
	"deepseek":   {BaseURL: "https://api.deepseek.com/v1", Protocol: config.ProtocolOpenAI},
	"together":   {BaseURL: "https://api.together.xyz/v1", Protocol: config.ProtocolOpenAI},
	"fireworks":  {BaseURL: "https://api.fireworks.ai/inference/v1", Protocol: config.ProtocolOpenAI},
	"mistral":    {BaseURL: "https://api.mistral.ai/v1", Protocol: config.ProtocolOpenAI},
	"cerebras":   {BaseURL: "https://api.cerebras.ai/v1", Protocol: config.ProtocolOpenAI},
	"xai":        {BaseURL: "https://api.x.ai/v1", Protocol: config.ProtocolOpenAI},
	"perplexity": {BaseURL: "https://api.perplexity.ai", Protocol: config.ProtocolOpenAI},
	"nvidia":     {BaseURL: "https://integrate.api.nvidia.com/v1", Protocol: config.ProtocolOpenAI},

	// Aggregator. Optional in every sense: it is one row in this map, nothing
	// depends on it, and removing it would change no behaviour for anyone who
	// does not name it.
	"openrouter": {BaseURL: "https://openrouter.ai/api/v1", Protocol: config.ProtocolOpenAI},

	// Local inference servers. Their defaults are the documented localhost
	// ports, so "provider": "ollama" alone is a working config on a stock
	// install — no key, no URL.
	"ollama":   {BaseURL: "http://localhost:11434/v1", Protocol: config.ProtocolOpenAI},
	"vllm":     {BaseURL: "http://localhost:8000/v1", Protocol: config.ProtocolOpenAI},
	"lmstudio": {BaseURL: "http://localhost:1234/v1", Protocol: config.ProtocolOpenAI},
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
func resolveEndpoint(m config.ModelConfig) (baseURL string, proto config.Protocol) {
	preset, hasPreset := lookupPreset(m.Provider)

	// Protocol.
	switch {
	case m.Protocol != "":
		proto = config.Protocol(strings.ToLower(strings.TrimSpace(m.Protocol)))
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
func sniffProtocol(baseURL string) config.Protocol {
	if strings.Contains(baseURL, "api.anthropic.com") && !strings.Contains(baseURL, "/openai") {
		return config.ProtocolAnthropic
	}
	return config.ProtocolOpenAI
}

// noEndpointError explains a model block that names nowhere to send the request.
// It is the error a user is most likely to meet first, so it names both fixes.
func noEndpointError(m config.ModelConfig) error {
	if p := strings.TrimSpace(m.Provider); p != "" {
		return fmt.Errorf("provider: %q has no built-in endpoint, so model.baseURL is required. "+
			"Either set baseURL to the provider's OpenAI-compatible root (e.g. https://api.example.com/v1), "+
			"or use one of the providers with a built-in endpoint: %s",
			p, strings.Join(KnownProviders(), ", "))
	}
	return fmt.Errorf("provider: no endpoint configured — set model.baseURL to an OpenAI-compatible root, "+
		"or set model.provider to one with a built-in endpoint: %s", strings.Join(KnownProviders(), ", "))
}
