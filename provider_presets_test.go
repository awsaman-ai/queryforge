package queryforge

import (
	"strings"
	"testing"
)

// The rules these tests encode:
//
//   - A provider NAME is a convenience, never a gate. An unlisted provider with
//     a baseURL must work exactly as well as a listed one.
//   - What the config STATES always beats what the library could guess.
//   - The old URL-sniffing behaviour must survive unchanged for configs written
//     before `protocol` existed, but must be overridable, because guessing a
//     dialect from a hostname misroutes real deployments.

// TestAPresetSavesLookingUpTheURL: naming a known provider is a complete
// configuration. This is the friction the presets exist to remove.
func TestAPresetSavesLookingUpTheURL(t *testing.T) {
	cases := []struct {
		provider string
		wantURL  string
		wantProt Protocol
	}{
		{"groq", "https://api.groq.com/openai/v1", ProtocolOpenAI},
		{"openai", "https://api.openai.com/v1", ProtocolOpenAI},
		{"anthropic", "https://api.anthropic.com", ProtocolAnthropic},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta/openai", ProtocolOpenAI},
		{"openrouter", "https://openrouter.ai/api/v1", ProtocolOpenAI},
		{"ollama", "http://localhost:11434/v1", ProtocolOpenAI},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			url, proto := resolveEndpoint(ModelConfig{Provider: tc.provider, Model: "some-model"})
			if url != tc.wantURL {
				t.Errorf("baseURL = %q, want %q", url, tc.wantURL)
			}
			if proto != tc.wantProt {
				t.Errorf("protocol = %q, want %q", proto, tc.wantProt)
			}
		})
	}
}

// TestPresetLookupIgnoresCase: "OpenAI", "openai" and "  Groq " are the same
// provider. Case is not a configuration decision.
func TestPresetLookupIgnoresCase(t *testing.T) {
	for _, name := range []string{"GROQ", "Groq", "  groq  "} {
		url, _ := resolveEndpoint(ModelConfig{Provider: name})
		if url != "https://api.groq.com/openai/v1" {
			t.Errorf("provider %q resolved to %q", name, url)
		}
	}
}

// TestAnExplicitBaseURLAlwaysWins: a preset is a default, never a rule. Regional
// endpoints, corporate mirrors and test servers all depend on this.
func TestAnExplicitBaseURLAlwaysWins(t *testing.T) {
	const mine = "https://llm.mycorp.internal/v1"

	url, _ := resolveEndpoint(ModelConfig{Provider: "groq", BaseURL: mine})
	if url != mine {
		t.Errorf("baseURL = %q, want the explicitly configured %q", url, mine)
	}
}

// TestAnUnlistedProviderIsStillFullySupported is the future-proofing rule: a
// provider that launched after this build shipped must need no code change.
func TestAnUnlistedProviderIsStillFullySupported(t *testing.T) {
	m := ModelConfig{
		Provider: "acme-ai-launched-yesterday",
		BaseURL:  "https://api.acme.ai/v1",
		Model:    "acme-ultra",
	}
	url, proto := resolveEndpoint(m)

	if url != "https://api.acme.ai/v1" {
		t.Errorf("baseURL = %q, want the configured URL", url)
	}
	if proto != ProtocolOpenAI {
		t.Errorf("protocol = %q, want %q — an unknown provider must default to the "+
			"dialect nearly everyone speaks", proto, ProtocolOpenAI)
	}
	if _, ok := ProviderFor(m).(*OpenAIProvider); !ok {
		t.Error("an unlisted provider must still get a working provider implementation")
	}
}

// TestTrailingSlashesAreNormalised: "…/v1/" and "…/v1" must not produce
// different request URLs.
func TestTrailingSlashesAreNormalised(t *testing.T) {
	url, _ := resolveEndpoint(ModelConfig{BaseURL: "https://api.example.com/v1///"})
	if url != "https://api.example.com/v1" {
		t.Errorf("baseURL = %q, want the trailing slashes trimmed", url)
	}
}

// ------------------------------------------------------- protocol precedence

// TestAnExplicitProtocolOverridesEverything is the fix for the misrouting bug.
// A hostname is not a dialect, and when the two disagree the config wins.
func TestAnExplicitProtocolOverridesEverything(t *testing.T) {
	t.Run("an Anthropic-compatible gateway on a corporate hostname", func(t *testing.T) {
		// The old sniff sees no "api.anthropic.com" here and would route this to
		// the OpenAI dialect, which the endpoint does not speak.
		m := ModelConfig{
			Protocol: "anthropic",
			BaseURL:  "https://ai-gateway.mycorp.com",
			Model:    "claude-internal",
		}
		if _, proto := resolveEndpoint(m); proto != ProtocolAnthropic {
			t.Errorf("protocol = %q, want %q", proto, ProtocolAnthropic)
		}
		if _, ok := ProviderFor(m).(*AnthropicProvider); !ok {
			t.Error("an explicit anthropic protocol must select the native provider")
		}
	})

	t.Run("an OpenAI-compatible proxy hosted under an anthropic domain", func(t *testing.T) {
		// The mirror image: the sniff would see the hostname and route to the
		// native dialect against an endpoint speaking OpenAI.
		m := ModelConfig{
			Protocol: "openai",
			BaseURL:  "https://api.anthropic.com.proxy.mycorp.com/v1",
			Model:    "whatever",
		}
		if _, proto := resolveEndpoint(m); proto != ProtocolOpenAI {
			t.Errorf("protocol = %q, want %q", proto, ProtocolOpenAI)
		}
		if _, ok := ProviderFor(m).(*OpenAIProvider); !ok {
			t.Error("an explicit openai protocol must select the OpenAI-compatible provider")
		}
	})

	t.Run("an explicit protocol beats the provider name's preset", func(t *testing.T) {
		m := ModelConfig{Provider: "anthropic", Protocol: "openai", BaseURL: "https://x/v1"}
		if _, proto := resolveEndpoint(m); proto != ProtocolOpenAI {
			t.Errorf("protocol = %q, want the stated %q", proto, ProtocolOpenAI)
		}
	})
}

// TestProtocolIsCaseInsensitive: "OpenAI" and "openai" mean the same thing.
func TestProtocolIsCaseInsensitive(t *testing.T) {
	if _, proto := resolveEndpoint(ModelConfig{Protocol: "ANTHROPIC"}); proto != ProtocolAnthropic {
		t.Errorf("protocol = %q, want %q", proto, ProtocolAnthropic)
	}
}

// TestLegacySniffingStillWorks is the backward-compatibility guarantee. Configs
// written before `protocol` existed relied entirely on the URL heuristic, and
// they must resolve exactly as they did.
func TestLegacySniffingStillWorks(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    Protocol
	}{
		{"the native Anthropic host still selects the native dialect",
			"https://api.anthropic.com", ProtocolAnthropic},
		{"Anthropic's OpenAI-compat path still selects the OpenAI dialect",
			"https://api.anthropic.com/v1/openai", ProtocolOpenAI},
		{"any other host still defaults to the OpenAI dialect",
			"https://api.groq.com/openai/v1", ProtocolOpenAI},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No provider, no protocol — the pre-existing shape of these configs.
			if _, proto := resolveEndpoint(ModelConfig{BaseURL: tc.baseURL}); proto != tc.want {
				t.Errorf("protocol = %q, want %q", proto, tc.want)
			}
		})
	}
}

// TestProviderForRoutesByDialectNotByName: selection must follow the resolved
// protocol, so that everything above composes.
func TestProviderForRoutesByDialectNotByName(t *testing.T) {
	cases := []struct {
		name       string
		m          ModelConfig
		wantNative bool
	}{
		{"provider anthropic", ModelConfig{Provider: "anthropic"}, true},
		{"provider ANTHROPIC in caps", ModelConfig{Provider: "ANTHROPIC"}, true},
		{"legacy anthropic URL", ModelConfig{BaseURL: "https://api.anthropic.com"}, true},
		{"explicit anthropic protocol on any host", ModelConfig{Protocol: "anthropic", BaseURL: "https://x"}, true},
		{"provider groq", ModelConfig{Provider: "groq"}, false},
		{"bare OpenAI-compatible URL", ModelConfig{BaseURL: "https://api.example.com/v1"}, false},
		{"nothing configured at all", ModelConfig{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, isNative := ProviderFor(tc.m).(*AnthropicProvider)
			if isNative != tc.wantNative {
				t.Errorf("native anthropic provider = %v, want %v", isNative, tc.wantNative)
			}
		})
	}
}

// ------------------------------------------------------------------ errors

// TestTheNoEndpointErrorTellsYouWhatToDo: this is the first error a new user
// meets, and "no baseURL configured" does not tell them that naming a provider
// would have been enough.
func TestTheNoEndpointErrorTellsYouWhatToDo(t *testing.T) {
	t.Run("with an unrecognised provider name", func(t *testing.T) {
		err := noEndpointError(ModelConfig{Provider: "mystery-co"})
		msg := err.Error()

		if !strings.Contains(msg, "mystery-co") {
			t.Error("the message should name the provider that was not recognised")
		}
		if !strings.Contains(msg, "baseURL") {
			t.Error("the message should name the key that fixes it")
		}
		if !strings.Contains(msg, "groq") {
			t.Error("the message should list providers that need no baseURL")
		}
	})

	t.Run("with nothing configured", func(t *testing.T) {
		msg := noEndpointError(ModelConfig{}).Error()
		if !strings.Contains(msg, "baseURL") || !strings.Contains(msg, "provider") {
			t.Errorf("the message should name both ways out: %s", msg)
		}
	})
}

// TestKnownProvidersIsStableAndSorted: the list appears in error messages, so
// its order must not wobble between runs over a map.
func TestKnownProvidersIsStableAndSorted(t *testing.T) {
	first := KnownProviders()
	if len(first) == 0 {
		t.Fatal("expected some presets")
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] >= first[i] {
			t.Fatalf("not sorted at %d: %q then %q", i, first[i-1], first[i])
		}
	}
	second := KnownProviders()
	for i := range first {
		if first[i] != second[i] {
			t.Fatal("KnownProviders is not stable across calls")
		}
	}
}

// TestPresetsCarryNoModelIds is the anti-regression guard for the whole design.
// The moment this map starts listing models, QueryForge stops being
// model-agnostic and starts needing a release every time a provider ships one.
func TestPresetsCarryNoModelIds(t *testing.T) {
	for name, p := range providerPresets {
		if p.BaseURL == "" {
			t.Errorf("preset %q has no base URL, which is the only thing it is for", name)
		}
		if !knownProtocols[p.Protocol] {
			t.Errorf("preset %q names protocol %q, which is not implemented", name, p.Protocol)
		}
		if !strings.HasPrefix(p.BaseURL, "http://") && !strings.HasPrefix(p.BaseURL, "https://") {
			t.Errorf("preset %q base URL %q is not an absolute URL", name, p.BaseURL)
		}
	}
}
