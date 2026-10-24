package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// A distinctive, plausible-looking key, so a leak is unmistakable in any output.
const testKey = "AIzaSyLEAKCANARY-0123456789abcdef"

func cfgWith(model string) string {
	return `{"entity":"Order","model":` + model + `,"fields":[{"name":"n","type":"number"}]}`
}

func TestAPIKeyParsesAndIsRevealable(t *testing.T) {
	c, err := ParseConfig([]byte(cfgWith(`{"provider":"gemini","model":"m","apiKey":"` + testKey + `"}`)))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := c.Model.APIKey.Reveal(); got != testKey {
		t.Fatalf("Reveal = %q", got)
	}
}

func TestAPIKeyAcceptedInFallbackChain(t *testing.T) {
	c, err := ParseConfig([]byte(`{"entity":"O","model":{"provider":"gemini","apiKeyEnv":"A_B"},
	  "models":[{"provider":"groq","model":"x","apiKey":"` + testKey + `"}],"fields":[{"name":"n","type":"number"}]}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if c.Models[0].APIKey.Reveal() != testKey {
		t.Fatal("fallback entry lost its key")
	}
}

// The worst-case inputs: every one must fail at load, name the field, and never
// echo the key.
func TestAPIKeyRejections(t *testing.T) {
	cases := []struct {
		name, model, wantSub string
	}{
		{"both set", `{"apiKey":"` + testKey + `","apiKeyEnv":"QF_API_KEY"}`, "both apiKey and apiKeyEnv"},
		{"blank", `{"apiKey":"   "}`, "apiKey is blank"},
		{"inner space", `{"apiKey":"AIza first second"}`, "whitespace or a control character"},
		{"newline inside", `{"apiKey":"AIzaabc\ndef"}`, "whitespace or a control character"},
		{"tab inside", `{"apiKey":"AIza\tabc"}`, "whitespace or a control character"},
		{"nul inside", `{"apiKey":"AIza\u0000abc"}`, "whitespace or a control character"},
		{"placeholder", `{"apiKey":"<paste your key here>"}`, "whitespace or a control character"},
		{"too long", `{"apiKey":"` + strings.Repeat("a", 5000) + `"}`, "implausibly long"},
		{"wrong type", `{"apiKey":12345}`, ""},
		{"object", `{"apiKey":{"k":"v"}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(cfgWith(tc.model)))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q lacks %q", err, tc.wantSub)
			}
			for _, secret := range []string{testKey, "AIza first", "AIzaabc"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks the key: %q", err)
				}
			}
		})
	}
}

func TestAPIKeyRejectionNamesTheFallbackIndex(t *testing.T) {
	_, err := ParseConfig([]byte(`{"entity":"O","model":{},"models":[{"provider":"g"},
	  {"provider":"x","apiKey":"bad key","baseURL":"http://h"}],"fields":[{"name":"n","type":"number"}]}`))
	if err == nil || !strings.Contains(err.Error(), "models[1].apiKey") {
		t.Fatalf("want models[1].apiKey error, got %v", err)
	}
}

// A key pasted into apiKeyEnv is still rejected, and the message now points at
// the supported field instead of saying keys may never be in a config.
func TestKeyInAPIKeyEnvStillRejectedWithPointerToAPIKey(t *testing.T) {
	_, err := ParseConfig([]byte(cfgWith(`{"apiKeyEnv":"` + testKey + `"}`)))
	if err == nil || !strings.Contains(err.Error(), "apiKey field instead") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("error leaks the key")
	}
}

func TestSecretNeverPrintsMarshalsOrLogs(t *testing.T) {
	m := ModelConfig{Provider: "gemini", Model: "m", APIKey: Secret(testKey)}
	c := &Config{Entity: "Order", Model: m}

	outputs := map[string]string{
		"%v":        fmt.Sprintf("%v", m),
		"%+v":       fmt.Sprintf("%+v", m),
		"%#v":       fmt.Sprintf("%#v", m),
		"%s":        fmt.Sprint(m.APIKey),
		"%v config": fmt.Sprintf("%+v", c),
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	outputs["json"] = string(b)
	pretty, _ := json.MarshalIndent(m, "", "  ")
	outputs["json indent"] = string(pretty)

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "model", m, "key", m.APIKey)
	outputs["slog json"] = buf.String()
	buf.Reset()
	slog.New(slog.NewTextHandler(&buf, nil)).Info("cfg", "model", m, "key", m.APIKey)
	outputs["slog text"] = buf.String()

	for name, out := range outputs {
		if strings.Contains(out, testKey) || strings.Contains(out, "LEAKCANARY") {
			t.Errorf("%s leaks the key: %s", name, out)
		}
	}
	if !strings.Contains(outputs["json"], `"apiKey":"[REDACTED]"`) {
		t.Errorf("json should show the redaction marker, got %s", outputs["json"])
	}
}

func TestEmptySecretIsOmittedAndPrintsEmpty(t *testing.T) {
	b, _ := json.Marshal(ModelConfig{Provider: "x"})
	if strings.Contains(string(b), "apiKey") {
		t.Errorf("an unset key must be omitted: %s", b)
	}
	if Secret("").String() != "" {
		t.Error("empty secret should print empty, not the marker")
	}
}

func TestResolveAPIKey(t *testing.T) {
	env := map[string]string{"QF_API_KEY": "from-env\n", "EMPTY": ""}
	get := func(k string) string { return env[k] }
	cases := []struct {
		name string
		m    ModelConfig
		want string
	}{
		{"inline wins", ModelConfig{APIKey: "inline"}, "inline"},
		{"inline trimmed", ModelConfig{APIKey: "  inline\r\n"}, "inline"},
		{"env, trimmed (trailing newline from a .env file)", ModelConfig{APIKeyEnv: "QF_API_KEY"}, "from-env"},
		{"inline beats env when both reach the resolver", ModelConfig{APIKey: "inline", APIKeyEnv: "QF_API_KEY"}, "inline"},
		{"env var unset", ModelConfig{APIKeyEnv: "NOPE"}, ""},
		{"env var empty", ModelConfig{APIKeyEnv: "EMPTY"}, ""},
		{"neither (keyless local server)", ModelConfig{}, ""},
	}
	for _, tc := range cases {
		if got := ResolveAPIKey(tc.m, get); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}
