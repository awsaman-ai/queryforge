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

	"github.com/awsaman-ai/queryforge/internal/testutil"
)

const e2eKey = "AIzaSyLEAKCANARY-0123456789abcdef"

// configWithInlineKey builds a config whose model block carries the key inline
// and points at a fake endpoint.
func configWithInlineKey(t *testing.T, baseURL string) *Config {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal([]byte(testutil.GenConfigJSON), &raw); err != nil {
		t.Fatal(err)
	}
	raw["model"] = map[string]any{"provider": "custom", "baseURL": baseURL, "model": "m", "apiKey": e2eKey}
	b, _ := json.Marshal(raw)
	c, err := ParseConfig(b)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return c
}

func chatReply(content string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}}}})
	return string(b)
}

// Whole path: config JSON with an inline key -> New -> Translate -> the key
// reaches the endpoint, and appears in no event, error or formatted value.
func TestInlineAPIKeyEndToEnd(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, chatReply(testutil.CanonicalAST))
	}))
	defer srv.Close()

	cfg := configWithInlineKey(t, srv.URL)
	var rec recorder
	e := New(cfg)
	e.SetObserver(rec.observe())

	if _, err := e.Translate(context.Background(), "delivered orders in the last 30 days", "sql", nil); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if gotAuth != "Bearer "+e2eKey {
		t.Errorf("Authorization = %q", gotAuth)
	}
	for i, ev := range rec.all() {
		if s := fmt.Sprintf("%+v", ev); strings.Contains(s, e2eKey) {
			t.Errorf("event %d leaks the key: %s", i, s)
		}
	}
	if s := fmt.Sprintf("%+v %#v", cfg, cfg.Model); strings.Contains(s, e2eKey) {
		t.Errorf("formatted config leaks the key: %s", s)
	}
}

// Worst case: the endpoint rejects the key and echoes it back. The failure the
// caller sees, and every event, stay clean.
func TestInlineAPIKeyRejectedByProviderDoesNotLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "invalid credential "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	defer srv.Close()

	var rec recorder
	e := New(configWithInlineKey(t, srv.URL))
	e.SetObserver(rec.observe())

	_, err := e.Translate(context.Background(), "delivered orders", "sql", nil)
	if err == nil {
		t.Fatal("expected a provider error")
	}
	if strings.Contains(err.Error(), e2eKey) {
		t.Errorf("error leaks the key: %v", err)
	}
	for i, ev := range rec.all() {
		if s := fmt.Sprintf("%+v", ev); strings.Contains(s, e2eKey) {
			t.Errorf("event %d leaks the key: %s", i, s)
		}
	}
}

// A fallback chain may mix an env-var entry and an inline-key entry; the chain
// must authenticate each with its own credential.
func TestFallbackChainMixesEnvAndInlineKeys(t *testing.T) {
	t.Setenv("QF_E2E_PRIMARY", "primary-env-key")
	var primaryAuth, backupAuth string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusBadRequest) // non-retryable: falls through to the backup
		io.WriteString(w, "nope")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupAuth = r.Header.Get("Authorization")
		io.WriteString(w, chatReply(testutil.CanonicalAST))
	}))
	defer backup.Close()

	var raw map[string]any
	_ = json.Unmarshal([]byte(testutil.GenConfigJSON), &raw)
	raw["model"] = map[string]any{"provider": "custom", "baseURL": primary.URL, "model": "m", "apiKeyEnv": "QF_E2E_PRIMARY"}
	raw["models"] = []any{map[string]any{"provider": "custom", "baseURL": backup.URL, "model": "m", "apiKey": e2eKey}}
	b, _ := json.Marshal(raw)
	cfg, err := ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg).Translate(context.Background(), "delivered orders in the last 30 days", "sql", nil); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if primaryAuth != "Bearer primary-env-key" || backupAuth != "Bearer "+e2eKey {
		t.Errorf("primary=%q backup=%q", primaryAuth, backupAuth)
	}
}
