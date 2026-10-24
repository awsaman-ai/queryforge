package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/awsaman-ai/queryforge/internal/config"
	"github.com/awsaman-ai/queryforge/internal/failure"
	"github.com/awsaman-ai/queryforge/internal/observe"
)

// BedrockProvider calls Amazon Bedrock's Converse API — the model-agnostic
// request shape that fronts every model family Bedrock hosts (Claude, Nova,
// Llama, Mistral, …) — so one provider covers all of them. It uses only the
// standard library, preserving QueryForge's zero-dependency core.
//
// Two authentication modes, chosen by what the config supplies (see AWSConfig):
//
//   - bearer: a Bedrock API key sent as `Authorization: Bearer`.
//   - sigv4: IAM access keys (+ optional session token) used to sign the request.
//
// What it does not do is discover credentials: no profiles, SSO, AssumeRole,
// web identity or instance metadata. It uses what the config and the standard
// AWS_* environment variables give it. A deployment on a role-based identity
// resolves temporary credentials itself and hands them over.
type BedrockProvider struct {
	Region     string // AWS region, e.g. us-east-1
	BaseURL    string // endpoint root, no trailing slash; derived from Region unless overridden
	Model      string // model id, inference-profile id, or inference-profile ARN
	MaxTokens  int    // response cap; Converse treats it as inferenceConfig.maxTokens
	HTTPClient *http.Client

	// Temperature is sent only when positive. Several current models reject the
	// parameter outright, so "unset" must mean "omitted", not "0".
	Temperature float64

	// MaxRetries / RetryBackoff mirror the other providers: zero means no
	// retrying. NewBedrockProvider applies the config defaults.
	MaxRetries   int
	RetryBackoff time.Duration

	ProviderID string
	Observe    observe.Observer

	// bearer is non-empty in bearer mode; otherwise creds signs the request.
	bearer string
	creds  sigV4Credentials

	// authErr is a credential/region problem found while constructing. It is
	// reported on the first call rather than at construction, matching the other
	// providers: constructing must not fail, because a config that is only ever
	// used for deterministic generation never calls a model and needs no key.
	authErr error

	// secrets are scrubbed from every error text this provider produces.
	secrets []string

	now    func() time.Time // injectable clock for signing tests
	jitter func(time.Duration) time.Duration
}

// SetObserver installs the Observer, satisfying observerSetter.
func (p *BedrockProvider) SetObserver(o observe.Observer) { p.Observe = o }

// NewBedrockProvider builds the provider from a config model block, reading any
// credential supplied by reference from the process environment.
func NewBedrockProvider(m config.ModelConfig) *BedrockProvider {
	return newBedrockProvider(m, os.Getenv)
}

// newBedrockProvider is NewBedrockProvider with an injectable environment, so
// credential resolution can be tested without touching the real one.
func newBedrockProvider(m config.ModelConfig, getenv func(string) string) *BedrockProvider {
	aws := m.AWS
	if aws == nil {
		aws = &config.AWSConfig{}
	}

	timeout := 60 * time.Second // large models cold-start; matches the Anthropic provider
	if m.TimeoutSeconds > 0 {
		timeout = m.EffectiveTimeout()
	}
	maxTokens := m.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024 // a structured AST is small
	}
	providerID := m.Provider
	if providerID == "" {
		providerID = "bedrock"
	}

	p := &BedrockProvider{
		Model:        strings.TrimSpace(m.Model),
		MaxTokens:    maxTokens,
		Temperature:  m.Temperature,
		HTTPClient:   &http.Client{Timeout: timeout},
		MaxRetries:   m.EffectiveMaxRetries(),
		RetryBackoff: m.EffectiveRetryBackoff(),
		ProviderID:   providerID,
	}

	region, regionErr := resolveAWSRegion(aws, getenv)
	p.Region = region

	// An explicit baseURL (a VPC endpoint, a FIPS endpoint, a proxy) wins over
	// the region-derived host, exactly as it wins over a preset elsewhere.
	if baseURL, _ := resolveEndpoint(m); baseURL != "" {
		p.BaseURL = baseURL
	} else if regionErr == nil {
		p.BaseURL = bedrockRuntimeURL(region)
	}

	bearer, creds, authErr := resolveAWSAuth(aws, getenv)
	p.bearer, p.creds = bearer, creds
	// The region is needed to build the default endpoint and, in SigV4 mode, for
	// the signature itself. A bearer-token caller with an explicit baseURL needs
	// neither, so a missing region is not an error for them.
	switch {
	case authErr != nil:
		p.authErr = authErr
	case regionErr != nil && (p.BaseURL == "" || bearer == ""):
		p.authErr = regionErr
	}

	for _, s := range []string{bearer, creds.SecretAccessKey, creds.SessionToken} {
		if s != "" {
			p.secrets = append(p.secrets, s)
		}
	}
	return p
}

// bedrockRuntimeURL is the public runtime endpoint for a region. The China
// partitions use a different DNS suffix; GovCloud shares the standard one.
func bedrockRuntimeURL(region string) string {
	suffix := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = "amazonaws.com.cn"
	}
	return "https://bedrock-runtime." + region + "." + suffix
}

// resolveAWSRegion applies: config value, then AWS_REGION, then
// AWS_DEFAULT_REGION. Errors name where to set it and never echo a value.
func resolveAWSRegion(a *config.AWSConfig, getenv func(string) string) (string, error) {
	region := strings.TrimSpace(a.Region)
	if region == "" {
		for _, name := range []string{"AWS_REGION", "AWS_DEFAULT_REGION"} {
			if v := strings.TrimSpace(getenv(name)); v != "" {
				region = v
				break
			}
		}
	}
	if region == "" {
		return "", fmt.Errorf("no AWS region: set model.aws.region in the config, or export AWS_REGION")
	}
	if !config.ValidRegion(region) {
		return "", fmt.Errorf("the AWS region taken from the config or the AWS_REGION / AWS_DEFAULT_REGION " +
			"environment is not a region name (expected e.g. \"us-east-1\")")
	}
	return region, nil
}

// resolveAWSAuth picks the authentication mode and reads its credentials.
//
// Precedence, most explicit first:
//
//  1. a bearer token written in the config (inline or by env reference);
//  2. IAM keys written in the config (inline or by env reference);
//  3. nothing configured — the standard environment: AWS_BEARER_TOKEN_BEDROCK
//     if set, else AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY [+ AWS_SESSION_TOKEN].
//
// validateAWS has already rejected a config that names both modes, so (1) and
// (2) cannot both apply.
//
// A session token read from the AMBIENT environment is attached only when the
// access key itself came from the environment. Pairing an inline access key with
// whatever AWS_SESSION_TOKEN happens to be exported would sign with a token that
// belongs to a different key, and the resulting InvalidClientTokenId is baffling.
func resolveAWSAuth(a *config.AWSConfig, getenv func(string) string) (bearer string, creds sigV4Credentials, err error) {
	// ref reads one configured credential. configured reports whether the config
	// named it at all; a configured credential that resolves empty is an error,
	// because it is a mistake the caller can fix, unlike an unset ambient default.
	ref := func(field, inline, envName string) (val string, configured bool, err error) {
		inline, envName = strings.TrimSpace(inline), strings.TrimSpace(envName)
		switch {
		case inline != "":
			return inline, true, nil
		case envName != "":
			v := strings.TrimSpace(getenv(envName))
			if v == "" {
				return "", true, fmt.Errorf("model.aws.%sEnv names the environment variable %s, which is empty or not set in this process",
					field, envName)
			}
			return v, true, nil
		}
		return "", false, nil
	}

	// (1) bearer configured.
	if tok, ok, e := ref("bearerToken", a.BearerToken.Reveal(), a.BearerTokenEnv); e != nil {
		return "", creds, e
	} else if ok {
		return tok, creds, nil
	}

	// (2) IAM keys configured.
	id, idCfg, e := ref("accessKeyId", a.AccessKeyID, a.AccessKeyIDEnv)
	if e != nil {
		return "", creds, e
	}
	secret, secretCfg, e := ref("secretAccessKey", a.SecretAccessKey.Reveal(), a.SecretAccessKeyEnv)
	if e != nil {
		return "", creds, e
	}
	session, sessionCfg, e := ref("sessionToken", a.SessionToken.Reveal(), a.SessionTokenEnv)
	if e != nil {
		return "", creds, e
	}
	if idCfg || secretCfg {
		creds = sigV4Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: session}
		// Ambient session token only when both key halves came from the environment.
		if !sessionCfg && strings.TrimSpace(a.AccessKeyID) == "" && strings.TrimSpace(a.SecretAccessKey.Reveal()) == "" {
			creds.SessionToken = strings.TrimSpace(getenv(config.DefaultAWSSessionEnv))
		}
		return "", creds, nil
	}

	// (3) ambient environment.
	if tok := strings.TrimSpace(getenv(config.DefaultAWSBearerEnv)); tok != "" {
		return tok, creds, nil
	}
	id = strings.TrimSpace(getenv(config.DefaultAWSAccessKeyEnv))
	secret = strings.TrimSpace(getenv(config.DefaultAWSSecretEnv))
	if id != "" && secret != "" {
		if !sessionCfg {
			session = strings.TrimSpace(getenv(config.DefaultAWSSessionEnv))
		}
		return "", sigV4Credentials{AccessKeyID: id, SecretAccessKey: secret, SessionToken: session}, nil
	}
	return "", creds, fmt.Errorf("no AWS credentials: set model.aws.bearerToken / bearerTokenEnv (a Bedrock API key), " +
		"or model.aws.accessKeyId + secretAccessKey, or export AWS_BEARER_TOKEN_BEDROCK, " +
		"or AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY (+ AWS_SESSION_TOKEN for temporary credentials)")
}

// converseRequest is the Converse request body.
type converseRequest struct {
	Messages        []converseMessage  `json:"messages"`
	System          []converseText     `json:"system,omitempty"`
	InferenceConfig *converseInference `json:"inferenceConfig,omitempty"`
}

type converseMessage struct {
	Role    string         `json:"role"`
	Content []converseText `json:"content"`
}

type converseText struct {
	Text string `json:"text"`
}

type converseInference struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
}

// converseResponse is the subset of the Converse response we consume.
type converseResponse struct {
	Output struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
		TotalTokens  int `json:"totalTokens"`
	} `json:"usage"`
}

// Complete performs a Converse call, retrying retryable round trips.
func (p *BedrockProvider) Complete(ctx context.Context, system, user string) (string, error) {
	rp := retryPolicy{MaxRetries: p.MaxRetries, BaseBackoff: p.RetryBackoff, jitter: p.jitter}
	return rp.do(ctx, func(ctx context.Context, n int) (string, error) {
		return p.roundTrip(ctx, system, user, n)
	})
}

// roundTrip performs exactly one Converse request, emitting one EventModelCall.
func (p *BedrockProvider) roundTrip(ctx context.Context, system, user string, retry int) (out string, err error) {
	start := time.Now()
	ev := observe.Event{Kind: observe.EventModelCall, Provider: p.ProviderID, Model: p.Model, Outcome: observe.OutcomeOK, Retry: retry}
	defer func() {
		ev.Latency = time.Since(start)
		if err != nil {
			ev.Outcome = observe.OutcomeTransport
			ev.Err = err
			if pe, ok := asProviderError(err); ok {
				ev.ErrorKind = pe.Kind
			}
		}
		observe.Emit(p.Observe, ctx, ev)
	}()

	if p.authErr != nil {
		return "", p.fail(failure.KindAuth, 0, p.authErr.Error(), nil)
	}
	if p.Model == "" {
		return "", p.fail(failure.KindInvalidRequest, 0,
			"model.model is required for provider \"bedrock\": a Bedrock model id or inference-profile id, "+
				"e.g. the id shown on the model's page in the Bedrock console", nil)
	}
	if p.BaseURL == "" {
		return "", p.fail(failure.KindInvalidRequest, 0, "no Bedrock endpoint: set model.aws.region or model.baseURL", nil)
	}

	body := converseRequest{
		Messages: []converseMessage{{Role: "user", Content: []converseText{{Text: user}}}},
	}
	if system != "" {
		body.System = []converseText{{Text: system}}
	}
	inf := &converseInference{MaxTokens: p.MaxTokens}
	if p.Temperature > 0 {
		t := p.Temperature
		inf.Temperature = &t
	}
	body.InferenceConfig = inf

	raw, err := json.Marshal(body)
	if err != nil {
		return "", p.fail(failure.KindInvalidRequest, 0, "marshal request: "+err.Error(), err)
	}

	// The model id goes into the path fully percent-encoded: ids contain ":" and
	// inference-profile ARNs contain "/" too, and neither may travel raw.
	endpoint := p.BaseURL + "/model/" + uriEncode(p.Model) + "/converse"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return "", p.fail(failure.KindInvalidRequest, 0, "build request: "+err.Error(), err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	if p.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+p.bearer)
	} else {
		now := time.Now
		if p.now != nil {
			now = p.now
		}
		signRequest(req, raw, p.creds, p.Region, "bedrock", now())
	}

	client := p.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := safeClient(client).Do(req)
	if err != nil {
		err = scrubURLError(err) // the URL may carry a token in its query string
		return "", p.fail(classifyTransport(ctx, err), 0, "request failed: "+err.Error(), err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", p.fail(classifyTransport(ctx, err), resp.StatusCode, "read response: "+err.Error(), err)
	}
	if resp.StatusCode/100 != 2 {
		errType := bedrockErrorType(resp.Header)
		detail := snippet(data, 300)
		if errType != "" {
			detail = errType + ": " + detail
		}
		pe := p.fail(classifyBedrock(resp.StatusCode, errType, detail), resp.StatusCode, detail, nil)
		pe.RetryAfter = parseRetryAfter(resp.Header)
		return "", p.unreachable(pe)
	}

	var cr converseResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return "", p.fail(failure.KindBadResponse, resp.StatusCode, "decode response: "+err.Error(), err)
	}
	ev.PromptTokens = cr.Usage.InputTokens
	ev.CompletionTokens = cr.Usage.OutputTokens
	ev.TotalTokens = cr.Usage.TotalTokens
	if ev.TotalTokens == 0 {
		ev.TotalTokens = cr.Usage.InputTokens + cr.Usage.OutputTokens
	}
	ev.FinishReason = cr.StopReason

	switch cr.StopReason {
	case "max_tokens":
		// Same reasoning as the other dialects: a truncated reply looks like valid
		// JSON with its tail missing, and an identical request truncates
		// identically, so this is reported plainly and not retried.
		return "", p.fail(failure.KindBadResponse, resp.StatusCode, fmt.Sprintf(
			"reply truncated by the token budget (stopReason=max_tokens; input=%d, output=%d): raise model.maxTokens in the config",
			cr.Usage.InputTokens, cr.Usage.OutputTokens), nil)
	case "guardrail_intervened", "content_filtered":
		return "", p.fail(failure.KindBadResponse, resp.StatusCode,
			"the reply was blocked by a Bedrock guardrail or content filter (stopReason="+cr.StopReason+")", nil)
	}

	var b strings.Builder
	for _, block := range cr.Output.Message.Content {
		b.WriteString(block.Text)
	}
	out = b.String()
	for _, s := range p.secrets { // see the twin comment in provider.go
		out = redactSecret(out, s)
	}
	if out == "" {
		return "", p.fail(failure.KindBadResponse, resp.StatusCode, "response contained no text content", nil)
	}
	return out, nil
}

// bedrockErrorType extracts the exception name AWS puts in the X-Amzn-ErrorType
// header. The value is sometimes "Name:https://…", sometimes just "Name".
func bedrockErrorType(h http.Header) string {
	v := strings.TrimSpace(h.Get("X-Amzn-ErrorType"))
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[:i]
	}
	return v
}

// classifyBedrock maps a Bedrock failure onto a kind.
//
// The exception name is consulted first because Bedrock reuses status codes —
// 400 covers both a malformed request and an exhausted service quota, which want
// opposite handling — and falls back to the status mapping every other provider
// shares. Names are matched, not message prose, for the reason classifyStatus
// gives; the one prose hint (quota wording) is the shared one.
func classifyBedrock(status int, errType, detail string) failure.ProviderErrorKind {
	switch errType {
	case "ServiceQuotaExceededException":
		return failure.KindQuota
	case "ThrottlingException", "ModelNotReadyException", "TooManyRequestsException":
		if looksLikeQuota(detail) {
			return failure.KindQuota
		}
		return failure.KindRateLimit
	case "ResourceNotFoundException":
		return failure.KindModelNotFound
	case "ModelTimeoutException":
		return failure.KindTimeout
	case "ModelErrorException", "InternalServerException", "ServiceUnavailableException":
		return failure.KindUnavailable
	case "ExpiredTokenException", "UnrecognizedClientException", "InvalidSignatureException",
		"IncompleteSignature", "InvalidClientTokenId", "SignatureDoesNotMatch",
		"MissingAuthenticationToken", "AccessDeniedException":
		return failure.KindAuth
	case "ValidationException":
		return failure.KindInvalidRequest
	}
	return classifyStatus(status, detail)
}

// unreachable appends the Bedrock-specific remedy to a terminal failure, naming
// the knob that fixes it. The shared errModelUnreachable points at apiKeyEnv,
// which means nothing to a Bedrock config.
func (p *BedrockProvider) unreachable(pe *ProviderError) error {
	low := strings.ToLower(pe.Detail)
	switch {
	case strings.Contains(low, "inference profile"):
		// Many models cannot be invoked by their bare id; they need a
		// cross-region inference profile id (prefixed "us.", "eu.", "apac." or
		// "global."). Bedrock says so, but in a sentence nobody connects to
		// model.model.
		return fmt.Errorf("%w: this model must be invoked through an inference profile. "+
			"Set model.model to the profile id or ARN (the bare model id with a geography prefix such as \"us.\" or \"eu.\")", pe)
	case pe.Kind == failure.KindAuth && strings.Contains(low, "expired"):
		return fmt.Errorf("%w: the AWS credentials have expired. Temporary credentials (STS, AssumeRole, SSO) "+
			"last minutes to hours; supply fresh ones", pe)
	case pe.Kind == failure.KindAuth:
		return fmt.Errorf("%w: check the AWS credentials are valid for region %s, that a session token accompanies "+
			"temporary keys, and that the IAM identity may call bedrock:InvokeModel on this model. "+
			"For \"access to the model\" errors, model access may not be enabled for this account and region", pe, p.Region)
	case pe.Kind == failure.KindModelNotFound:
		return fmt.Errorf("%w: Bedrock does not serve this model id in region %s; check model.model and model.aws.region", pe, p.Region)
	case pe.Kind == failure.KindQuota:
		return fmt.Errorf("%w: this account has hit a Bedrock service quota; waiting will not clear it — "+
			"switch models or add a `models` fallback entry", pe)
	}
	return pe
}

// fail builds a classified error with every credential scrubbed from the text.
func (p *BedrockProvider) fail(kind failure.ProviderErrorKind, status int, detail string, cause error) *ProviderError {
	for _, s := range p.secrets {
		detail = redactSecret(detail, s)
	}
	return newProviderError(kind, status, p.ProviderID, p.Model, detail, "", cause)
}
