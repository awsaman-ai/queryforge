package main

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	qf "github.com/awsaman-ai/queryforge"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// invoke drives one full request through run(), the way the process does, and
// returns the exit code plus both streams. Everything below goes through this
// rather than calling handle() directly, because the properties being tested —
// stdout stays clean, a bad level fails fast, logs reach stderr — are properties
// of the whole invocation.
type invocation struct {
	code   int
	stdout string
	stderr string
}

func invoke(t *testing.T, opts cliOptions, request string) invocation {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(opts, strings.NewReader(request), &stdout, &stderr)
	return invocation{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// response decodes the single JSON object stdout is contractually required to
// carry. It fails the test when stdout holds anything else, which is the
// invariant most at risk from adding a logger.
func (inv invocation) response(t *testing.T) *Response {
	t.Helper()
	trimmed := strings.TrimSpace(inv.stdout)
	if trimmed == "" {
		t.Fatalf("stdout was empty; stderr: %s", inv.stderr)
	}
	if strings.Count(trimmed, "\n") != 0 {
		t.Fatalf("stdout carried more than one line — an SDK parses this stream:\n%s", inv.stdout)
	}
	var resp Response
	if err := json.Unmarshal([]byte(trimmed), &resp); err != nil {
		t.Fatalf("stdout is not one JSON object (%v):\n%s", err, inv.stdout)
	}
	return &resp
}

// logs decodes the JSON log records written to stderr.
func (inv invocation) logs(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(inv.stderr), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stderr line is not JSON (%v): %s", err, line)
		}
		out = append(out, m)
	}
	return out
}

// envOf builds an environment lookup for cliOptions. Tests inject one rather
// than calling os.Setenv, so the whole file stays safe to run in parallel — a
// shared process environment is the classic cause of a suite that passes alone
// and fails together.
func envOf(pairs map[string]string) func(string) string {
	return func(name string) string { return pairs[name] }
}

// translateRequest renders a request body with the shared test config.
func translateRequest(t *testing.T, question string, mutate func(map[string]any)) string {
	t.Helper()
	var cfg map[string]any
	if err := json.Unmarshal([]byte(testConfigJSON), &cfg); err != nil {
		t.Fatalf("test config: %v", err)
	}
	body := map[string]any{
		"op":      "translate",
		"backend": "sql",
		"config":  cfg,
		"query":   question,
	}
	if mutate != nil {
		mutate(body)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(raw)
}

// ─────────────────────────────────────────────────────────────────────────────
// Level resolution
// ─────────────────────────────────────────────────────────────────────────────

func TestParseLogLevel(t *testing.T) {
	ok := map[string]slog.Level{
		"off": levelOff, "none": levelOff, "silent": levelOff,
		"error": slog.LevelError,
		"warn":  slog.LevelWarn, "warning": slog.LevelWarn,
		"info":  slog.LevelInfo,
		"debug": slog.LevelDebug,
		// Case and surrounding space are a human typing at a shell, not a
		// mistake worth failing on.
		"DEBUG": slog.LevelDebug, "  Info  ": slog.LevelInfo,
	}
	for name, want := range ok {
		got, err := parseLogLevel(name)
		if err != nil {
			t.Errorf("parseLogLevel(%q) errored: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", name, got, want)
		}
	}

	// A typo must be an error, never a default. Defaulting to off hides
	// diagnostics from someone who explicitly asked for them; defaulting to
	// debug starts writing model output that nobody authorised.
	for _, bad := range []string{"debgu", "trace", "verbose", "1"} {
		if _, err := parseLogLevel(bad); err == nil {
			t.Errorf("parseLogLevel(%q) should have failed", bad)
		}
	}
}

func TestResolveLogLevelPrecedence(t *testing.T) {
	cases := []struct {
		name               string
		flag, env, request string
		want               slog.Level
		why                string
	}{
		{"nothing set", "", "", "", levelOff,
			"silence is the default; a subprocess must not start writing to stderr uninvited"},
		{"request only", "", "", "info", slog.LevelInfo,
			"an SDK never sees a command line, so this is its only channel"},
		{"env beats request", "", "debug", "info", slog.LevelDebug,
			"the environment is the operator's channel and must win over the application's default"},
		{"flag beats env", "warn", "debug", "info", slog.LevelWarn,
			"a person at a shell is the most specific intent"},
		{"request can turn it off", "", "", "off", levelOff,
			"an explicit off must be honoured, not treated as unset"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLogLevel(tc.flag, tc.env, tc.request)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("level = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// TestResolveLogLevelNamesTheBadChannel: three things can set the level, and an
// error that does not say which one was wrong sends someone checking all three.
func TestResolveLogLevelNamesTheBadChannel(t *testing.T) {
	cases := []struct{ flag, env, request, wantIn string }{
		{"nope", "", "", "--log-level"},
		{"", "nope", "", logLevelEnvVar},
		{"", "", "nope", "options.logLevel"},
	}
	for _, tc := range cases {
		_, err := resolveLogLevel(tc.flag, tc.env, tc.request)
		if err == nil {
			t.Fatalf("expected an error for %+v", tc)
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("error %q does not name the channel %q", err, tc.wantIn)
		}
	}
}

// TestBadLogLevelFailsFastWithAProtocolResponse: a configuration failure must
// fail fast (§11) AND keep the protocol's promise that stdout carries one
// well-formed JSON object. Exiting with a bare stderr message would satisfy the
// first and break the second.
func TestBadLogLevelFailsFastWithAProtocolResponse(t *testing.T) {
	inv := invoke(t, cliOptions{logLevel: "verbose"}, `{"op":"version"}`)

	if inv.code != exitFailure {
		t.Errorf("exit = %d, want %d", inv.code, exitFailure)
	}
	resp := inv.response(t)
	if resp.Success {
		t.Fatal("a bad log level must not report success")
	}
	if resp.Code != CodeInvalidRequest {
		t.Errorf("code = %q, want %q", resp.Code, CodeInvalidRequest)
	}
	if !strings.Contains(resp.Message, "verbose") {
		t.Errorf("message should quote the rejected value, got %q", resp.Message)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The stdout contract
// ─────────────────────────────────────────────────────────────────────────────

// TestLogsNeverTouchStdout is the single most important test in this file. One
// stray log line on stdout breaks every SDK simultaneously, and it would break
// them at the JSON decoder with an error that points nowhere near the logger.
func TestLogsNeverTouchStdout(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})

	for _, level := range []string{"error", "warn", "info", "debug"} {
		t.Run(level, func(t *testing.T) {
			inv := invoke(t, cliOptions{logLevel: level},
				translateRequest(t, "delivered orders", nil))

			resp := inv.response(t) // fails if stdout is not exactly one JSON object
			if !resp.Success {
				t.Fatalf("translate failed: %s", resp.Message)
			}
			if level == "debug" && inv.stderr == "" {
				t.Error("at debug the engine should have said something on stderr")
			}
		})
	}
}

// TestLoggingOffIsByteIdenticalToBefore: the default must leave stderr
// completely untouched. The SDKs surface stderr verbatim when a process dies,
// so an uninvited log line changes what every existing caller sees.
func TestLoggingOffIsByteIdenticalToBefore(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})

	inv := invoke(t, cliOptions{}, translateRequest(t, "delivered orders", nil))
	if inv.stderr != "" {
		t.Errorf("logging is off by default, but stderr got:\n%s", inv.stderr)
	}

	// And an explicit off, and an off arriving from the environment.
	for _, opts := range []cliOptions{
		{logLevel: "off"},
		{env: envOf(map[string]string{logLevelEnvVar: "off"})},
	} {
		if got := invoke(t, opts, translateRequest(t, "delivered orders", nil)).stderr; got != "" {
			t.Errorf("stderr should be empty, got:\n%s", got)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Outcome logging
// ─────────────────────────────────────────────────────────────────────────────

// TestEveryOutcomeIsLoggedExactlyOnce: no failure exits unlogged, and none is
// logged twice at successive layers. Both halves matter — the first is the
// no-silent-failures rule, the second is what keeps the stream readable.
func TestEveryOutcomeIsLoggedExactlyOnce(t *testing.T) {
	cases := []struct {
		name      string
		provider  qf.ModelProvider
		request   string
		wantLevel string
		wantCode  Code
	}{
		{
			name:      "success",
			provider:  &qf.StubProvider{Response: modelReply},
			request:   translateRequest(t, "delivered orders", nil),
			wantLevel: "INFO",
		},
		{
			name:      "unknown op",
			provider:  &qf.StubProvider{Response: modelReply},
			request:   `{"op":"transmogrify"}`,
			wantLevel: "ERROR",
			wantCode:  CodeUnknownOp,
		},
		{
			name:      "missing config",
			provider:  &qf.StubProvider{Response: modelReply},
			request:   `{"op":"translate","query":"x"}`,
			wantLevel: "ERROR",
			wantCode:  CodeInvalidRequest,
		},
		{
			name:      "unparseable config",
			provider:  &qf.StubProvider{Response: modelReply},
			request:   `{"op":"translate","query":"x","config":{"entity":"Order"}}`,
			wantLevel: "ERROR",
			wantCode:  CodeInvalidConfig,
		},
		{
			name:      "unknown backend",
			provider:  &qf.StubProvider{Response: modelReply},
			request:   translateRequest(t, "x", func(m map[string]any) { m["backend"] = "cassandra" }),
			wantLevel: "ERROR",
			wantCode:  CodeUnknownBackend,
		},
		{
			name:      "refusal is INFO, not ERROR",
			provider:  &qf.StubProvider{Response: `{"unsupported":"no field for warehouse"}`},
			request:   translateRequest(t, "orders by warehouse", nil),
			wantLevel: "INFO",
			wantCode:  CodeUnsupportedRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withProvider(t, tc.provider)
			inv := invoke(t, cliOptions{logLevel: "info"}, tc.request)

			// Exactly one line from the CLI component describing the outcome.
			var outcomes []map[string]any
			for _, rec := range inv.logs(t) {
				if rec[logKeyComponent] == logComponentCLI && rec[logKeyOutcome] != nil {
					outcomes = append(outcomes, rec)
				}
			}
			if len(outcomes) != 1 {
				t.Fatalf("expected exactly 1 outcome record, got %d; stderr:\n%s",
					len(outcomes), inv.stderr)
			}

			rec := outcomes[0]
			if rec["level"] != tc.wantLevel {
				t.Errorf("level = %v, want %v", rec["level"], tc.wantLevel)
			}
			if tc.wantCode != "" && rec[logKeyErrorCode] != string(tc.wantCode) {
				t.Errorf("error_code = %v, want %q", rec[logKeyErrorCode], tc.wantCode)
			}
			if _, ok := rec[logKeyDuration]; !ok {
				t.Error("every outcome line should carry duration_ms")
			}
			// The code on the log line and the code the SDK will raise must be
			// the same string, or a support conversation cannot be matched up.
			if tc.wantCode != "" {
				if got := inv.response(t).Code; got != tc.wantCode {
					t.Errorf("response code = %q, want %q", got, tc.wantCode)
				}
			}
		})
	}
}

// TestLoggingCarriesTheCrossLanguageFields pins the field names the Python and
// Java SDKs also emit.
func TestLoggingCarriesTheCrossLanguageFields(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})
	inv := invoke(t, cliOptions{logLevel: "info"},
		translateRequest(t, "delivered orders", func(m map[string]any) {
			m["options"] = map[string]any{"requestId": "req-abc-123"}
		}))

	recs := inv.logs(t)
	if len(recs) == 0 {
		t.Fatal("expected log output")
	}
	for _, rec := range recs {
		for _, key := range []string{logKeyLibrary, logKeyLanguage, logKeyVersion, logKeyRequestID} {
			if _, ok := rec[key]; !ok {
				t.Errorf("record is missing %q: %v", key, rec)
			}
		}
		if rec[logKeyLanguage] != "go" {
			t.Errorf("language = %v, want \"go\"", rec[logKeyLanguage])
		}
		if rec[logKeyRequestID] != "req-abc-123" {
			t.Errorf("request_id = %v, want the caller's id", rec[logKeyRequestID])
		}
	}
}

// TestEngineEventsReachTheLog: the CLI wires the library's Observer seam into
// its logger with SetObserver, so the engine's own attempt/translate events —
// and the provider's latency and token counts — appear alongside the CLI's
// outcome line. Assigning Engine.Observe instead would silently drop the
// provider half, which is the expensive half.
func TestEngineEventsReachTheLog(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})
	inv := invoke(t, cliOptions{logLevel: "debug"}, translateRequest(t, "delivered orders", nil))

	var sawEngine bool
	for _, rec := range inv.logs(t) {
		if rec[logKeyComponent] == "engine" {
			sawEngine = true
		}
	}
	if !sawEngine {
		t.Errorf("no component=engine records; the Observer seam is not wired up:\n%s", inv.stderr)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Request-id sanitization
// ─────────────────────────────────────────────────────────────────────────────

func TestSanitizeRequestID(t *testing.T) {
	cases := []struct{ in, want, why string }{
		{"", "", "empty stays empty"},
		{"8f3a-1c2d-4e5f", "8f3a-1c2d-4e5f", "a UUID-shaped id passes through"},
		{"trace:abc/1.2_3", "trace:abc/1.2_3", "the punctuation real trace ids use is allowed"},
		{
			"good\nlevel=ERROR msg=\"forged\"", "goodlevelERRORmsgforged",
			"a newline would let a caller forge a second log record",
		},
		{"tab\there", "tabhere", "control characters are dropped"},
		{"esc\x1b[31m", "esc31m", "a terminal escape must not survive into a log someone cats"},
	}
	for _, tc := range cases {
		if got := sanitizeRequestID(tc.in); got != tc.want {
			t.Errorf("sanitizeRequestID(%q) = %q, want %q — %s", tc.in, got, tc.want, tc.why)
		}
	}

	// Bounded, so the field cannot be used to write a megabyte per request into
	// someone's log index.
	long := strings.Repeat("a", maxRequestIDLength*3)
	if got := sanitizeRequestID(long); len(got) != maxRequestIDLength {
		t.Errorf("length = %d, want %d", len(got), maxRequestIDLength)
	}
}

// TestForgedRequestIDCannotInjectALogRecord is the end-to-end version of the
// above: a caller-controlled id goes straight into a log sink, so a newline in
// it must not become a second record.
func TestForgedRequestIDCannotInjectALogRecord(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})
	inv := invoke(t, cliOptions{logLevel: "info"},
		translateRequest(t, "delivered orders", func(m map[string]any) {
			m["options"] = map[string]any{
				"requestId": "x\n{\"level\":\"ERROR\",\"msg\":\"FORGED\"}",
			}
		}))

	for _, rec := range inv.logs(t) {
		if rec["msg"] == "FORGED" {
			t.Fatalf("a forged record was injected through request_id:\n%s", inv.stderr)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Privacy canaries
//
// Same reasoning as the library's: this binary is a subprocess whose stderr the
// host will collect, so anything it writes is in practice going into a shared
// index. Each canary is planted where a naive implementation would echo it.
// ─────────────────────────────────────────────────────────────────────────────

// TestNeverLogsTheQuestion: the natural-language question is user data.
func TestNeverLogsTheQuestion(t *testing.T) {
	const canary = "CANARY-QUESTION-cli-51ab"

	for _, tc := range []struct {
		name     string
		provider qf.ModelProvider
	}{
		{"success", &qf.StubProvider{Response: modelReply}},
		{"parse failure", &qf.StubProvider{Response: "not json"}},
		{"refusal", &qf.StubProvider{Response: `{"unsupported":"nope"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withProvider(t, tc.provider)
			inv := invoke(t, cliOptions{logLevel: "debug"},
				translateRequest(t, "orders for "+canary, nil))
			if strings.Contains(inv.stderr, canary) {
				t.Errorf("the question reached the log:\n%s", inv.stderr)
			}
		})
	}
}

// TestNeverLogsScopeValues: scope values are tenant and user ids. Only the keys
// may be logged — and they must be, or an audit trail cannot say which filters
// were forced onto a query.
func TestNeverLogsScopeValues(t *testing.T) {
	const canary = "CANARY-TENANT-cli-77de"

	withProvider(t, &qf.StubProvider{Response: modelReply})
	inv := invoke(t, cliOptions{logLevel: "debug"},
		translateRequest(t, "delivered orders", func(m map[string]any) {
			m["scope"] = map[string]any{"customerName": canary}
		}))

	if strings.Contains(inv.stderr, canary) {
		t.Errorf("a scope VALUE reached the log:\n%s", inv.stderr)
	}
	if !strings.Contains(inv.stderr, "customerName") {
		t.Errorf("the scope KEY should be logged for the audit trail:\n%s", inv.stderr)
	}
}

// TestNeverLogsTheConfig: a config carries physical table and column names,
// which plenty of organisations treat as confidential. Its SHAPE — entity and
// field count — is what gets logged instead.
func TestNeverLogsTheConfig(t *testing.T) {
	withProvider(t, &qf.StubProvider{Response: modelReply})
	inv := invoke(t, cliOptions{logLevel: "debug"}, translateRequest(t, "delivered orders", nil))

	// "total_amount" is a physical column name in testConfigJSON, and
	// "internalNote" a field the config deliberately hides from results.
	for _, secret := range []string{"total_amount", "customer_name", "internalNote"} {
		if strings.Contains(inv.stderr, secret) {
			t.Errorf("config content %q reached the log:\n%s", secret, inv.stderr)
		}
	}
	// The shape must be there, or the log cannot tell two configs apart.
	if !strings.Contains(inv.stderr, `"entity":"Order"`) {
		t.Errorf("the entity should be logged:\n%s", inv.stderr)
	}
	if !strings.Contains(inv.stderr, logKeyFieldsN) {
		t.Errorf("the field count should be logged:\n%s", inv.stderr)
	}
}
