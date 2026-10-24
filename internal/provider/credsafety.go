package provider

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// This file holds the controls that keep a credential from leaving through a
// side door: a redirect that carries it to another host, an error message that
// prints it as part of a URL, a %v that dumps it from a provider value.

// errCrossOriginRedirect is why a redirect was refused. It names no URL: the
// target of a redirect is attacker-influenced text, and the source URL may
// itself carry a secret.
var errCrossOriginRedirect = errors.New("refusing a redirect to a different origin: the credential would be forwarded to it")

// maxRedirects matches net/http's own default of 10 hops.
const maxRedirects = 10

// sameOriginOnly is an http.Client.CheckRedirect that follows a redirect only
// when it stays on the same scheme and host:port as the original request.
//
// net/http already drops the Authorization header when a redirect crosses to a
// different host, but NOT custom headers: Anthropic's x-api-key, and the
// X-Amz-* headers Bedrock sends, would be forwarded verbatim. A model endpoint
// has no business redirecting a POST to another origin, so the safe policy is
// to refuse rather than to enumerate which headers to strip.
func sameOriginOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	first := via[0].URL
	if req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return errCrossOriginRedirect
	}
	return nil
}

// safeClient returns c with the same-origin redirect policy applied, unless the
// caller installed a CheckRedirect of their own — an explicit choice is
// respected. The client is copied, never mutated: it may be shared.
func safeClient(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	if c.CheckRedirect != nil {
		return c
	}
	cp := *c
	cp.CheckRedirect = sameOriginOnly
	return &cp
}

// scrubURLError returns err with any *url.Error's URL reduced to scheme, host
// and path. net/http puts the full request URL into the error text, and a query
// string (or userinfo) is exactly where a user who points baseURL at a gateway
// tends to keep a token. The result still unwraps to the same cause, so
// errors.Is(err, context.DeadlineExceeded) and Timeout() behave as before.
func scrubURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	clean := &url.Error{Op: ue.Op, URL: redactURL(ue.URL), Err: ue.Err}
	if clean.URL == ue.URL {
		return err
	}
	return clean
}

// redactURL drops the userinfo, query and fragment of a URL string. An
// unparseable string is replaced outright rather than risk printing it.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable url]"
	}
	if u.User == nil && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery {
		return raw
	}
	u.User, u.RawQuery, u.Fragment, u.ForceQuery = nil, "", "", false
	return u.String()
}

// String, GoString: a provider holds its key in a field, so printing one with
// %v, %+v or %#v would print the key. These cover every verb fmt and a
// structured logger reach for.
func (p *OpenAIProvider) String() string {
	return fmt.Sprintf("OpenAIProvider{provider:%q model:%q baseURL:%q apiKey:%s}", p.ProviderID, p.Model, redactURL(p.BaseURL), keyState(p.APIKey))
}
func (p *OpenAIProvider) GoString() string { return p.String() }

func (p *AnthropicProvider) String() string {
	return fmt.Sprintf("AnthropicProvider{provider:%q model:%q baseURL:%q apiKey:%s}", p.ProviderID, p.Model, redactURL(p.BaseURL), keyState(p.APIKey))
}
func (p *AnthropicProvider) GoString() string { return p.String() }

func (p *BedrockProvider) String() string {
	return fmt.Sprintf("BedrockProvider{provider:%q model:%q region:%q credentials:[REDACTED]}", p.ProviderID, p.Model, p.Region)
}
func (p *BedrockProvider) GoString() string { return p.String() }

// keyState says whether a key is configured without saying what it is.
func keyState(k string) string {
	if k == "" {
		return "unset"
	}
	return "[REDACTED]"
}
