package queryforge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PromptVersion identifies the prompt template. Bumping it lets prompt changes
// be A/B tested and rolled back, and it is cheap to log per request.
const PromptVersion = "qf-planner-v2"

// Sentinel errors that classify a failed Plan call. The engine's repair loop
// keys off these: a reply we received but could not read is worth asking again
// (models occasionally emit truncated or double-brace JSON), whereas never
// reaching the model at all will not fix itself on a retry.
var (
	// ErrModelTransport means the model was not reached or refused the call —
	// network failure, auth rejection, retired model, quota block. Not repairable.
	ErrModelTransport = errors.New("model transport failure")

	// ErrModelOutput means the model answered but the reply could not be parsed
	// into an AST. Repairable: resend with a hint describing what was wrong.
	ErrModelOutput = errors.New("model output not parseable")
)

// UnsupportedRequestError reports that the request cannot be expressed with the
// vocabulary this config exposes — the user asked about something the config
// does not describe. It is a definitive answer, not a failure to retry: the
// model declined on purpose rather than substituting a field that does exist.
// Returning it is strictly safer than compiling a query against a guessed field,
// which would hand back confident, silently wrong rows.
type UnsupportedRequestError struct {
	Reason string // the model's short explanation, e.g. "no field for shipping warehouse"
}

// Error renders the refusal.
func (e *UnsupportedRequestError) Error() string {
	if e.Reason == "" {
		return "request is not supported by this config"
	}
	return "request is not supported by this config: " + e.Reason
}

// RepairKind says why a previous attempt was rejected, so the retry prompt can
// tell the model something actionable instead of a generic "try again".
type RepairKind int

const (
	RepairNone       RepairKind = iota // first attempt: no prior failure
	RepairValidation                   // parsed fine, but broke a config rule
	RepairParse                        // the reply was not usable JSON
)

// RepairHint carries the previous failure into the next prompt.
type RepairHint struct {
	Kind    RepairKind // what went wrong last time
	Message string     // the underlying error text, fed back verbatim
}

// Planner is the only AI-touching component. It builds a prompt entirely from
// the config (the config IS the prompt: which fields, operators, and enum
// values the model may use), asks the model for a JSON AST, and parses that
// text back into a *Query. It performs no validation itself — the deterministic
// validator is the guarantee — so the Planner's job ends at "produce a
// candidate AST".
type Planner struct {
	Config   *Config          // vocabulary + model selection
	Provider ModelProvider    // how the model is reached
	Now      func() time.Time // injectable clock (relative dates, tests)
}

// NewPlanner wires a planner to a config and provider.
func NewPlanner(c *Config, p ModelProvider) *Planner {
	return &Planner{Config: c, Provider: p, Now: func() time.Time { return time.Now().UTC() }}
}

// Plan converts natural-language text into a candidate AST. hint is empty on the
// first attempt; on a repair attempt it carries the previous validation error so
// the model can correct itself. It returns the parsed AST and the raw model text
// (useful for logging/telemetry).
func (pl *Planner) Plan(ctx context.Context, text string, hint RepairHint) (*Query, string, error) {
	system := pl.SystemPrompt(pl.now()) // stable shell + injected config
	user := buildUserPrompt(text, hint) // the request (+ optional repair hint)

	raw, err := pl.Provider.Complete(ctx, system, user)
	if err != nil {
		// Tagged as transport so the caller fails fast instead of burning the
		// repair budget on an endpoint that is not answering.
		return nil, "", fmt.Errorf("planner: model call failed: %w: %w", ErrModelTransport, err)
	}

	ast, err := parseAST(raw, pl.Config)
	if err != nil {
		// A deliberate refusal is a real answer: pass it through untagged so it
		// is never mistaken for a malformed reply and retried.
		var unsupported *UnsupportedRequestError
		if errors.As(err, &unsupported) {
			return nil, raw, err
		}
		return nil, raw, fmt.Errorf("planner: %w: %w", ErrModelOutput, err)
	}
	return ast, raw, nil
}

// now returns the planner's reference time (defaults to wall clock).
func (pl *Planner) now() time.Time {
	if pl.Now != nil {
		return pl.Now()
	}
	return time.Now().UTC()
}

// SystemPrompt renders the stable instruction shell with the config's vocabulary
// injected. Only metadata is ever placed here — never data values or PII.
func (pl *Planner) SystemPrompt(now time.Time) string {
	c := pl.Config
	var b strings.Builder

	// Role + hard rules.
	b.WriteString("You translate a user's natural-language request into a JSON Query AST.\n")
	b.WriteString("Return ONLY the JSON object — no prose, no markdown fences.\n")
	b.WriteString("This is a read-only query builder: never invent fields, operators, or enum values.\n\n")

	// Entity + today's date (for relative dates).
	fmt.Fprintf(&b, "Entity: %s\n", c.Entity)
	fmt.Fprintf(&b, "Today (UTC): %s\n", now.Format("2006-01-02"))
	b.WriteString("For relative times like \"last 30 days\", use a relative_date value: {\"kind\":\"relative_date\",\"unit\":\"day\",\"amount\":-30}. Units: minute, hour, day, week, month, year.\n\n")

	// Fields the model may use (queryable only — excluded fields are hidden).
	b.WriteString("Allowed fields:\n")
	for i := range c.Fields {
		f := &c.Fields[i]
		if !f.EffectiveQueryable() {
			continue // never expose an excluded field to the model
		}
		b.WriteString(describeFieldForPrompt(f))
	}
	b.WriteString("\n")

	// The AST shape, shown by example.
	b.WriteString("AST shape (example):\n")
	b.WriteString(astShapeExample)
	b.WriteString("\nValue kinds: string, number, boolean, enum, array, date, relative_date.\n")
	b.WriteString("Combine predicates with logical nodes: {\"type\":\"logical\",\"op\":\"AND|OR|NOT\",\"children\":[...]}.\n")
	b.WriteString("Omit any part you don't need (filter, sort, limit, select).\n\n")

	// Refusal contract. Without this the model will happily map an unlisted
	// concept onto whichever listed field looks closest (observed: "shipping
	// warehouse" and "courier" both collapsed onto a generic tags field), which
	// validates cleanly and returns silently wrong rows. Declining is the only
	// safe answer, so the prompt gives it an explicit way to decline.
	b.WriteString("If the request needs a field, operator, or enum value that is NOT listed above, do not substitute a different field and do not guess.\n")
	b.WriteString("Instead return exactly: {\"unsupported\":\"<short reason naming what is missing>\"}\n")
	b.WriteString("Only decline for genuinely missing vocabulary — a phrasing that matches a listed field's synonyms is supported.\n")
	return b.String()
}

// describeFieldForPrompt renders one field's contract as a compact line so the
// model sees exactly what it may emit for that field.
func describeFieldForPrompt(f *Field) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("- %s (%s)", f.Name, f.Type)) // name + type

	if f.Type == FieldEnum && len(f.Values) > 0 { // enum domain
		parts = append(parts, "values=["+strings.Join(f.Values, ",")+"]")
	}
	parts = append(parts, "operators=["+joinOperators(f.EffectiveOperators())+"]") // legal operators
	if len(f.Synonyms) > 0 {                                                       // phrasings that map here
		parts = append(parts, "synonyms=["+strings.Join(f.Synonyms, ",")+"]")
	}
	if !f.EffectiveSortable() { // tell the model it cannot sort by this
		parts = append(parts, "not-sortable")
	}
	return strings.Join(parts, " ") + "\n"
}

// astShapeExample is a compact, canonical AST used to anchor the model's output.
//
// The third predicate earns its place: it is the only one carrying an array
// value. Five operators require one — between, in, notIn, containsAny,
// containsAll — and with every example predicate holding a scalar, the model had
// never been shown the shape those operators need. It then invented a scalar
// instead, which the validator rejected with `operator "between" expects an
// array value, got "number"`. Measured on gemini-3.1-flash-lite over 18
// questions x 5 runs: `between` failed 10/10 without this predicate and 0/10
// with it, taking overall accuracy from 88.9% to 100%. Anything removing it
// should expect that regression back.
const astShapeExample = `{"version":"1.0","entity":"Order","filter":{"type":"logical","op":"AND","children":[{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}},{"type":"comparison","field":"createdAt","operator":"after","value":{"kind":"relative_date","unit":"day","amount":-30}},{"type":"comparison","field":"amount","operator":"between","value":{"kind":"array","v":[100,500]}}]},"sort":[{"field":"createdAt","dir":"DESC"}],"limit":50}`

// buildUserPrompt assembles the user turn: the request, plus a repair hint when
// a prior attempt failed. The two failure kinds get different advice — a schema
// violation needs the rule it broke, whereas an unreadable reply needs to be
// told about the output format, not about fields.
func buildUserPrompt(text string, hint RepairHint) string {
	if hint.Kind == RepairNone || hint.Message == "" {
		return "Request: " + text
	}
	if hint.Kind == RepairParse {
		return "Request: " + text +
			"\n\nYour previous reply could not be parsed as JSON: " + hint.Message +
			"\nReturn exactly one JSON object and nothing else — no prose, no markdown fences, no trailing text."
	}
	return "Request: " + text +
		"\n\nYour previous JSON failed validation: " + hint.Message +
		"\nReturn corrected JSON only."
}

// parseAST extracts the JSON object from the model's raw text and decodes it
// into a Query. It tolerates code fences and surrounding prose by slicing from
// the first '{' to the last '}'. Defaults are filled for version/entity so a
// terse model output still forms a complete AST.
func parseAST(raw string, c *Config) (*Query, error) {
	js := extractJSONObject(raw)
	if js == "" {
		return nil, fmt.Errorf("no JSON object found in model output")
	}
	// Check for a deliberate refusal before decoding an AST. The model returns
	// {"unsupported":"…"} when the request needs vocabulary the config does not
	// define; that object would otherwise decode into an empty Query and compile
	// into an unfiltered "return everything" query — the worst possible reading
	// of "I can't answer this".
	var refusal struct {
		Unsupported string `json:"unsupported"`
	}
	if err := json.Unmarshal([]byte(js), &refusal); err == nil && strings.TrimSpace(refusal.Unsupported) != "" {
		return nil, &UnsupportedRequestError{Reason: strings.TrimSpace(refusal.Unsupported)}
	}

	var q Query
	// Lenient decode: ignore any extra keys the model may add (e.g. a stray
	// "explanation"). Semantic correctness is the validator's job, not the
	// parser's.
	if err := json.Unmarshal([]byte(js), &q); err != nil {
		return nil, fmt.Errorf("model output was not a valid AST: %w", err)
	}
	if q.Version == "" { // default the version if the model omitted it
		q.Version = ASTVersion
	}
	if q.Entity == "" && c != nil { // default the entity to the config's
		q.Entity = c.Entity
	}
	return &q, nil
}

// extractJSONObject returns the first complete, brace-balanced JSON object in
// the model's reply, after stripping markdown code fences. Returns "" when no
// complete object is present.
//
// It scans for the matching close rather than taking the last '}' in the string,
// because "first brace to last brace" breaks on trailing junk: Gemini's
// OpenAI-compatibility endpoint sometimes appends a spurious extra '}', which
// that approach would include, yielding "invalid character '}' after top-level
// value". Counting depth ignores anything after the object closes.
//
// Braces inside string literals are skipped, so a value like "}" cannot end the
// object early. If the depth never returns to zero the object is incomplete; we
// return "" rather than inventing the missing braces, because guessing the
// structure of a truncated filter could silently produce a valid-looking query
// that means something the user never asked for.
func extractJSONObject(s string) string {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "```") { // strip ```json ... ``` fences if present
		s = strings.ReplaceAll(s, "```json", "")
		s = strings.ReplaceAll(s, "```JSON", "")
		s = strings.ReplaceAll(s, "```", "")
		s = strings.TrimSpace(s)
	}

	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}

	depth := 0        // open-brace count at the current position
	inString := false // true while inside a "…" literal
	escaped := false  // true when the previous char was a backslash
	for i := start; i < len(s); i++ {
		ch := s[i]

		if escaped { // this char is escaped: consume it literally
			escaped = false
			continue
		}
		if ch == '\\' && inString { // start of an escape sequence
			escaped = true
			continue
		}
		if ch == '"' { // enter or leave a string literal
			inString = !inString
			continue
		}
		if inString { // braces inside strings are data, not structure
			continue
		}

		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 { // the object that opened at start just closed
				return s[start : i+1]
			}
		}
	}
	return "" // unbalanced: the object never closed
}
