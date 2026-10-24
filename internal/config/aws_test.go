package config_test

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	. "github.com/awsaman-ai/queryforge/internal/config"
)

// cfgWithModel wraps a model block in the smallest config that loads.
func cfgWithModel(model string) string {
	return `{"entity":"Order","fields":[{"name":"id","type":"string"}],"model":` + model + `}`
}

func TestAWSBlockLoadsInBothSpellings(t *testing.T) {
	good := map[string]string{
		"nothing but the provider":        `{"provider":"bedrock","model":"us.anthropic.claude-sonnet-4-6"}`,
		"inline bearer":                   `{"provider":"bedrock","model":"m","aws":{"region":"us-east-1","bearerToken":"abc123"}}`,
		"bearer by env":                   `{"provider":"bedrock","model":"m","aws":{"region":"us-east-1","bearerTokenEnv":"MY_BEDROCK_KEY"}}`,
		"inline IAM keys":                 `{"provider":"bedrock","model":"m","aws":{"accessKeyId":"AKIDEXAMPLE","secretAccessKey":"s3cr3t","sessionToken":"tok"}}`,
		"IAM keys by env":                 `{"provider":"bedrock","model":"m","aws":{"accessKeyIdEnv":"A","secretAccessKeyEnv":"B","sessionTokenEnv":"C"}}`,
		"mixed: id inline, secret by env": `{"provider":"bedrock","model":"m","aws":{"accessKeyId":"AKIDEXAMPLE","secretAccessKeyEnv":"B"}}`,
		"govcloud and china regions":      `{"provider":"bedrock","model":"m","aws":{"region":"us-gov-west-1"}}`,
	}
	for name, m := range good {
		if _, err := ParseConfig([]byte(cfgWithModel(m))); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAWSBlockRejections(t *testing.T) {
	bad := []struct{ name, model, want string }{
		{"both spellings of one credential",
			`{"provider":"bedrock","aws":{"bearerToken":"x","bearerTokenEnv":"Y"}}`, "both bearerToken and bearerTokenEnv"},
		{"bearer and IAM keys together",
			`{"provider":"bedrock","aws":{"bearerToken":"x","accessKeyId":"A","secretAccessKey":"S"}}`, "Pick one authentication mode"},
		{"half a key pair",
			`{"provider":"bedrock","aws":{"accessKeyId":"AKIDEXAMPLE"}}`, "only one of accessKeyId and secretAccessKey"},
		{"an access key id pasted into an *Env field",
			`{"provider":"bedrock","aws":{"accessKeyIdEnv":"AKIAIOSFODNN7EXAMPLE","secretAccessKeyEnv":"S"}}`, "looks like an AWS access key id"},
		{"an invalid env var name",
			`{"provider":"bedrock","aws":{"bearerTokenEnv":"not a name!"}}`, "not a valid environment variable name"},
		{"a malformed region",
			`{"provider":"bedrock","aws":{"region":"US EAST"}}`, "not an AWS region name"},
	}
	for _, c := range bad {
		_, err := ParseConfig([]byte(cfgWithModel(c.model)))
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q should contain %q", c.name, err, c.want)
		}
	}
}

// An error must never reprint the value it rejected: for an *Env field that
// value is probably a secret in the wrong slot.
func TestAWSRejectionsNeverEchoTheValue(t *testing.T) {
	for _, m := range []string{
		`{"provider":"bedrock","aws":{"accessKeyIdEnv":"AKIAIOSFODNN7EXAMPLE","secretAccessKeyEnv":"S"}}`,
		`{"provider":"bedrock","aws":{"bearerTokenEnv":"sup3r-secret-token"}}`,
		`{"provider":"bedrock","aws":{"region":"sup3r-secret-token"}}`,
	} {
		_, err := ParseConfig([]byte(cfgWithModel(m)))
		if err == nil {
			t.Fatalf("accepted %s", m)
		}
		for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "sup3r-secret-token"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error echoed %q: %v", leak, err)
			}
		}
	}
}

func TestFallbackModelsAreHeldToTheSameAWSRules(t *testing.T) {
	cfg := `{"entity":"Order","fields":[{"name":"id","type":"string"}],
		"model":{"provider":"bedrock","model":"m"},
		"models":[{"provider":"bedrock","model":"m2","aws":{"accessKeyId":"AKIDEXAMPLE"}}]}`
	_, err := ParseConfig([]byte(cfg))
	if err == nil || !strings.Contains(err.Error(), "models[0].aws") {
		t.Fatalf("err = %v, want it to name models[0].aws", err)
	}
}

func TestInlineSecretsReportsNamesNeverValues(t *testing.T) {
	c, err := ParseConfig([]byte(cfgWithModel(
		`{"provider":"bedrock","model":"m","aws":{"accessKeyId":"AKIDEXAMPLE","secretAccessKey":"topsecretvalue","sessionTokenEnv":"T"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Model.InlineSecrets()
	if strings.Join(got, ",") != "aws.accessKeyId,aws.secretAccessKey" {
		t.Errorf("InlineSecrets = %v (by-reference fields must not be reported)", got)
	}
	adv := strings.Join(c.Advisories(), "\n")
	if !strings.Contains(adv, "aws.secretAccessKey") {
		t.Errorf("advisory should name the field: %q", adv)
	}
	if strings.Contains(adv, "topsecretvalue") {
		t.Error("an advisory must never contain the secret")
	}
}

func TestNoAdvisoryWhenEverythingIsByReference(t *testing.T) {
	c, err := ParseConfig([]byte(cfgWithModel(
		`{"provider":"bedrock","model":"m","aws":{"accessKeyIdEnv":"A","secretAccessKeyEnv":"B"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	if adv := c.Advisories(); len(adv) != 0 {
		t.Errorf("advisories = %v", adv)
	}
}

// The new inline fields must not have loosened the old rule: a key pasted into
// apiKeyEnv is still rejected.
func TestApiKeyEnvStillRejectsPastedKeys(t *testing.T) {
	_, err := ParseConfig([]byte(cfgWithModel(`{"provider":"openai","model":"m","apiKeyEnv":"sk-abc123"}`)))
	if err == nil || !strings.Contains(err.Error(), "apiKeyEnv") {
		t.Errorf("err = %v", err)
	}
}

func TestBedrockIsAKnownProtocol(t *testing.T) {
	if _, err := ParseConfig([]byte(cfgWithModel(`{"provider":"x","baseURL":"https://x","protocol":"bedrock"}`))); err != nil {
		t.Errorf("protocol \"bedrock\" rejected: %v", err)
	}
}

// A config carrying inline AWS credentials must be safe to print, log and
// re-marshal: the secrets are Secret values, which redact themselves. This is
// the guarantee that makes allowing inline credentials tolerable at all.
func TestInlineAWSSecretsNeverLeakThroughFormattingLoggingOrJSON(t *testing.T) {
	const bearer, secret, session = "ABSKtopsecretbearer123", "topsecretaccesskey456", "topsecretsessiontoken789"
	c, err := ParseConfig([]byte(cfgWithModel(
		`{"provider":"bedrock","model":"m","aws":{"accessKeyId":"AKIDEXAMPLE","secretAccessKey":"` + secret +
			`","sessionToken":"` + session + `"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	c2, err := ParseConfig([]byte(cfgWithModel(`{"provider":"bedrock","model":"m","aws":{"bearerToken":"` + bearer + `"}}`)))
	if err != nil {
		t.Fatal(err)
	}

	var logbuf strings.Builder
	slog.New(slog.NewTextHandler(&logbuf, nil)).Info("loaded", "aws", c.Model.AWS, "token", c2.Model.AWS.BearerToken)
	out := map[string]string{
		"%v":   fmt.Sprintf("%v %v", c.Model.AWS, c2.Model.AWS),
		"%+v":  fmt.Sprintf("%+v %+v", c.Model, c2.Model),
		"%#v":  fmt.Sprintf("%#v %#v", c.Model.AWS, c2.Model.AWS),
		"slog": logbuf.String(),
	}
	for _, cfg := range []*Config{c, c2} {
		b, err := json.Marshal(cfg.Model)
		if err != nil {
			t.Fatal(err)
		}
		out["json"] += string(b)
	}
	for how, text := range out {
		for _, leak := range []string{bearer, secret, session} {
			if strings.Contains(text, leak) {
				t.Errorf("%s leaked %q: %s", how, leak, text)
			}
		}
	}
	// The identifier is not a secret and stays readable, so a log line is still useful.
	if !strings.Contains(fmt.Sprintf("%+v", *c.Model.AWS), "AKIDEXAMPLE") {
		t.Error("the access key id is an identifier and should remain visible")
	}
}
