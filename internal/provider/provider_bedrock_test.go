package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awsaman-ai/queryforge/internal/config"
	"github.com/awsaman-ai/queryforge/internal/failure"
	"github.com/awsaman-ai/queryforge/internal/observe"
)

// env builds the getenv a test hands the constructor, so no test depends on, or
// disturbs, the real process environment.
func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

const okReply = `{"output":{"message":{"role":"assistant","content":[{"text":"{\"ok\":true}"}]}},` +
	`"stopReason":"end_turn","usage":{"inputTokens":11,"outputTokens":7,"totalTokens":18}}`

// bedrockServer stands in for bedrock-runtime and records what it was sent.
type seen struct {
	path, rawPath, auth, token, ctype string
	body                              string
}

func bedrockServer(t *testing.T, s *seen, handler func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.path, s.rawPath = r.URL.Path, r.URL.EscapedPath()
		s.auth, s.token = r.Header.Get("Authorization"), r.Header.Get("X-Amz-Security-Token")
		s.ctype, s.body = r.Header.Get("Content-Type"), string(b)
		handler(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func replyOK(w http.ResponseWriter) { io.WriteString(w, okReply) }

func TestBedrockBearerModeSendsTheKeyAndTheConverseShape(t *testing.T) {
	var s seen
	srv := bedrockServer(t, &s, replyOK)
	m := config.ModelConfig{
		Provider: "bedrock", Model: "us.anthropic.claude-sonnet-4-6", BaseURL: srv.URL,
		AWS: &config.AWSConfig{BearerToken: "bearer-secret-value"},
	}
	p := newBedrockProvider(m, env(nil))
	out, err := p.Complete(context.Background(), "sys prompt", "user text")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Errorf("out = %q", out)
	}
	if s.auth != "Bearer bearer-secret-value" {
		t.Errorf("Authorization = %q", s.auth)
	}
	if s.path != "/model/us.anthropic.claude-sonnet-4-6/converse" {
		t.Errorf("path = %q", s.path)
	}
	for _, want := range []string{`"system":[{"text":"sys prompt"}]`, `"content":[{"text":"user text"}]`, `"maxTokens":1024`} {
		if !strings.Contains(s.body, want) {
			t.Errorf("body missing %s:\n%s", want, s.body)
		}
	}
	if strings.Contains(s.body, "temperature") {
		t.Errorf("temperature must be omitted when unset (some models reject it): %s", s.body)
	}
}

func TestBedrockTemperatureIsSentOnlyWhenPositive(t *testing.T) {
	var s seen
	srv := bedrockServer(t, &s, replyOK)
	m := config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: srv.URL, Temperature: 0.3,
		AWS: &config.AWSConfig{BearerToken: "bearer-secret-value"}}
	if _, err := newBedrockProvider(m, env(nil)).Complete(context.Background(), "", "u"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.body, `"temperature":0.3`) {
		t.Errorf("body = %s", s.body)
	}
	if strings.Contains(s.body, `"system"`) {
		t.Errorf("an empty system prompt must be omitted: %s", s.body)
	}
}

func TestBedrockSigV4ModeSignsAndCarriesTheSessionToken(t *testing.T) {
	var s seen
	srv := bedrockServer(t, &s, replyOK)
	m := config.ModelConfig{Provider: "bedrock", Model: "anthropic.claude-3-5-sonnet-20241022-v2:0", BaseURL: srv.URL,
		AWS: &config.AWSConfig{Region: "us-west-2", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secretsecretsecret", SessionToken: "session-token-xyz"}}
	p := newBedrockProvider(m, env(nil))
	p.now = func() time.Time { return time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC) }
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20261024/us-west-2/bedrock/aws4_request") {
		t.Errorf("Authorization = %q", s.auth)
	}
	if !strings.Contains(s.auth, "x-amz-security-token") || s.token != "session-token-xyz" {
		t.Errorf("session token not signed/sent: auth=%q token=%q", s.auth, s.token)
	}
	if strings.Contains(s.auth, "secretsecretsecret") {
		t.Error("the secret key must never appear in a header")
	}
	// The colon in the model id must travel percent-encoded.
	if s.rawPath != "/model/anthropic.claude-3-5-sonnet-20241022-v2%3A0/converse" {
		t.Errorf("wire path = %q", s.rawPath)
	}
}

func TestBedrockDerivesTheEndpointFromTheRegion(t *testing.T) {
	cases := map[string]string{
		"us-east-1":     "https://bedrock-runtime.us-east-1.amazonaws.com",
		"eu-central-1":  "https://bedrock-runtime.eu-central-1.amazonaws.com",
		"us-gov-west-1": "https://bedrock-runtime.us-gov-west-1.amazonaws.com",
		"cn-north-1":    "https://bedrock-runtime.cn-north-1.amazonaws.com.cn",
	}
	for region, want := range cases {
		p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m",
			AWS: &config.AWSConfig{Region: region, BearerToken: "bearer-secret-value"}}, env(nil))
		if p.BaseURL != want {
			t.Errorf("%s: BaseURL = %q, want %q", region, p.BaseURL, want)
		}
	}
	// An explicit baseURL (VPC / FIPS endpoint) wins over the region.
	p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: "https://vpce.example/",
		AWS: &config.AWSConfig{Region: "us-east-1", BearerToken: "bearer-secret-value"}}, env(nil))
	if p.BaseURL != "https://vpce.example" {
		t.Errorf("explicit baseURL lost: %q", p.BaseURL)
	}
}

func TestBedrockCredentialResolution(t *testing.T) {
	type want struct {
		bearer, id, secret, session string
		err                         string // substring; empty = success
	}
	cases := []struct {
		name string
		aws  *config.AWSConfig
		env  map[string]string
		want want
	}{
		{"bare config reads the ambient bearer key", nil,
			map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "tok"}, want{bearer: "tok"}},
		{"ambient bearer beats ambient IAM keys (AWS's own precedence)", nil,
			map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "tok", "AWS_ACCESS_KEY_ID": "A", "AWS_SECRET_ACCESS_KEY": "S"}, want{bearer: "tok"}},
		{"ambient IAM keys with a session token", nil,
			map[string]string{"AWS_ACCESS_KEY_ID": "A", "AWS_SECRET_ACCESS_KEY": "S", "AWS_SESSION_TOKEN": "T"}, want{id: "A", secret: "S", session: "T"}},
		{"inline bearer", &config.AWSConfig{BearerToken: "inline"}, nil, want{bearer: "inline"}},
		{"bearer by env reference", &config.AWSConfig{BearerTokenEnv: "MY_KEY"},
			map[string]string{"MY_KEY": "viaenv"}, want{bearer: "viaenv"}},
		{"inline IAM keys", &config.AWSConfig{AccessKeyID: "A", SecretAccessKey: "S"}, nil, want{id: "A", secret: "S"}},
		{"IAM keys by env reference, incl. session", &config.AWSConfig{AccessKeyIDEnv: "K", SecretAccessKeyEnv: "X", SessionTokenEnv: "T"},
			map[string]string{"K": "A", "X": "S", "T": "TOK"}, want{id: "A", secret: "S", session: "TOK"}},
		{"configured bearer beats ambient IAM keys", &config.AWSConfig{BearerToken: "inline"},
			map[string]string{"AWS_ACCESS_KEY_ID": "A", "AWS_SECRET_ACCESS_KEY": "S"}, want{bearer: "inline"}},
		{"inline keys do not inherit an ambient session token that belongs to another key",
			&config.AWSConfig{AccessKeyID: "A", SecretAccessKey: "S"},
			map[string]string{"AWS_SESSION_TOKEN": "STRAY"}, want{id: "A", secret: "S", session: ""}},
		{"env-referenced keys do pick up the ambient session token",
			&config.AWSConfig{AccessKeyIDEnv: "K", SecretAccessKeyEnv: "X"},
			map[string]string{"K": "A", "X": "S", "AWS_SESSION_TOKEN": "T"}, want{id: "A", secret: "S", session: "T"}},
		{"a named env var that is empty is an error naming the variable",
			&config.AWSConfig{BearerTokenEnv: "MY_KEY"}, nil, want{err: "MY_KEY"}},
		{"nothing anywhere", nil, nil, want{err: "no AWS credentials"}},
		{"half an ambient key pair is not credentials", nil,
			map[string]string{"AWS_ACCESS_KEY_ID": "A"}, want{err: "no AWS credentials"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := c.aws
			if a == nil {
				a = &config.AWSConfig{}
			}
			bearer, creds, err := resolveAWSAuth(a, env(c.env))
			if c.want.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.want.err) {
					t.Fatalf("err = %v, want it to contain %q", err, c.want.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if bearer != c.want.bearer || creds.AccessKeyID != c.want.id ||
				creds.SecretAccessKey != c.want.secret || creds.SessionToken != c.want.session {
				t.Errorf("got bearer=%q id=%q secret=%q session=%q; want %+v", bearer, creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, c.want)
			}
		})
	}
}

func TestBedrockRegionResolution(t *testing.T) {
	get := func(a config.AWSConfig, e map[string]string) (string, error) { return resolveAWSRegion(&a, env(e)) }
	if r, _ := get(config.AWSConfig{Region: "eu-west-1"}, map[string]string{"AWS_REGION": "us-east-1"}); r != "eu-west-1" {
		t.Errorf("config must beat environment, got %q", r)
	}
	if r, _ := get(config.AWSConfig{}, map[string]string{"AWS_REGION": "us-east-2", "AWS_DEFAULT_REGION": "us-west-1"}); r != "us-east-2" {
		t.Errorf("AWS_REGION must beat AWS_DEFAULT_REGION, got %q", r)
	}
	if r, _ := get(config.AWSConfig{}, map[string]string{"AWS_DEFAULT_REGION": "us-west-1"}); r != "us-west-1" {
		t.Errorf("AWS_DEFAULT_REGION fallback, got %q", r)
	}
	if _, err := get(config.AWSConfig{}, nil); err == nil || !strings.Contains(err.Error(), "AWS_REGION") {
		t.Errorf("a missing region must say where to set it, got %v", err)
	}
	if _, err := get(config.AWSConfig{}, map[string]string{"AWS_REGION": "bogus_value_123"}); err == nil {
		t.Error("a malformed region must be rejected")
	} else if strings.Contains(err.Error(), "bogus_value_123") {
		t.Error("the error must not echo the environment value")
	}
}

func TestBedrockMissingCredentialsFailAsAuthWithoutAnyRequest(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: srv.URL}, env(nil))
	p.MaxRetries = 3
	_, err := p.Complete(context.Background(), "s", "u")
	pe, ok := asProviderError(err)
	if !ok || pe.Kind != failure.KindAuth {
		t.Fatalf("want an AUTH ProviderError, got %v", err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Error("no request may be sent without credentials, and an auth failure must not retry")
	}
}

func TestBedrockRequiresAModel(t *testing.T) {
	p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", BaseURL: "http://x",
		AWS: &config.AWSConfig{BearerToken: "bearer-secret-value"}}, env(nil))
	_, err := p.Complete(context.Background(), "s", "u")
	if pe, ok := asProviderError(err); !ok || pe.Kind != failure.KindInvalidRequest || !strings.Contains(pe.Detail, "model.model") {
		t.Errorf("err = %v", err)
	}
}

func TestBedrockErrorClassification(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		errType string
		body    string
		want    failure.ProviderErrorKind
		hint    string
	}{
		{"throttling retries", 429, "ThrottlingException:http://internal.amazon.com/coral/", `{"message":"Too many requests"}`, failure.KindRateLimit, ""},
		{"model not ready retries", 429, "ModelNotReadyException", `{"message":"warming up"}`, failure.KindRateLimit, ""},
		{"service quota is NOT retryable even on a 400", 400, "ServiceQuotaExceededException", `{"message":"limit"}`, failure.KindQuota, "service quota"},
		{"access denied", 403, "AccessDeniedException", `{"message":"You don't have access to the model"}`, failure.KindAuth, "model access"},
		{"expired token", 403, "ExpiredTokenException", `{"message":"The security token included in the request is expired"}`, failure.KindAuth, "expired"},
		{"bad signature", 403, "InvalidSignatureException", `{"message":"nope"}`, failure.KindAuth, ""},
		{"unknown model", 404, "ResourceNotFoundException", `{"message":"no such model"}`, failure.KindModelNotFound, "region"},
		{"validation", 400, "ValidationException", `{"message":"bad field"}`, failure.KindInvalidRequest, ""},
		{"profile required", 400, "ValidationException", `{"message":"Invocation of model ID x with on-demand throughput isn't supported. Retry your request with the ID or ARN of an inference profile"}`, failure.KindInvalidRequest, "inference profile"},
		{"model timeout", 408, "ModelTimeoutException", `{"message":"slow"}`, failure.KindTimeout, ""},
		{"internal", 500, "InternalServerException", `{"message":"oops"}`, failure.KindUnavailable, ""},
		{"unavailable", 503, "ServiceUnavailableException", `{"message":"down"}`, failure.KindUnavailable, ""},
		{"model error", 424, "ModelErrorException", `{"message":"model failed"}`, failure.KindUnavailable, ""},
		{"no header falls back to status", 502, "", `bad gateway`, failure.KindUnavailable, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.errType != "" {
					w.Header().Set("x-amzn-ErrorType", c.errType)
				}
				w.WriteHeader(c.status)
				io.WriteString(w, c.body)
			}))
			defer srv.Close()
			p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: srv.URL,
				AWS: &config.AWSConfig{Region: "us-east-1", BearerToken: "bearer-secret-value"}}, env(nil))
			p.MaxRetries = 0
			_, err := p.Complete(context.Background(), "s", "u")
			pe, ok := asProviderError(err)
			if !ok {
				t.Fatalf("not a ProviderError: %v", err)
			}
			if pe.Kind != c.want {
				t.Errorf("kind = %s, want %s", pe.Kind, c.want)
			}
			if pe.Status != c.status {
				t.Errorf("status = %d", pe.Status)
			}
			if c.hint != "" && !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.hint)) {
				t.Errorf("error should hint at %q: %v", c.hint, err)
			}
		})
	}
}

func TestBedrockRetriesAThrottleThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("x-amzn-ErrorType", "ThrottlingException")
			w.WriteHeader(429)
			io.WriteString(w, `{"message":"slow down"}`)
			return
		}
		io.WriteString(w, okReply)
	}))
	defer srv.Close()
	p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: srv.URL,
		AWS: &config.AWSConfig{BearerToken: "bearer-secret-value"}}, env(nil))
	p.RetryBackoff = time.Millisecond
	p.jitter = func(d time.Duration) time.Duration { return d }
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

func TestBedrockTruncationAndGuardrailAreNotRetried(t *testing.T) {
	for _, stop := range []string{"max_tokens", "guardrail_intervened", "content_filtered"} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			io.WriteString(w, `{"output":{"message":{"content":[{"text":"partial"}]}},"stopReason":"`+stop+`","usage":{"inputTokens":1,"outputTokens":2}}`)
		}))
		p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: srv.URL,
			AWS: &config.AWSConfig{BearerToken: "bearer-secret-value"}}, env(nil))
		p.MaxRetries = 2
		_, err := p.Complete(context.Background(), "s", "u")
		srv.Close()
		if pe, ok := asProviderError(err); !ok || pe.Kind != failure.KindBadResponse {
			t.Errorf("%s: err = %v", stop, err)
		}
		if calls != 1 {
			t.Errorf("%s: retried %d times", stop, calls)
		}
		if stop == "max_tokens" && !strings.Contains(err.Error(), "maxTokens") {
			t.Errorf("truncation should name the knob: %v", err)
		}
	}
}

// Every credential must be scrubbed from error text, including when a gateway
// echoes the request back — the case redaction exists for.
func TestBedrockErrorsNeverContainCredentials(t *testing.T) {
	const bearer, secret, session = "ABSKbearerbearerbearer", "SECRETKEYSECRETKEYSECRET", "SESSIONTOKENSESSIONTOKEN"
	// Each mode is only responsible for the credentials it actually holds.
	for name, tc := range map[string]struct {
		aws     *config.AWSConfig
		secrets []string
	}{
		"bearer": {&config.AWSConfig{BearerToken: bearer}, []string{bearer}},
		"sigv4":  {&config.AWSConfig{Region: "us-east-1", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: secret, SessionToken: session}, []string{secret, session}},
	} {
		aws := tc.aws
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(500)
				io.WriteString(w, "echo: "+r.Header.Get("Authorization")+" "+bearer+" "+secret+" "+session)
			}))
			defer srv.Close()
			p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: srv.URL, AWS: aws}, env(nil))
			p.MaxRetries = 0
			_, err := p.Complete(context.Background(), "s", "u")
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, s := range tc.secrets {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error leaked %q: %v", s, err)
				}
			}
		})
	}
}

func TestBedrockSurfacesTokenUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, okReply) }))
	defer srv.Close()
	var events []observe.Event
	p := newBedrockProvider(config.ModelConfig{Provider: "bedrock", Model: "m", BaseURL: srv.URL,
		AWS: &config.AWSConfig{BearerToken: "bearer-secret-value"}}, env(nil))
	p.SetObserver(func(_ context.Context, e observe.Event) { events = append(events, e) })
	if _, err := p.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want one model-call event per round trip, got %d", len(events))
	}
	ev := events[0]
	if ev.PromptTokens != 11 || ev.CompletionTokens != 7 || ev.TotalTokens != 18 || ev.FinishReason != "end_turn" || ev.Provider != "bedrock" {
		t.Errorf("event = %+v", ev)
	}
}

func TestProviderForRoutesBedrock(t *testing.T) {
	if _, ok := ProviderFor(config.ModelConfig{Provider: "bedrock", Model: "m"}).(*BedrockProvider); !ok {
		t.Error("provider \"bedrock\" must route to the Bedrock provider")
	}
	if _, ok := ProviderFor(config.ModelConfig{Provider: "acme", Protocol: "bedrock", Model: "m"}).(*BedrockProvider); !ok {
		t.Error("an explicit protocol \"bedrock\" must route to the Bedrock provider")
	}
}
