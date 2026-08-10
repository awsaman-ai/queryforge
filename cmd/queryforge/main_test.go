package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exercise runs the process body over an input string and returns the decoded
// response plus the exit code and stderr. Every test here goes through this, so
// each one is asserting on the same thing an SDK actually sees.
func exercise(t *testing.T, input string) (*Response, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run("", false, strings.NewReader(input), &stdout, &stderr)

	// The contract is one JSON object and nothing else. Decoding with a streaming
	// decoder and then asserting the stream is exhausted checks both halves: that
	// what came out parses, and that nothing followed it.
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("stdout is not a single JSON object (%v); got: %q", err, stdout.String())
	}
	if dec.More() {
		t.Errorf("stdout carried more than one JSON value: %q", stdout.String())
	}
	return &resp, code, stderr.String()
}

// TestExitCodes: 0 for a success, 1 for a well-formed failure. An SDK does not
// depend on this — it reads the JSON — but a shell user does, and getting it
// backwards would make the binary unusable in a script.
func TestExitCodes(t *testing.T) {
	if _, code, _ := exercise(t, `{"op":"version"}`); code != exitOK {
		t.Errorf("successful request exited %d, want %d", code, exitOK)
	}
	if _, code, _ := exercise(t, `{"op":"nope"}`); code != exitFailure {
		t.Errorf("failed request exited %d, want %d", code, exitFailure)
	}
}

// TestFailuresStillProduceJSON is the load-bearing promise of this protocol: an
// SDK reads stdout unconditionally, so every failure mode must leave a
// parseable object there rather than an empty pipe and a message on stderr.
func TestFailuresStillProduceJSON(t *testing.T) {
	cases := map[string]struct {
		input string
		code  Code
	}{
		"empty input":          {``, CodeInvalidRequest},
		"whitespace only":      {"  \n\t ", CodeInvalidRequest},
		"not json":             {`hello`, CodeInvalidRequest},
		"truncated json":       {`{"op":"version"`, CodeInvalidRequest},
		"json array":           {`[{"op":"version"}]`, CodeInvalidRequest},
		"json string":          {`"version"`, CodeInvalidRequest},
		"json null":            {`null`, CodeInvalidRequest},
		"op is a number":       {`{"op":123}`, CodeInvalidRequest},
		"unknown op":           {`{"op":"drop"}`, CodeUnknownOp},
		"missing op":           {`{"backend":"sql"}`, CodeInvalidRequest},
		"misspelled field":     {`{"op":"version","backends":"sql"}`, CodeInvalidRequest},
		"trailing object":      {`{"op":"version"}{"op":"version"}`, CodeInvalidRequest},
		"scope wrong type":     {`{"op":"generate","scope":"tenant-1"}`, CodeInvalidRequest},
		"options wrong type":   {`{"op":"generate","options":[]}`, CodeInvalidRequest},
		"ast is not an object": {`{"op":"generate","ast":"SELECT 1"}`, CodeInvalidRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp, code, _ := exercise(t, tc.input)
			if resp.Success {
				t.Fatalf("expected failure, got success: %+v", resp)
			}
			if resp.Code != tc.code {
				t.Errorf("code = %s, want %s (message: %s)", resp.Code, tc.code, resp.Message)
			}
			if resp.Message == "" {
				t.Error("failure carried no message")
			}
			if code == exitOK {
				t.Error("a failure exited 0")
			}
		})
	}
}

// TestUnknownFieldIsRejected earns its own test because the alternative — the
// standard library's default of silently dropping unknown fields — is the worst
// failure this protocol could have. A misspelled "scop" would produce a
// perfectly valid query built without the tenancy filter, and nothing in the
// output would look wrong.
func TestUnknownFieldIsRejected(t *testing.T) {
	resp, _, _ := exercise(t, `{"op":"generate","scop":{"tenantId":"t1"}}`)
	if resp.Success {
		t.Fatal("a misspelled field was silently ignored")
	}
	if resp.Code != CodeInvalidRequest {
		t.Errorf("code = %s, want %s", resp.Code, CodeInvalidRequest)
	}
	if !strings.Contains(resp.Message, "scop") {
		t.Errorf("message does not name the offending field: %s", resp.Message)
	}
}

// TestNothingButJSONOnStdout: any diagnostic that escaped onto stdout would
// corrupt the stream for every SDK at once, so assert stdout is byte-for-byte a
// JSON object even on the paths that write to stderr.
func TestNothingButJSONOnStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run("/nonexistent/request.json", false, strings.NewReader(""), &stdout, &stderr)
	if stdout.Len() != 0 {
		t.Errorf("a pre-protocol failure wrote to stdout: %q", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Error("a pre-protocol failure was silent on stderr")
	}
}

// TestRequestFromFile covers the --request path, which exists for debugging and
// for platforms where piping to stdin is awkward.
func TestRequestFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "req.json")
	if err := os.WriteFile(path, []byte(`{"op":"version"}`), 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := run(path, false, strings.NewReader(""), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var resp Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.Op != OpVersion {
		t.Errorf("unexpected response: %+v", resp)
	}
}

// TestMissingRequestFile exits with the protocol code, not the failure code:
// the caller's invocation was wrong, and no response was produced at all.
func TestMissingRequestFile(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(filepath.Join(t.TempDir(), "absent.json"), false, strings.NewReader(""), &stdout, &stderr); code != exitProtocol {
		t.Errorf("exit = %d, want %d", code, exitProtocol)
	}
}

// TestOversizedRequestIsRefused: the cap has to produce a clean error rather
// than an out-of-memory kill, and it has to be a JSON one like everything else.
func TestOversizedRequestIsRefused(t *testing.T) {
	// One byte over the limit, built as valid JSON so the size check is what
	// rejects it rather than the parser.
	padding := strings.Repeat("x", maxRequestBytes)
	resp, code, _ := exercise(t, `{"op":"version","query":"`+padding+`"}`)
	if resp.Success {
		t.Fatal("an oversized request was accepted")
	}
	if resp.Code != CodeInvalidRequest {
		t.Errorf("code = %s, want %s", resp.Code, CodeInvalidRequest)
	}
	if code == exitOK {
		t.Error("an oversized request exited 0")
	}
}

// TestPrettyIsStillOneObject: --pretty is a human affordance, but indented
// output must remain a single parseable object in case an SDK is pointed at a
// wrapper script that sets it.
func TestPrettyIsStillOneObject(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run("", true, strings.NewReader(`{"op":"version"}`), &stdout, &stderr)
	if !strings.Contains(stdout.String(), "\n  ") {
		t.Error("--pretty produced no indentation")
	}
	var resp Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("pretty output does not parse: %v", err)
	}
}

// TestResponseIsNewlineTerminated: json.Encoder appends one, and the SDKs read
// the whole stream to EOF rather than a line — but a caller that does read a
// line must not block forever, so pin the behaviour.
func TestResponseIsNewlineTerminated(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run("", false, strings.NewReader(`{"op":"version"}`), &stdout, &stderr)
	if !strings.HasSuffix(stdout.String(), "\n") {
		t.Errorf("response is not newline-terminated: %q", stdout.String())
	}
	if strings.Count(strings.TrimSuffix(stdout.String(), "\n"), "\n") != 0 {
		t.Errorf("compact response spans multiple lines: %q", stdout.String())
	}
}
