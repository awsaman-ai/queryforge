package queryforge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awsaman-ai/queryforge/internal/testutil"
)

// Credentials written inline must surface as a warning on every translation,
// without ever carrying the value.
func TestInlineAWSCredentialsRaiseAWarningOnTranslate(t *testing.T) {
	cfg := engineTestConfig(t)
	cfg.Model = ModelConfig{Provider: "bedrock", Model: "m",
		AWS: &AWSConfig{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "topsecretvalue", SessionToken: "tok-inline"}}
	e := NewWithProvider(cfg, &StubProvider{Response: testutil.CanonicalAST})
	e.Now = func() time.Time { return testutil.FixedNow }

	res, err := e.Translate(context.Background(), "delivered orders", "sql", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "aws.secretAccessKey") {
		t.Errorf("expected an inline-credentials warning, got %q", joined)
	}
	for _, leak := range []string{"topsecretvalue", "tok-inline"} {
		if strings.Contains(joined, leak) {
			t.Errorf("warning leaked %q", leak)
		}
	}
}

// End to end through the public constructor: New(cfg) with provider "bedrock"
// must reach a Bedrock endpoint, sign or authenticate, and compile the reply.
func TestEngineTranslatesThroughBedrock(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)
		reply, _ := jsonString(testutil.CanonicalAST)
		io.WriteString(w, `{"output":{"message":{"content":[{"text":`+reply+`}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`)
	}))
	defer srv.Close()

	cfg := engineTestConfig(t)
	cfg.Model = ModelConfig{Provider: "bedrock", Model: "us.anthropic.claude-sonnet-4-6", BaseURL: srv.URL,
		AWS: &AWSConfig{Region: "us-east-1", BearerTokenEnv: "QF_TEST_BEDROCK_KEY"}}
	t.Setenv("QF_TEST_BEDROCK_KEY", "bearer-from-the-environment")

	e := New(cfg)
	e.Now = func() time.Time { return testutil.FixedNow }
	res, err := e.Translate(context.Background(), "delivered orders", "sql", nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer bearer-from-the-environment" {
		t.Errorf("Authorization = %q", auth)
	}
	if !strings.HasPrefix(res.Query.SQL, "SELECT * FROM orders WHERE") {
		t.Errorf("SQL = %s", res.Query.SQL)
	}
}

func jsonString(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}
