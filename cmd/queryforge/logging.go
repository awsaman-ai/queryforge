package main

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// Structured logging for the engine binary.
//
// The library it wraps deliberately never logs (see observe.go in the parent
// package). This binary is not a library — it is an application, and an
// application that produces no diagnostics is one nobody can debug in
// production. So logging lives here, at the process boundary, where there is a
// single owner for destination, format and level.
//
// THREE RULES, all of them load-bearing:
//
//  1. LOGS GO TO STDERR. Never stdout. stdout carries exactly one JSON object —
//     the response — and that is the entire contract every SDK parses against.
//     One stray log line on stdout breaks all of them at once. TestLogsNeverTouchStdout
//     pins this.
//
//  2. OFF BY DEFAULT. A library's subprocess that starts writing to stderr
//     uninvited changes what every existing caller sees; the SDKs surface stderr
//     verbatim in a crash message. The host opts in — by flag, by environment,
//     or through the SDK — and gets silence otherwise.
//
//  3. NEVER THE QUESTION, NEVER THE SCOPE VALUES, NEVER THE CONFIG. The
//     natural-language question is user data; scope values are tenant, user and
//     subscription ids; the config can carry table and column names a customer
//     considers confidential. What is logged instead is shape: the entity name,
//     the backend, how many fields, which scope KEYS. Pinned by canary tests in
//     logging_test.go.
// ─────────────────────────────────────────────────────────────────────────────

// logLevelEnvVar is the deployment's channel for turning diagnostics on. It is
// the same spelling the other QueryForge surfaces use.
const logLevelEnvVar = "QUERYFORGE_LOG_LEVEL"

// levelOff disables logging entirely. It is a real level rather than a nil
// logger so that every call site stays unconditional — no `if logger != nil`
// scattered through the request path, which is exactly where a missing nil
// check eventually panics.
//
// slog has no "off", so this is one step above the highest real level; a
// handler thresholded here discards everything, including Error.
const levelOff = slog.LevelError + 4

// parseLogLevel maps a level name onto its slog.Level.
//
// An unrecognised name is an ERROR, not a default. Both plausible defaults are
// actively harmful: falling back to `off` hides diagnostics from someone who
// explicitly asked for them and will conclude the feature is broken, and
// falling back to `debug` starts writing model output that a typo did not
// authorise. Failing fast — §11 of the design brief — is the only answer that
// cannot mislead.
func parseLogLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "off", "none", "silent":
		return levelOff, nil
	case "error":
		return slog.LevelError, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	default:
		return levelOff, fmt.Errorf(
			"unknown log level %q (expected one of: off, error, warn, info, debug)", name)
	}
}

// resolveLogLevel picks the effective level from the three channels that can
// set it, in this precedence:
//
//	--log-level flag  >  QUERYFORGE_LOG_LEVEL  >  request options.logLevel  >  off
//
// The ordering is deliberate and is about who can reach which channel. The flag
// is a person at a shell and is always the most specific intent. The
// environment is the DEPLOYMENT's channel — the one an operator can change on a
// running container without shipping code — so it outranks the request option,
// which is the application's compiled-in default. That ordering is what makes
// "set QUERYFORGE_LOG_LEVEL=debug and reproduce it" a workable instruction for
// an operator debugging someone else's application.
//
// Each channel is validated on its own, so a bad value in one is reported
// naming that channel rather than surfacing as a mysterious silence.
func resolveLogLevel(flagValue, envValue, requestValue string) (slog.Level, error) {
	for _, src := range []struct{ origin, value string }{
		{"--log-level", flagValue},
		{logLevelEnvVar, envValue},
		{`options.logLevel`, requestValue},
	} {
		if strings.TrimSpace(src.value) == "" {
			continue
		}
		level, err := parseLogLevel(src.value)
		if err != nil {
			return levelOff, fmt.Errorf("%s: %w", src.origin, err)
		}
		return level, nil
	}
	return levelOff, nil
}

// newLogger builds the process logger: line-delimited JSON on the given writer,
// which is always stderr in production.
//
// JSON rather than slog's text format because these lines exist to be shipped
// to an aggregator. A human running the binary by hand can pipe it through jq;
// a machine parsing logfmt-with-quoted-spaces cannot be made reliable.
//
// Time is left to the handler's default (RFC3339 with nanoseconds, key "time"),
// and source location is off: the useful location is the `component` and
// `operation` attributes, not a file:line inside this binary.
func newLogger(w io.Writer, level slog.Level, version string) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})
	// Attributes every record carries. Attaching them once here rather than at
	// each site is what guarantees they are never forgotten on the one line
	// somebody eventually greps for.
	return slog.New(handler).With(
		logKeyLibrary, logLibraryName,
		logKeyLanguage, logLanguageName,
		logKeyVersion, version,
	)
}

// discardLogger returns a logger that drops everything. Used when the level is
// off, so the rest of the binary needs no special case.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: levelOff}))
}

// Attribute keys. They mirror the Go library's (slog.go), the Python SDK's and
// the Java SDK's, because the whole point is that one saved search works across
// all four surfaces.
const (
	logKeyLibrary   = "library"
	logKeyLanguage  = "language"
	logKeyVersion   = "version"
	logKeyComponent = "component"
	logKeyOperation = "operation"
	logKeyRequestID = "request_id"
	logKeyBackend   = "backend"
	logKeyEntity    = "entity"
	logKeyFieldsN   = "config_fields"
	logKeyDuration  = "duration_ms"
	logKeyErrorCode = "error_code"
	logKeyOutcome   = "outcome"
	logKeyScopeKeys = "scope_keys"
)

const (
	logLibraryName  = "queryforge"
	logLanguageName = "go"

	// logComponentCLI marks the lines this binary writes on its own account, as
	// distinct from the ones forwarded out of the library by SlogObserver, which
	// carry component=engine or component=provider.
	logComponentCLI = "cli"
)

// maxRequestIDLength bounds a caller-supplied correlation id.
//
// 128 characters comfortably fits a UUID, a W3C trace id, or a hand-written
// label, and stops a caller from using the field to write a megabyte into
// someone's log index on every request.
const maxRequestIDLength = 128

// sanitizeRequestID makes a caller-supplied correlation id safe to log.
//
// The id arrives as arbitrary caller text and goes straight into a log record,
// which is a text sink that other tools will parse. Anything outside a
// conservative allow-list is dropped rather than escaped: an id is a label, so
// there is no legitimate reason for it to contain a newline (log injection — a
// forged second record), a control character (terminal escape sequences, when
// someone cats the log), or a quote.
//
// Dropping rather than rejecting is deliberate. A correlation id is a debugging
// convenience; failing the caller's actual query because their trace id had an
// odd character in it would be a worse outcome than logging a slightly shortened
// id. An id that sanitizes away to nothing is simply omitted.
func sanitizeRequestID(id string) string {
	if id == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range id {
		if b.Len() >= maxRequestIDLength {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/':
			b.WriteRune(r)
		default:
			// Silently dropped. See the note above on why this is not an error.
		}
	}
	return b.String()
}

// sortedKeys returns a scope map's field NAMES, sorted.
//
// Names only, never values — a scope value is a tenant, user or subscription
// id, which is the single most sensitive thing this process handles. Sorted so
// that two log lines for the same call shape are byte-comparable, which matters
// when diffing a working request against a broken one.
//
// It takes map[string]any rather than qf.Scope so the privacy-critical helper
// has no dependency on the library's types and can be read in isolation.
func sortedKeys(m map[string]any) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
