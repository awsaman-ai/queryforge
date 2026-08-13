package queryforge

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Classify
//
// These tests are about ONE property: an error that reaches a caller must carry
// the code that names what actually went wrong. The pipeline wraps errors
// several deep — a budget-exhausted failure wraps the last validation error,
// which wraps a ValidationErrors — so "the code is right" is a claim about
// unwrapping order, not about a switch statement.
// ─────────────────────────────────────────────────────────────────────────────

func TestClassifyNilIsNotAFailure(t *testing.T) {
	// A nil error must not classify as INTERNAL. Callers stamp the code onto a
	// log line unconditionally, and a success reporting error_code=INTERNAL is
	// worse than no code at all.
	if got := Classify(nil); got != FailureNone {
		t.Errorf("Classify(nil) = %q, want %q", got, FailureNone)
	}
}

func TestClassifySentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureCode
	}{
		{
			name: "scope",
			err:  fmt.Errorf("scope field %q: %w", "tenantId", ErrScope),
			want: FailureInvalidScope,
		},
		{
			name: "model output",
			err:  fmt.Errorf("planner: %w", ErrModelOutput),
			want: FailureModelOutput,
		},
		{
			name: "model transport",
			err:  fmt.Errorf("planner: %w", ErrModelTransport),
			want: FailureModelTransport,
		},
		{
			name: "refusal",
			err:  &UnsupportedRequestError{Reason: "no field for warehouse"},
			want: FailureUnsupported,
		},
		{
			name: "wrapped refusal",
			err:  fmt.Errorf("plan: %w", &UnsupportedRequestError{Reason: "no field for warehouse"}),
			want: FailureUnsupported,
		},
		{
			name: "validation findings",
			err:  ValidationErrors{{Code: CodeUnknownField, Path: "$", Field: "stat"}},
			want: FailureValidation,
		},
		{
			name: "deadline",
			err:  context.DeadlineExceeded,
			want: FailureTimeout,
		},
		{
			name: "unrecognised",
			err:  errors.New("something nobody named"),
			want: FailureInternal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyOrderingIsLoadBearing pins the four precedence decisions that make
// Classify correct rather than merely plausible. Each case is an error that
// matches TWO branches; the test asserts the one a person would want.
func TestClassifyOrderingIsLoadBearing(t *testing.T) {
	t.Run("deadline beats transport", func(t *testing.T) {
		// The engine surfaces a cancelled model call as a transport failure. If
		// transport won, the caller would be told to check their API key when
		// the real fix is a longer timeout.
		err := fmt.Errorf("provider: request failed: %w: %w", context.DeadlineExceeded, ErrModelTransport)
		if got := Classify(err); got != FailureTimeout {
			t.Errorf("got %q, want %q — a deadline must not be reported as an unreachable model", got, FailureTimeout)
		}
	})

	t.Run("refusal beats validation", func(t *testing.T) {
		// A refusal is a well-formed answer to show the user; a validation
		// failure is feedback about a config gap. Reporting a refusal as
		// VALIDATION_FAILED would send someone editing a config for a question
		// that was answered correctly.
		// Both are wrapped, so both are reachable by errors.As — which is the
		// point: the test is about which one Classify picks, not about which
		// one is present.
		err := fmt.Errorf("%w (after %w)", &UnsupportedRequestError{Reason: "no warehouse field"},
			ValidationErrors{{Code: CodeUnknownField}})
		if got := Classify(err); got != FailureUnsupported {
			t.Errorf("got %q, want %q", got, FailureUnsupported)
		}
	})

	t.Run("validation survives the budget-exhausted wrapper", func(t *testing.T) {
		// This is the real shape produced by Translate when every repair attempt
		// failed validation. Reporting it as INTERNAL — which is what happens if
		// the wrapper is not unwrapped — hides a config problem behind "a bug in
		// QueryForge".
		inner := ValidationErrors{{Code: CodeUnknownField, Field: "stat"}}
		err := fmt.Errorf("translation failed validation after 3 attempt(s); last error: %w", inner)
		if got := Classify(err); got != FailureValidation {
			t.Errorf("got %q, want %q", got, FailureValidation)
		}
	})

	t.Run("scope beats validation", func(t *testing.T) {
		// A bad scope is the calling application's bug and should page whoever
		// wired up the tenancy filters; a validation failure is ordinary
		// feedback about a question. They must not collapse into one code.
		err := fmt.Errorf("%w: %w", ErrScope, ValidationErrors{{Code: CodeKindMismatch}})
		if got := Classify(err); got != FailureInvalidScope {
			t.Errorf("got %q, want %q", got, FailureInvalidScope)
		}
	})
}

// TestRetryableIsConservative pins which codes invite a retry. Getting this
// wrong in the generous direction is expensive — a client retrying a
// VALIDATION_FAILED pays for three identical model calls to reach the same
// answer — so the test enumerates the whole vocabulary rather than spot-checking.
func TestRetryableIsConservative(t *testing.T) {
	retryable := map[FailureCode]bool{
		FailureModelOutput:    true,
		FailureModelTransport: true,
	}
	all := []FailureCode{
		FailureNone, FailureInvalidRequest, FailureInvalidConfig, FailureUnknownBackend,
		FailureInvalidScope, FailureValidation, FailureUnsupported, FailureModelOutput,
		FailureModelTransport, FailureGenerate, FailureTimeout, FailureInternal,
	}
	for _, code := range all {
		if got, want := code.Retryable(), retryable[code]; got != want {
			t.Errorf("%q.Retryable() = %v, want %v", code, got, want)
		}
	}
}

// TestClassifyMatchesTheEngineEndToEnd is the test that would actually catch a
// regression: it runs real translations through a real Engine and checks the
// code on the error the caller receives. A unit test over hand-built errors can
// pass while the engine wraps something differently.
func TestClassifyMatchesTheEngineEndToEnd(t *testing.T) {
	cases := []struct {
		name     string
		provider ModelProvider
		backend  string
		scope    Scope
		want     FailureCode
	}{
		{
			name:     "unreachable model",
			provider: &erroringProvider{err: fmt.Errorf("dial tcp: %w", ErrModelTransport)},
			backend:  "sql",
			want:     FailureModelTransport,
		},
		{
			name:     "model never produced parseable output",
			provider: &StubProvider{Response: unparseableReply},
			backend:  "sql",
			want:     FailureModelOutput,
		},
		{
			name:     "model never produced a conforming AST",
			provider: &StubProvider{Response: invalidAST},
			backend:  "sql",
			want:     FailureValidation,
		},
		{
			name:     "model refused",
			provider: &StubProvider{Response: refusalReply},
			backend:  "sql",
			want:     FailureUnsupported,
		},
		{
			name:     "bad scope",
			provider: &StubProvider{Response: validAST},
			backend:  "sql",
			scope:    Scope{"status": "NOT_A_STATUS"},
			want:     FailureInvalidScope,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEngine(t, tc.provider)
			_, err := e.Translate(context.Background(), "delivered orders", tc.backend, tc.scope)
			if err == nil {
				t.Fatal("expected a failure, got a result — a failing translation must never return a query")
			}
			if got := Classify(err); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
}
