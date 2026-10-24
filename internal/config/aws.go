package config

import (
	"fmt"
	"regexp"
	"strings"
)

// AWSConfig holds everything the Amazon Bedrock provider needs that an
// OpenAI-style model block has no place for: a region, and a credential.
//
// Every credential can be supplied in TWO ways, and the two are mutually
// exclusive per credential:
//
//   - by reference: <field>Env names an environment variable. This is the
//     default, documented path, and it is the only one that keeps a secret out
//     of the config file, version control, and any bug report the config is
//     pasted into.
//   - inline: <field> carries the value itself. Supported because a deployment
//     that renders its config from a secrets manager at startup has no
//     environment to point at, and refusing the value would just push those
//     users into writing a wrapper. It is accepted with eyes open: loading a
//     config that uses it raises an advisory (see InlineSecrets), and the value
//     is redacted from every error the provider produces.
//
// Nothing here is required. A bare {"provider":"bedrock","model":"..."} works
// if the process environment already carries the standard AWS variables
// (AWS_REGION, AWS_BEARER_TOKEN_BEDROCK, or AWS_ACCESS_KEY_ID +
// AWS_SECRET_ACCESS_KEY [+ AWS_SESSION_TOKEN]) — the same names every AWS SDK
// and the AWS CLI already read, so existing shell setups just work.
//
// Two authentication modes exist and are chosen by what is configured:
//
//   - bearer: a Bedrock API key, sent as `Authorization: Bearer`.
//   - sigv4: IAM access keys (optionally with a session token for temporary
//     STS credentials), used to SIGN each request.
type AWSConfig struct {
	// Region is the AWS region of the Bedrock endpoint, e.g. "us-east-1".
	// Empty falls back to AWS_REGION, then AWS_DEFAULT_REGION.
	Region string `json:"region,omitempty"`

	// SigV4 credentials. SessionToken is only for TEMPORARY credentials (STS,
	// AssumeRole, SSO) and must accompany the access key it was issued with.
	//
	// The three secrets are Secret values, so a config carrying them can be
	// printed, logged or re-marshalled without disclosing them (see Secret). The
	// access key id is an identifier, not a secret, and stays a plain string.
	AccessKeyID        string `json:"accessKeyId,omitempty"`
	AccessKeyIDEnv     string `json:"accessKeyIdEnv,omitempty"`
	SecretAccessKey    Secret `json:"secretAccessKey,omitempty"`
	SecretAccessKeyEnv string `json:"secretAccessKeyEnv,omitempty"`
	SessionToken       Secret `json:"sessionToken,omitempty"`
	SessionTokenEnv    string `json:"sessionTokenEnv,omitempty"`

	// Bearer credential: a Bedrock API key (short- or long-term).
	BearerToken    Secret `json:"bearerToken,omitempty"`
	BearerTokenEnv string `json:"bearerTokenEnv,omitempty"`
}

// Default environment variable names, used when a credential is configured
// neither inline nor by reference. They are the names AWS itself defines.
const (
	DefaultAWSBearerEnv    = "AWS_BEARER_TOKEN_BEDROCK"
	DefaultAWSAccessKeyEnv = "AWS_ACCESS_KEY_ID"
	DefaultAWSSecretEnv    = "AWS_SECRET_ACCESS_KEY"
	DefaultAWSSessionEnv   = "AWS_SESSION_TOKEN"
)

// regionPattern is deliberately loose: it rejects an obvious paste error
// ("US EAST", a URL, a key) without maintaining a list of AWS regions that
// would go stale the day a new one launches. Matches us-east-1, eu-west-3,
// ap-southeast-2, us-gov-west-1, cn-north-1, il-central-1.
var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// ValidRegion reports whether s has the shape of an AWS region name.
func ValidRegion(s string) bool { return regionPattern.MatchString(s) }

// looksLikeAWSAccessKeyID reports whether s is shaped like an IAM access key
// id: exactly 20 characters opening AKIA (long-term) or ASIA (temporary). Used
// to catch a key id pasted where an environment-variable NAME belongs.
func looksLikeAWSAccessKeyID(s string) bool {
	return len(s) == 20 && (strings.HasPrefix(s, "AKIA") || strings.HasPrefix(s, "ASIA"))
}

// credentialPair is one credential's two spellings, for uniform validation.
type credentialPair struct {
	field  string // JSON name of the inline form, for messages
	inline string
	env    string
}

func (a *AWSConfig) pairs() []credentialPair {
	return []credentialPair{
		{"accessKeyId", a.AccessKeyID, a.AccessKeyIDEnv},
		{"secretAccessKey", a.SecretAccessKey.Reveal(), a.SecretAccessKeyEnv},
		{"sessionToken", a.SessionToken.Reveal(), a.SessionTokenEnv},
		{"bearerToken", a.BearerToken.Reveal(), a.BearerTokenEnv},
	}
}

// validateAWS checks the aws block of one model block.
//
// Errors never quote an offending value: for an inline field that value IS the
// secret, and for an *Env field it is probably a secret pasted into the wrong
// slot. Echoing either would reprint it into logs, which is the thing the
// separate fields exist to avoid.
func (m ModelConfig) validateAWS(where string) error {
	a := m.AWS
	if a == nil {
		return nil
	}
	if r := strings.TrimSpace(a.Region); r != "" && !ValidRegion(r) {
		return fmt.Errorf("config: %s.aws.region is not an AWS region name (expected e.g. \"us-east-1\")", where)
	}

	for _, p := range a.pairs() {
		if strings.TrimSpace(p.inline) != "" && strings.TrimSpace(p.env) != "" {
			return fmt.Errorf("config: %s.aws sets both %s and %sEnv. Choose one: the value inline, "+
				"or the NAME of the environment variable that holds it", where, p.field, p.field)
		}
		name := strings.TrimSpace(p.env)
		if name == "" {
			continue
		}
		if looksLikeAWSAccessKeyID(name) {
			return fmt.Errorf("config: %s.aws.%sEnv looks like an AWS access key id, not an environment variable name. "+
				"Put the id in %s instead, or export it and name the variable here (e.g. \"AWS_ACCESS_KEY_ID\")",
				where, p.field, p.field)
		}
		if !validEnvVarName(name) {
			return fmt.Errorf("config: %s.aws.%sEnv is not a valid environment variable name "+
				"(expected letters, digits and underscores, not starting with a digit). "+
				"This field takes the variable's NAME; to give the value directly use %s",
				where, p.field, p.field)
		}
	}

	// One request cannot be authenticated two ways. AWS's own tools resolve this
	// silently by precedence; here it is a load error, because a config that
	// names both is almost always a leftover that will surprise someone later.
	bearer := strings.TrimSpace(a.BearerToken.Reveal()) != "" || strings.TrimSpace(a.BearerTokenEnv) != ""
	sigv4 := strings.TrimSpace(a.AccessKeyID) != "" || strings.TrimSpace(a.AccessKeyIDEnv) != "" ||
		strings.TrimSpace(a.SecretAccessKey.Reveal()) != "" || strings.TrimSpace(a.SecretAccessKeyEnv) != ""
	if bearer && sigv4 {
		return fmt.Errorf("config: %s.aws configures both a bearer token and access keys. "+
			"Pick one authentication mode: bearerToken/bearerTokenEnv (a Bedrock API key) "+
			"or accessKeyId + secretAccessKey (IAM keys)", where)
	}

	// Half a key pair never authenticates. Both halves must come from the config
	// (in either spelling) — a half-configured pair that quietly fills the rest
	// from ambient environment would sign with a mismatched pair.
	hasID := strings.TrimSpace(a.AccessKeyID) != "" || strings.TrimSpace(a.AccessKeyIDEnv) != ""
	hasSecret := strings.TrimSpace(a.SecretAccessKey.Reveal()) != "" || strings.TrimSpace(a.SecretAccessKeyEnv) != ""
	if hasID != hasSecret {
		return fmt.Errorf("config: %s.aws sets only one of accessKeyId and secretAccessKey. "+
			"IAM keys are a pair: set both, or neither to use the AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY environment", where)
	}
	return nil
}

// InlineSecrets lists the JSON names of credentials written directly into this
// model block's config. Empty when every credential is by reference or absent.
//
// It reports NAMES only — never values — so the result is safe to log and to
// return to a caller.
func (m ModelConfig) InlineSecrets() []string {
	if m.AWS == nil {
		return nil
	}
	var out []string
	for _, p := range m.AWS.pairs() {
		if strings.TrimSpace(p.inline) != "" {
			out = append(out, "aws."+p.field)
		}
	}
	return out
}

// Advisories returns non-fatal notices about the config itself, as opposed to
// about a particular question. Today that is one thing: credentials written
// inline.
//
// An advisory rather than an error because inline credentials are a supported
// choice, and rather than silence because they are the riskier one. The text
// names the field and the remedy, and never the value.
func (c *Config) Advisories() []string {
	var out []string
	note := func(where string, m ModelConfig) {
		if fields := m.InlineSecrets(); len(fields) > 0 {
			out = append(out, fmt.Sprintf(
				"%s: credentials are written inline in the config (%s). Anyone who can read the config can read them, "+
					"and configs get committed and pasted into bug reports. Prefer the matching *Env field "+
					"(e.g. secretAccessKeyEnv) and export the value instead",
				where, strings.Join(fields, ", ")))
		}
	}
	note("model", c.Model)
	for i := range c.Models {
		note(fmt.Sprintf("models[%d]", i), c.Models[i])
	}
	return out
}
