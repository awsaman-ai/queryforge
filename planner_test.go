package queryforge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func plannerTestConfig(t *testing.T) *Config { return mustParse(t, genConfigJSON) }

// TestSystemPromptContent checks the config is faithfully injected and that an
// excluded field never leaks into the prompt.
func TestSystemPromptContent(t *testing.T) {
	c := mustParse(t, `{
      "entity":"Order","model":{},
      "fields":[
        {"name":"status","type":"enum","values":["PLACED","DELIVERED"],"operators":["equals","in"],"synonyms":["state"]},
        {"name":"secret","type":"string","queryable":false}
      ]
    }`)
	pl := NewPlanner(c, &StubProvider{})
	prompt := pl.SystemPrompt(time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC))

	for _, want := range []string{"Entity: Order", "2026-07-28", "status (enum)", "PLACED,DELIVERED", "operators=[equals, in]", "synonyms=[state]"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "secret") {
		t.Errorf("excluded field leaked into prompt:\n%s", prompt)
	}
}

// TestPlanHappyPath feeds a canned AST through the stub and checks parsing.
func TestPlanHappyPath(t *testing.T) {
	c := plannerTestConfig(t)
	stub := &StubProvider{Response: canonicalAST}
	pl := NewPlanner(c, stub)

	ast, raw, err := pl.Plan(context.Background(), "delivered orders last 30 days", RepairHint{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if raw == "" {
		t.Errorf("raw output should be returned for logging")
	}
	if ast.Entity != "Order" || ast.Filter == nil || ast.Filter.Type != CondLogical {
		t.Errorf("parsed AST wrong: %+v", ast)
	}
	if stub.LastUser != "Request: delivered orders last 30 days" {
		t.Errorf("user prompt wrong: %q", stub.LastUser)
	}
}

// TestParseASTTolerant checks that fences and surrounding prose are tolerated.
func TestParseASTTolerant(t *testing.T) {
	c := plannerTestConfig(t)
	cases := []string{
		"```json\n{\"entity\":\"Order\",\"limit\":10}\n```",
		"Sure! Here is the AST:\n{\"entity\":\"Order\",\"limit\":10}\nHope that helps.",
		"{\"entity\":\"Order\",\"limit\":10}",
	}
	for _, raw := range cases {
		q, err := parseAST(raw, c)
		if err != nil {
			t.Errorf("parseAST(%q) errored: %v", raw, err)
			continue
		}
		if q.Entity != "Order" || q.Limit == nil || *q.Limit != 10 {
			t.Errorf("parseAST(%q) wrong: %+v", raw, q)
		}
	}
}

// TestParseASTDefaults checks version/entity defaulting for terse output.
func TestParseASTDefaults(t *testing.T) {
	c := plannerTestConfig(t)
	q, err := parseAST(`{"filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"PLACED"}}}`, c)
	if err != nil {
		t.Fatalf("parseAST: %v", err)
	}
	if q.Version != ASTVersion {
		t.Errorf("version not defaulted: %q", q.Version)
	}
	if q.Entity != "Order" {
		t.Errorf("entity not defaulted: %q", q.Entity)
	}
}

// TestParseASTGarbage is the worst case: no JSON at all.
func TestParseASTGarbage(t *testing.T) {
	c := plannerTestConfig(t)
	if _, err := parseAST("I cannot help with that.", c); err == nil {
		t.Error("expected error on non-JSON output")
	}
	if _, err := parseAST("{not valid json}", c); err == nil {
		t.Error("expected error on malformed JSON")
	}
}

// TestPlanProviderError checks provider failures propagate.
func TestPlanProviderError(t *testing.T) {
	c := plannerTestConfig(t)
	pl := NewPlanner(c, &StubProvider{Err: errors.New("boom")})
	if _, _, err := pl.Plan(context.Background(), "x", RepairHint{}); err == nil {
		t.Error("expected provider error to propagate")
	}
}

// TestRepairHintInPrompt checks the repair hint reaches the user prompt.
func TestRepairHintInPrompt(t *testing.T) {
	c := plannerTestConfig(t)
	stub := &StubProvider{Response: canonicalAST}
	pl := NewPlanner(c, stub)
	_, _, _ = pl.Plan(context.Background(), "orders", RepairHint{Kind: RepairValidation, Message: `unknown field "stat"`})
	if !strings.Contains(stub.LastUser, "failed validation") || !strings.Contains(stub.LastUser, `unknown field "stat"`) {
		t.Errorf("repair hint not in prompt: %q", stub.LastUser)
	}
}

// TestExtractJSONObjectMalformedBraces covers BUG-008: Gemini's
// OpenAI-compatibility endpoint returns brace-unbalanced JSON, so the extractor
// must ignore trailing junk without inventing structure for an unclosed object.
func TestExtractJSONObjectMalformedBraces(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// The exact live failure: a spurious extra '}' after a complete object.
		// "first brace to last brace" would swallow it and fail to parse.
		{"trailing extra brace", "{\n  \"unsupported\": \"nope\"\n}\n}", "{\n  \"unsupported\": \"nope\"\n}"},
		{"prose after object", `{"a":1} hope that helps!`, `{"a":1}`},
		{"nested object intact", `{"a":{"b":2}}`, `{"a":{"b":2}}`},
		// Braces inside strings are data and must not close the object early.
		{"brace inside string", `{"a":"}"}`, `{"a":"}"}`},
		{"escaped quote then brace", `{"a":"say \"}\" ok"}`, `{"a":"say \"}\" ok"}`},
		{"code fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		// The other live variant: the object never closes. Guessing where it
		// should end could silently produce a query the user never asked for.
		{"unclosed object", `{"a":1`, ""},
		{"unclosed nested", `{"a":{"b":1`, ""},
		{"no object at all", `I cannot help with that`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractJSONObject(tc.in); got != tc.want {
				t.Errorf("extractJSONObject(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestJSONModeDefaultsOff pins BUG-008's other half: response_format is not sent
// unless the config opts in, because it makes Gemini emit unbalanced JSON.
func TestJSONModeDefaultsOff(t *testing.T) {
	if NewOpenAIProvider(ModelConfig{BaseURL: "http://x", Model: "m"}).JSONMode {
		t.Error("JSONMode must default to off")
	}
	on := true
	if !NewOpenAIProvider(ModelConfig{BaseURL: "http://x", Model: "m", JSONMode: &on}).JSONMode {
		t.Error("jsonMode:true in config must enable JSONMode")
	}
}
