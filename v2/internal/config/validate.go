package config

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"queryforge.local/v2experiment/internal/qferr"
)

// Rule names a validation rule. Rules are a closed set so that tests — and the
// Phase 10 failure taxonomy — can assert on WHY a config was rejected rather
// than pattern-matching prose. The Phase 1 exit criterion is that every invalid
// fixture is rejected "for the expected reason", which is only checkable if the
// reason has a name.
type Rule string

const (
	RuleNoEntities            Rule = "no_entities"
	RuleMissingEntityName     Rule = "missing_entity_name"
	RuleDuplicateEntity       Rule = "duplicate_entity"
	RuleMissingFieldName      Rule = "missing_field_name"
	RuleMissingFieldEntity    Rule = "missing_field_entity"
	RuleUnknownEntity         Rule = "unknown_entity"
	RuleDuplicateField        Rule = "duplicate_field"
	RuleUnsupportedFieldType  Rule = "unsupported_field_type"
	RuleOperatorTypeMismatch  Rule = "operator_type_mismatch"
	RuleUnknownOperator       Rule = "unknown_operator"
	RuleAggregationMismatch   Rule = "aggregation_type_mismatch"
	RuleUnknownAggregation    Rule = "unknown_aggregation"
	RuleInvalidIdentifier     Rule = "invalid_identifier"
	RuleMalformedValueAlias   Rule = "malformed_value_alias"
	RuleEnumValues            Rule = "enum_values"
	RuleMissingPhrases        Rule = "missing_phrases"
	RuleDuplicateRelationship Rule = "duplicate_relationship"
	RuleUnknownRelationTarget Rule = "unknown_relationship_target"
	RuleInvalidCardinality    Rule = "invalid_cardinality"
	RuleRelationshipTypes     Rule = "relationship_type_mismatch"
	RuleDuplicatePattern      Rule = "duplicate_pattern"
	RuleUnknownPatternIntent  Rule = "unknown_pattern_intent"
	RuleInvalidPatternOrder   Rule = "invalid_pattern_order"
	RuleInvalidRanking        Rule = "invalid_ranking_thresholds"
	RuleInvalidLimit          Rule = "invalid_limit"
)

// Warning rules (Phase 8, decisions 016 and 019 §4). A warning never rejects
// a config: it reports a phrase the engine will treat as ambiguous, or can
// never match, so the author can fix it. Reported by internal/configlint,
// which needs the corpus and ranking's own agreement rule and so cannot live
// in this package without an import cycle.
const (
	// RuleSharedAcrossKinds: one phrase is configured on targets of different
	// semantic kinds that ranking does not treat as the same reading —
	// "product" on an entity and a relationship.
	RuleSharedAcrossKinds Rule = "phrase_shared_across_kinds"
	// RulePhraseNeverForms: a configured phrase contains words Phase 3 always
	// reads as a number or a date ("one of"), so it can never be matched as
	// written.
	RulePhraseNeverForms Rule = "phrase_never_forms"
)

// NewWarning builds a warning. Arguments that are config VALUES must be
// wrapped in qferr.Sensitive, exactly as for a violation: Detail redacts them.
func NewWarning(rule Rule, path, format string, args ...any) Violation {
	return Violation{Rule: rule, Path: path, format: format, args: args}
}

// Violation is one broken rule, located at a path into the config.
//
// It stores its message unrendered so that the same violation can be printed
// two ways: redacted for default output, revealed under an explicit debug mode.
// Any argument wrapped in qferr.Sensitive is a config VALUE and is replaced by a
// placeholder in the default rendering — see the contract in
// internal/qferr/sensitive.go.
type Violation struct {
	Rule Rule
	// Path locates the problem the way a config author reads the file:
	// "fields[2] (customer.country)", "relationships[0].to".
	Path string

	format string
	args   []any
}

// Detail explains the failure with config values redacted. This is the only
// rendering that may reach default output.
func (v Violation) Detail() string { return qferr.Format(v.format, v.args...) }

// DetailRevealed explains the failure with config values shown.
//
// DEBUG ONLY. Callers must gate this behind an explicit flag.
func (v Violation) DetailRevealed() string { return qferr.FormatRevealed(v.format, v.args...) }

// HasRedactedValues reports whether this violation withheld anything, so a
// debug-mode caller knows there is more to see.
func (v Violation) HasRedactedValues() bool { return qferr.HasSensitive(v.args...) }

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s: %s", v.Path, v.Rule, v.Detail())
}

// StringRevealed is String with config values shown. DEBUG ONLY.
func (v Violation) StringRevealed() string {
	return fmt.Sprintf("%s: %s: %s", v.Path, v.Rule, v.DetailRevealed())
}

// identifierPattern is what may be emitted as a SQL identifier.
//
// Deliberately narrower than what Postgres would accept quoted: ASCII letters,
// digits and underscore, not starting with a digit. Section 16.2 requires that
// identifiers come only from validated config, and the cheapest way to keep that
// true is to allow only shapes that need no quoting and cannot carry a quote, a
// semicolon or whitespace in the first place.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxIdentifierLength is the PostgreSQL limit. Longer identifiers are silently
// truncated by the server, which would make two distinct configured columns
// collide into one — a wrong-answer bug, so it is rejected here instead.
const maxIdentifierLength = 63

// Validate checks a config against every rule and returns all violations at
// once, as a single qferr.CodeConfigError.
//
// Reporting every problem rather than the first is a deliberate cost: config
// authoring is iterative, and a validator that reveals one mistake per run turns
// a ten-error config into ten edit-run cycles.
func Validate(cfg *Config) error {
	v := &validator{cfg: cfg}
	v.run()
	if len(v.violations) == 0 {
		return nil
	}

	// Sorted so the message is byte-identical across runs. Go map iteration is
	// randomised, and several rules below walk maps; without this the same bad
	// config would produce differently ordered output each time and undermine
	// the determinism the experiment has to demonstrate (section 25).
	sort.Slice(v.violations, func(i, j int) bool {
		a, b := v.violations[i], v.violations[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Detail() < b.Detail()
	})

	return &ValidationError{
		coded:      qferr.New(qferr.CodeConfigError, "%s", renderViolations(v.violations, false)),
		Violations: v.violations,
	}
}

// renderViolations builds the multi-problem message. With reveal set, config
// values are shown; that rendering is for --debug only.
func renderViolations(violations []Violation, reveal bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "config is invalid (%d problem", len(violations))
	if len(violations) > 1 {
		b.WriteString("s")
	}
	b.WriteString("):")

	redacted := 0
	for _, viol := range violations {
		if reveal {
			fmt.Fprintf(&b, "\n  - %s", viol.StringRevealed())
			continue
		}
		fmt.Fprintf(&b, "\n  - %s", viol.String())
		if viol.HasRedactedValues() {
			redacted++
		}
	}

	// Saying that something was withheld, and how to see it, is the difference
	// between a redacted message and a mysteriously vague one.
	if redacted > 0 {
		fmt.Fprintf(&b, "\n(%d problem(s) withheld a config value; re-run with --debug to see them)", redacted)
	}
	return b.String()
}

// ValidationError carries the structured violations alongside the coded error,
// so tests and tooling can inspect rules without parsing the rendered message.
//
// The coded error is unexported and reached through Unwrap rather than embedded:
// a field named Error would shadow the Error() method the error interface needs.
type ValidationError struct {
	coded      *qferr.Error
	Violations []Violation
}

func (e *ValidationError) Error() string { return e.coded.Error() }

// ErrorRevealed renders the same failures with config values shown.
//
// DEBUG ONLY. The CLI reaches this only under --debug; nothing else should.
func (e *ValidationError) ErrorRevealed() string {
	return fmt.Sprintf("%s: %s", qferr.CodeConfigError, renderViolations(e.Violations, true))
}

// RedactedValueCount reports how many violations withheld a value.
func (e *ValidationError) RedactedValueCount() int {
	n := 0
	for _, v := range e.Violations {
		if v.HasRedactedValues() {
			n++
		}
	}
	return n
}

// Unwrap exposes the coded error, so qferr.CodeOf and errors.As find
// CONFIG_ERROR through a ValidationError exactly as they would through any
// other failure in the pipeline.
func (e *ValidationError) Unwrap() error { return e.coded }

// Code returns the error code, which is always qferr.CodeConfigError.
func (e *ValidationError) Code() qferr.Code { return e.coded.Code }

// Rules returns the distinct rules broken, sorted. Convenience for tests and for
// the Phase 10 failure taxonomy.
func (e *ValidationError) Rules() []Rule {
	seen := make(map[Rule]bool, len(e.Violations))
	out := make([]Rule, 0, len(e.Violations))
	for _, v := range e.Violations {
		if !seen[v.Rule] {
			seen[v.Rule] = true
			out = append(out, v.Rule)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// HasRule reports whether r was among the violations.
func (e *ValidationError) HasRule(r Rule) bool {
	for _, v := range e.Violations {
		if v.Rule == r {
			return true
		}
	}
	return false
}

type validator struct {
	cfg        *Config
	violations []Violation
}

// add records a violation. The message is stored unrendered: wrap any config
// VALUE in qferr.Sensitive and it redacts itself in default output while staying
// available under --debug.
func (v *validator) add(rule Rule, path, format string, args ...any) {
	v.violations = append(v.violations, Violation{Rule: rule, Path: path, format: format, args: args})
}

func (v *validator) run() {
	entities := v.checkEntities()
	fields := v.checkFields(entities)
	v.checkOperatorPhrases()
	v.checkAggregationPhrases()
	v.checkRelationships(entities, fields)
	v.checkPatterns(entities)
	v.checkSecurity()
	v.checkRanking()
}

// checkEntities validates entities and returns the set of known names, which the
// later checks need in order to report "unknown entity" usefully.
func (v *validator) checkEntities() map[string]bool {
	if len(v.cfg.Entities) == 0 {
		v.add(RuleNoEntities, "entities", "config declares no entities; at least one is required")
		return map[string]bool{}
	}

	known := make(map[string]bool, len(v.cfg.Entities))
	for i := range v.cfg.Entities {
		e := &v.cfg.Entities[i]
		path := fmt.Sprintf("entities[%d]", i)

		if strings.TrimSpace(e.Name) == "" {
			v.add(RuleMissingEntityName, path, "entity has no name")
			// Without a name nothing else about this entity can be reported
			// against something the author can find in the file.
			continue
		}
		path = fmt.Sprintf("entities[%d] (%s)", i, e.Name)

		if known[e.Name] {
			v.add(RuleDuplicateEntity, path, "entity %q is declared more than once", e.Name)
		}
		known[e.Name] = true

		v.checkIdentifier(path, "table", e.Table)
	}
	return known
}

// fieldKey identifies a field across the checks that follow.
type fieldKey struct{ entity, field string }

func (v *validator) checkFields(entities map[string]bool) map[fieldKey]*Field {
	byKey := make(map[fieldKey]*Field, len(v.cfg.Fields))

	for i := range v.cfg.Fields {
		f := &v.cfg.Fields[i]
		path := fmt.Sprintf("fields[%d]", i)

		if strings.TrimSpace(f.Name) == "" {
			v.add(RuleMissingFieldName, path, "field has no name")
			continue
		}
		if strings.TrimSpace(f.Entity) == "" {
			v.add(RuleMissingFieldEntity, path, "field %q does not say which entity it belongs to", f.Name)
			continue
		}
		path = fmt.Sprintf("fields[%d] (%s.%s)", i, f.Entity, f.Name)

		if !entities[f.Entity] {
			v.add(RuleUnknownEntity, path, "field belongs to unknown entity %q (known: %s)",
				f.Entity, joinSorted(entities))
			// The remaining checks are still worth running: a field on a
			// mistyped entity usually has other problems too, and reporting
			// them together saves a round trip.
		}

		key := fieldKey{f.Entity, f.Name}
		if _, dup := byKey[key]; dup {
			v.add(RuleDuplicateField, path, "field %q is declared more than once on entity %q", f.Name, f.Entity)
		}
		byKey[key] = f

		v.checkIdentifier(path, "column", f.Column)
		v.checkFieldType(path, f)
		v.checkFieldOperators(path, f)
		v.checkFieldAggregations(path, f)
		v.checkEnumValues(path, f)
		v.checkValueAliases(path, f)
	}
	return byKey
}

func (v *validator) checkFieldType(path string, f *Field) {
	if strings.TrimSpace(string(f.Type)) == "" {
		v.add(RuleUnsupportedFieldType, path, "field has no type (supported: %s)", joinTypes())
		return
	}
	if !ValidFieldType(f.Type) {
		v.add(RuleUnsupportedFieldType, path, "field type %q is not supported (supported: %s)", f.Type, joinTypes())
	}
}

func (v *validator) checkFieldOperators(path string, f *Field) {
	// An unsupported type has no compatibility matrix, so every operator would
	// be reported as incompatible. That noise would bury the real problem.
	typeKnown := ValidFieldType(f.Type)

	for _, op := range f.Operators {
		if !ValidOperator(op) {
			v.add(RuleUnknownOperator, path, "operator %q is not a supported operator (supported: %s)",
				op, joinOperators(Operators()))
			continue
		}
		if typeKnown && !OperatorAllowed(f.Type, op) {
			v.add(RuleOperatorTypeMismatch, path,
				"operator %q cannot be applied to a %s field (allowed for %s: %s)",
				op, f.Type, f.Type, joinOperators(OperatorsFor(f.Type)))
		}
	}
}

func (v *validator) checkFieldAggregations(path string, f *Field) {
	typeKnown := ValidFieldType(f.Type)

	for _, agg := range f.Aggregations {
		if !ValidAggregation(agg) {
			v.add(RuleUnknownAggregation, path, "aggregation %q is not supported (supported: %s)",
				agg, joinAggregations(Aggregations()))
			continue
		}
		if typeKnown && !AggregationAllowed(f.Type, agg) {
			v.add(RuleAggregationMismatch, path,
				"aggregation %q cannot be applied to a %s field (allowed for %s: %s)",
				agg, f.Type, f.Type, joinAggregations(AggregationsFor(f.Type)))
		}
	}
}

func (v *validator) checkEnumValues(path string, f *Field) {
	if f.Type == TypeEnum {
		if len(f.EnumValues) == 0 {
			v.add(RuleEnumValues, path, "enum field declares no values; the closed set is what makes it an enum")
			return
		}
		seen := make(map[string]bool, len(f.EnumValues))
		for _, val := range f.EnumValues {
			if strings.TrimSpace(val) == "" {
				v.add(RuleEnumValues, path, "enum field has a blank value")
				continue
			}
			// Case-insensitively, because matching a user's "CANCELLED" against
			// "cancelled" is the entire point; two values differing only by case
			// would make that match ambiguous.
			lower := strings.ToLower(val)
			if seen[lower] {
				v.add(RuleEnumValues, path, "enum field declares %s more than once (ignoring case)",
					qferr.Sensitive(val))
			}
			seen[lower] = true
		}
		return
	}
	if len(f.EnumValues) > 0 {
		v.add(RuleEnumValues, path, "only an enum field may declare enumValues; this field is %q", f.Type)
	}
}

func (v *validator) checkValueAliases(path string, f *Field) {
	if len(f.ValueAliases) == 0 {
		return
	}

	// Aliases map language to a stored value, which only makes sense where the
	// stored value is a name. On a number or a date the useful mapping is a
	// parse ("fifty thousand" → 50000, "last year" → a range), which is the NLP
	// layer's job in Phase 3 — not a per-field lookup table.
	if f.Type != TypeString && f.Type != TypeEnum {
		v.add(RuleMalformedValueAlias, path,
			"valueAliases are only supported on string and enum fields; this field is %q", f.Type)
		return
	}

	enum := make(map[string]bool, len(f.EnumValues))
	for _, val := range f.EnumValues {
		enum[strings.ToLower(val)] = true
	}

	// Sorted because this walks a map and every violation it raises must appear
	// in a stable order.
	aliases := make([]string, 0, len(f.ValueAliases))
	for alias := range f.ValueAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	// Keyed by the lowercased alias, holding the first spelling seen, so a
	// case collision can name both halves under --debug.
	seen := make(map[string]string, len(aliases))
	for _, alias := range aliases {
		target := f.ValueAliases[alias]
		switch {
		case strings.TrimSpace(alias) == "":
			// No value worth naming: the key is empty.
			v.add(RuleMalformedValueAlias, path, "a value alias has a blank key")
		case strings.TrimSpace(target) == "":
			v.add(RuleMalformedValueAlias, path, "value alias %s has a blank target",
				qferr.Sensitive(alias))
		}

		lower := strings.ToLower(strings.TrimSpace(alias))
		if lower != "" {
			if first, dup := seen[lower]; dup {
				// Alias matching is case-insensitive, so two keys differing only
				// by case are a genuine ambiguity, not a duplicate to dedupe.
				v.add(RuleMalformedValueAlias, path, "value aliases %s and %s differ only by case",
					qferr.Sensitive(first), qferr.Sensitive(alias))
			} else {
				seen[lower] = alias
			}
		}

		// On an enum the alias must land on a declared value; otherwise the
		// alias resolves to something the field can never hold.
		if f.Type == TypeEnum && len(enum) > 0 && strings.TrimSpace(target) != "" && !enum[strings.ToLower(target)] {
			v.add(RuleMalformedValueAlias, path, "value alias %s points at %s, which is not declared in enumValues",
				qferr.Sensitive(alias), qferr.Sensitive(target))
		}
	}
}

func (v *validator) checkOperatorPhrases() {
	ops := make([]Operator, 0, len(v.cfg.OperatorPhrases))
	for op := range v.cfg.OperatorPhrases {
		ops = append(ops, op)
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i] < ops[j] })

	for _, op := range ops {
		path := fmt.Sprintf("operators[%s]", op)
		if !ValidOperator(op) {
			v.add(RuleUnknownOperator, path, "%q is not a supported operator (supported: %s)",
				op, joinOperators(Operators()))
			continue
		}
		v.checkPhrases(path, v.cfg.OperatorPhrases[op], "operator")
	}
}

func (v *validator) checkAggregationPhrases() {
	aggs := make([]Aggregation, 0, len(v.cfg.AggregationPhrases))
	for agg := range v.cfg.AggregationPhrases {
		aggs = append(aggs, agg)
	}
	sort.Slice(aggs, func(i, j int) bool { return aggs[i] < aggs[j] })

	for _, agg := range aggs {
		path := fmt.Sprintf("aggregations[%s]", agg)
		if !ValidAggregation(agg) {
			v.add(RuleUnknownAggregation, path, "%q is not a supported aggregation (supported: %s)",
				agg, joinAggregations(Aggregations()))
			continue
		}
		v.checkPhrases(path, v.cfg.AggregationPhrases[agg], "aggregation")
	}
}

// checkPhrases rejects an empty or blank phrase list. A declared key with no
// phrases is almost always a half-finished edit, and it silently makes that
// operator or aggregation unreachable from natural language — a failure that
// would otherwise surface much later as an unexplained accuracy hole.
func (v *validator) checkPhrases(path string, phrases []string, kind string) {
	if len(phrases) == 0 {
		v.add(RuleMissingPhrases, path, "%s is declared with no phrases, so nothing can ever match it", kind)
		return
	}
	for _, p := range phrases {
		if strings.TrimSpace(p) == "" {
			v.add(RuleMissingPhrases, path, "%s has a blank phrase", kind)
		}
	}
}

func (v *validator) checkRelationships(entities map[string]bool, fields map[fieldKey]*Field) {
	seen := make(map[string]bool, len(v.cfg.Relationships))

	for i := range v.cfg.Relationships {
		r := &v.cfg.Relationships[i]
		path := fmt.Sprintf("relationships[%d]", i)
		if strings.TrimSpace(r.Name) != "" {
			path = fmt.Sprintf("relationships[%d] (%s)", i, r.Name)
			if seen[r.Name] {
				v.add(RuleDuplicateRelationship, path, "relationship %q is declared more than once", r.Name)
			}
			seen[r.Name] = true
		}

		if !ValidCardinality(r.Cardinality) {
			detail := fmt.Sprintf("cardinality %q is not supported (supported: %s)",
				r.Cardinality, joinCardinalities())
			if r.Cardinality == "many-to-many" {
				// Worth naming explicitly: it is the one a config author is most
				// likely to reach for, and "unsupported" alone would read as an
				// oversight rather than a scope decision.
				detail += "; many-to-many needs a junction table, which this relationship model does not express and the first experiment does not support"
			}
			v.add(RuleInvalidCardinality, path, "%s", detail)
		}

		from := v.checkEndpoint(path+".from", r.From, entities, fields)
		to := v.checkEndpoint(path+".to", r.To, entities, fields)

		// A join compares two columns, so their types have to match. Catching it
		// here turns a class of silently-empty result sets into a config error.
		if from != nil && to != nil && from.Type != to.Type && ValidFieldType(from.Type) && ValidFieldType(to.Type) {
			v.add(RuleRelationshipTypes, path,
				"join keys have different types: %s.%s is %q but %s.%s is %q",
				r.From.Entity, r.From.Field, from.Type, r.To.Entity, r.To.Field, to.Type)
		}
	}
}

// checkEndpoint validates one side of a relationship and returns the field it
// resolves to, or nil if it does not resolve.
func (v *validator) checkEndpoint(path string, ep Endpoint, entities map[string]bool, fields map[fieldKey]*Field) *Field {
	if strings.TrimSpace(ep.Entity) == "" {
		v.add(RuleUnknownRelationTarget, path, "endpoint names no entity")
		return nil
	}
	if strings.TrimSpace(ep.Field) == "" {
		v.add(RuleUnknownRelationTarget, path, "endpoint names no field")
		return nil
	}
	if !entities[ep.Entity] {
		v.add(RuleUnknownRelationTarget, path, "endpoint points at unknown entity %q (known: %s)",
			ep.Entity, joinSorted(entities))
		return nil
	}
	f, ok := fields[fieldKey{ep.Entity, ep.Field}]
	if !ok {
		v.add(RuleUnknownRelationTarget, path, "endpoint points at unknown field %q on entity %q",
			ep.Field, ep.Entity)
		return nil
	}
	return f
}

func (v *validator) checkPatterns(entities map[string]bool) {
	seen := make(map[string]bool, len(v.cfg.QueryPatterns))

	for i := range v.cfg.QueryPatterns {
		p := &v.cfg.QueryPatterns[i]
		path := fmt.Sprintf("queryPatterns[%d]", i)
		if strings.TrimSpace(p.Name) != "" {
			path = fmt.Sprintf("queryPatterns[%d] (%s)", i, p.Name)
			if seen[p.Name] {
				v.add(RuleDuplicatePattern, path, "query pattern %q is declared more than once", p.Name)
			}
			seen[p.Name] = true
		}

		if !ValidPatternIntent(p.Intent) {
			v.add(RuleUnknownPatternIntent, path, "intent %q is not supported (supported: %s)",
				p.Intent, joinIntents())
		}
		v.checkPhrases(path, p.Phrases, "query pattern")

		// Order is optional (decision 015), but when present it must be a real
		// direction on a pattern that ranks. On any other intent it would be a
		// key nothing reads, which is a config mistake worth reporting.
		switch {
		case p.Order == "":
		case p.Order != OrderAsc && p.Order != OrderDesc:
			v.add(RuleInvalidPatternOrder, path, "order %q is not a direction (use %q or %q)", p.Order, OrderAsc, OrderDesc)
		case p.Intent != IntentTopN:
			v.add(RuleInvalidPatternOrder, path, "order applies only to top_n patterns, not to intent %q", p.Intent)
		}

		if strings.TrimSpace(p.Entity) != "" && !entities[p.Entity] {
			v.add(RuleUnknownEntity, path, "pattern is scoped to unknown entity %q (known: %s)",
				p.Entity, joinSorted(entities))
		}
	}
}

// checkSecurity rejects negative limits. Zero is not an error: Normalize reads
// it as "unset" and substitutes the default, so a config can set only the limits
// it cares about.
func (v *validator) checkSecurity() {
	limits := []struct {
		name  string
		value int
	}{
		{"maxInputLength", v.cfg.Security.MaxInputLength},
		{"maxPhrases", v.cfg.Security.MaxPhrases},
		{"maxCandidates", v.cfg.Security.MaxCandidates},
		{"maxJoins", v.cfg.Security.MaxJoins},
		{"maxFilters", v.cfg.Security.MaxFilters},
		{"maxNestingDepth", v.cfg.Security.MaxNestingDepth},
		{"maxQueryLength", v.cfg.Security.MaxQueryLength},
		{"maxPlanningMillis", v.cfg.Security.MaxPlanningMillis},
	}
	for _, l := range limits {
		if l.value < 0 {
			v.add(RuleInvalidLimit, "security."+l.name, "limit must not be negative")
		}
	}
}

// checkIdentifier validates a resolved SQL identifier.
//
// The identifier itself is NOT wrapped in qferr.Sensitive. It is schema
// structure rather than stored data, and it is the one thing the author needs
// in order to find the offending line — a redacted "identifier <redacted> must
// match ..." would be unactionable. See the contract in qferr/sensitive.go.
//
// A blank value is a violation, not a skip. Normalize defaults table and column
// from the entity or field name, and the callers here only run once that name is
// known to be non-blank — so blank at this point means either that Normalize was
// never called or that the default was overridden with an empty string. Letting
// it through would hand the compiler an empty identifier to splice into FROM.
func (v *validator) checkIdentifier(path, kind, value string) {
	if strings.TrimSpace(value) == "" {
		v.add(RuleInvalidIdentifier, path, "%s identifier is empty", kind)
		return
	}
	if len(value) > maxIdentifierLength {
		v.add(RuleInvalidIdentifier, path,
			"%s identifier is %d characters, over the %d-character limit; PostgreSQL truncates longer names, which can silently merge two different columns",
			kind, len(value), maxIdentifierLength)
		return
	}
	if !identifierPattern.MatchString(value) {
		v.add(RuleInvalidIdentifier, path,
			"%s identifier %q must match %s; set it explicitly if the natural name is not a legal identifier",
			kind, value, identifierPattern.String())
	}
}

func joinSorted(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

func joinTypes() string {
	out := make([]string, 0, len(FieldTypes()))
	for _, t := range FieldTypes() {
		out = append(out, string(t))
	}
	return strings.Join(out, ", ")
}

func joinOperators(ops []Operator) string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, string(op))
	}
	return strings.Join(out, ", ")
}

func joinAggregations(aggs []Aggregation) string {
	out := make([]string, 0, len(aggs))
	for _, a := range aggs {
		out = append(out, string(a))
	}
	return strings.Join(out, ", ")
}

func joinCardinalities() string {
	out := make([]string, 0, len(Cardinalities()))
	for _, c := range Cardinalities() {
		out = append(out, string(c))
	}
	return strings.Join(out, ", ")
}

func joinIntents() string {
	out := make([]string, 0, len(PatternIntents()))
	for _, i := range PatternIntents() {
		out = append(out, string(i))
	}
	return strings.Join(out, ", ")
}

// checkRanking bounds the optional ranking thresholds. The ranges are the ones
// ranking.Policy.Validate enforces (config cannot import ranking): a minScore
// at or below 0 accepts noise, one above 2 can never be met; a margin lives in
// [0, 1]. Checked here so a bad value is a CONFIG_ERROR at load, not a
// failure on the first query.
func (v *validator) checkRanking() {
	r := v.cfg.Ranking
	if r.MinScore != nil && (math.IsNaN(*r.MinScore) || *r.MinScore <= 0 || *r.MinScore > 2) {
		v.add(RuleInvalidRanking, "ranking.minScore", "minScore must be in (0, 2], got %g", *r.MinScore)
	}
	if r.MinMargin != nil && (math.IsNaN(*r.MinMargin) || *r.MinMargin < 0 || *r.MinMargin > 1) {
		v.add(RuleInvalidRanking, "ranking.minMargin", "minMargin must be in [0, 1], got %g", *r.MinMargin)
	}
}
