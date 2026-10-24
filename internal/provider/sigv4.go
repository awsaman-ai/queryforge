package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, implemented against the standard library.
//
// This is written by hand rather than imported for one reason: QueryForge's Go
// core has no third-party dependencies, and the AWS SDK would be the first —
// inherited by every downstream module and bundled into every Python wheel and
// Java jar. Signing one POST is about a hundred lines. What it deliberately does
// NOT do is the rest of the SDK: it does not resolve credentials from profiles,
// SSO, AssumeRole, IMDS or web identity. It signs with the credentials it is
// handed.
//
// Correctness is pinned two ways in the tests: against AWS's published
// signing-key and signature example, and against signatures produced by
// botocore for the same inputs.

const (
	sigV4Algorithm = "AWS4-HMAC-SHA256"
	amzDateLayout  = "20060102T150405Z"
	dateLayout     = "20060102"
)

// sigV4Credentials is the material a signature needs. SessionToken is empty for
// long-term IAM keys and set for any temporary (STS) credential.
type sigV4Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// signRequest adds the Authorization (and, for temporary credentials,
// X-Amz-Security-Token) headers to req, signing the given body.
//
// canonicalPath is the URI-encoded path AS IT IS SENT, and the canonical request
// encodes it a second time. That double encoding is the classic SigV4 trap for
// every service except S3: a Bedrock model id such as
// "anthropic.claude-3-5-sonnet-20241022-v2:0" travels on the wire as "...v2%3A0"
// and is signed as "...v2%253A0". Getting it wrong yields a signature mismatch
// that only appears for model ids containing ":" or "/" — i.e. most of them.
func signRequest(req *http.Request, body []byte, creds sigV4Credentials, region, service string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(amzDateLayout)
	dateStamp := now.Format(dateLayout)

	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	// Only a fixed, small set of headers is signed. Signing more would be
	// stricter but fragile — a proxy that adds or reorders a header would break
	// the signature — and these are the ones the service requires.
	signed := map[string]string{
		"content-type": req.Header.Get("Content-Type"),
		"host":         req.URL.Host,
		"x-amz-date":   amzDate,
	}
	if creds.SessionToken != "" {
		signed["x-amz-security-token"] = creds.SessionToken
	}
	names := make([]string, 0, len(signed))
	for n := range signed {
		names = append(names, n)
	}
	sort.Strings(names)

	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteString(":")
		canonHeaders.WriteString(strings.TrimSpace(signed[n]))
		canonHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")

	canonicalURI := canonicalPath(req.URL.EscapedPath())
	canonicalQuery := canonicalQueryString(req)

	payloadHash := sha256Hex(body)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonHeaders.String(), // already newline-terminated, giving the required blank line
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + region + "/" + service + "/aws4_request"
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signature := hex.EncodeToString(hmacSHA256(signingKey(creds.SecretAccessKey, dateStamp, region, service), stringToSign))

	req.Header.Set("Authorization", sigV4Algorithm+
		" Credential="+creds.AccessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

// signingKey derives the per-day, per-region, per-service key.
func signingKey(secret, dateStamp, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalPath double-encodes an already-encoded request path, segment by
// segment, leaving the "/" separators alone. An empty path signs as "/".
func canonicalPath(escaped string) string {
	if escaped == "" {
		return "/"
	}
	segs := strings.Split(escaped, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQueryString renders the query as SigV4 requires: keys and values
// URI-encoded, pairs sorted by key then value. Bedrock Converse sends none, but
// the signer handles it so it can be checked against AWS's published examples.
func canonicalQueryString(req *http.Request) string {
	q := req.URL.Query()
	if len(q) == 0 {
		return ""
	}
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{uriEncode(k), uriEncode(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = p.k + "=" + p.v
	}
	return strings.Join(parts, "&")
}

// uriEncode percent-encodes every byte except the RFC 3986 unreserved set
// (A-Z a-z 0-9 - _ . ~), using upper-case hex. It is NOT url.PathEscape, which
// leaves ":" "@" "&" "=" "+" "$" unescaped — exactly the characters that appear
// in Bedrock model ids and ARNs, and exactly the ones SigV4 requires escaped.
func uriEncode(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}
