package queryforge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/config"
	"github.com/awsaman-ai/queryforge/internal/provider"
	"github.com/awsaman-ai/queryforge/internal/testutil"
)

// credleak_test.go: the credential-hygiene suite. Each test hands the library a
// recognisable canary secret and then searches everything it could hand to a log.

const probeKey = "AIzaSyLEAKCANARY-0123456789abcdef"

func probeCfg(t *testing.T, model map[string]any) *Config {
	t.Helper()
	var raw map[string]any
	_ = json.Unmarshal([]byte(testutil.GenConfigJSON), &raw)
	raw["model"] = model
	b, _ := json.Marshal(raw)
	c, err := ParseConfig(b)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return c
}

// Everything the engine could hand to a log, as one string.
func everything(rec *recorder, err error) string {
	var sb strings.Builder
	if err != nil {
		sb.WriteString("ERR:" + err.Error() + " ")
		fmt.Fprintf(&sb, "%+v %#v ", err, err)
	}
	for _, ev := range rec.all() {
		fmt.Fprintf(&sb, "%+v %#v ", ev, ev)
		if ev.Err != nil {
			sb.WriteString(ev.Err.Error())
		}
	}
	return sb.String()
}

func TestRedirectToAnotherOriginIsRefusedAndKeyNotForwarded(t *testing.T) {
	var otherSaw string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherSaw = r.Header.Get("x-api-key") + "|" + r.Header.Get("Authorization")
		io.WriteString(w, `{}`)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/messages", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	for _, proto := range []string{"anthropic", "openai"} {
		otherSaw = ""
		c := probeCfg(t, map[string]any{"provider": "custom", "protocol": proto, "baseURL": origin.URL, "model": "m", "apiKey": probeKey})
		_, _ = New(c).Translate(context.Background(), "delivered orders", "sql", nil)
		if strings.Contains(otherSaw, probeKey) {
			t.Errorf("%s: key forwarded to a different host on redirect: %q", proto, otherSaw)
		}
	}
}

func TestURLSecretsNeverReachErrorsOrEvents(t *testing.T) {
	// A dead endpoint whose address carries userinfo and a query-string secret.
	for _, base := range []string{
		"http://user:PASSCANARY99@127.0.0.1:1",
		"http://127.0.0.1:1/v1?token=QUERYCANARY99",
	} {
		c := probeCfg(t, map[string]any{"provider": "custom", "baseURL": base, "model": "m", "apiKey": probeKey, "maxRetries": 0})
		var rec recorder
		e := New(c)
		e.SetObserver(rec.observe())
		_, err := e.Translate(context.Background(), "delivered orders", "sql", nil)
		out := everything(&rec, err)
		for _, canary := range []string{"PASSCANARY99", "QUERYCANARY99", probeKey} {
			if strings.Contains(out, canary) {
				t.Errorf("base %q leaks %q:\n%s", base, canary, out)
			}
		}
	}
}

func TestProviderValuesDoNotPrintTheKey(t *testing.T) {
	m := config.ModelConfig{Provider: "custom", BaseURL: "http://h", Model: "m", APIKey: probeKey}
	for name, p := range map[string]any{"openai": provider.NewOpenAIProvider(m), "anthropic": provider.NewAnthropicProvider(m)} {
		for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
			if s := fmt.Sprintf(f, p); strings.Contains(s, probeKey) {
				t.Errorf("%s %s prints the key", name, f)
			}
		}
	}
}

func TestDecoderErrorDoesNotEchoANumericKey(t *testing.T) {
	_, err := ParseConfig([]byte(`{"entity":"O","model":{"apiKey":8675309123456789},"fields":[{"name":"n","type":"number"}]}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "8675309123456789") {
		t.Errorf("decoder error echoes the value: %v", err)
	}
}

// A redirect that stays on the same origin is ordinary and must still work.
func TestSameOriginRedirectStillFollowed(t *testing.T) {
	var hit string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old/chat/completions" {
			http.Redirect(w, r, srv.URL+"/new/chat/completions", http.StatusTemporaryRedirect)
			return
		}
		hit = r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":`+mustJSON(testutil.CanonicalAST)+`}}]}`)
	}))
	defer srv.Close()
	c := probeCfg(t, map[string]any{"provider": "custom", "baseURL": srv.URL + "/old", "model": "m", "apiKey": probeKey})
	if _, err := New(c).Translate(context.Background(), "delivered orders in the last 30 days", "sql", nil); err != nil {
		t.Fatalf("same-origin redirect should work: %v", err)
	}
	if hit != "Bearer "+probeKey {
		t.Errorf("Authorization after a same-origin redirect = %q", hit)
	}
}

func mustJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

// Scrubbing the URL must leave the diagnosis intact: host and path stay, and a
// timeout is still recognisable as one.
func TestScrubbedTransportErrorKeepsHostPathAndCause(t *testing.T) {
	c := probeCfg(t, map[string]any{"provider": "custom", "baseURL": "http://127.0.0.1:1/v1?token=QUERYCANARY99", "model": "m", "maxRetries": 0})
	_, err := New(c).Translate(context.Background(), "delivered orders", "sql", nil)
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("diagnostic text lost: %v", err)
	}
}

// A gateway that reflects request headers into the model's reply must not get
// the key into TranslateResult.Raw or the debug "raw" log attribute.
func TestKeyReflectedInModelReplyIsRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := "I cannot do that. Debug: " + r.Header.Get("Authorization")
		io.WriteString(w, `{"choices":[{"message":{"content":`+mustJSON(reply)+`}}]}`)
	}))
	defer srv.Close()
	c := probeCfg(t, map[string]any{"provider": "custom", "baseURL": srv.URL, "model": "m", "apiKey": probeKey, "maxRetries": 0})
	var rec recorder
	e := New(c)
	e.SetObserver(rec.observe())
	_, err := e.Translate(context.Background(), "delivered orders", "sql", nil)
	if out := everything(&rec, err); strings.Contains(out, probeKey) {
		t.Errorf("reflected key reached events/errors:\n%s", out)
	}
	var sawRaw bool
	for _, ev := range rec.all() {
		if ev.Raw != "" {
			sawRaw = true
			if !strings.Contains(ev.Raw, "[REDACTED]") {
				t.Errorf("Raw not redacted: %q", ev.Raw)
			}
		}
	}
	if !sawRaw {
		t.Error("expected a failed attempt carrying Raw")
	}
}
