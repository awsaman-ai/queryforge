package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/qferr"
)

func fixture(name string) string {
	return filepath.Join("..", "..", "testdata", "configs", name)
}

// loadInvalid loads a fixture that must fail validation and returns the
// structured error.
func loadInvalid(t *testing.T, name string) *ValidationError {
	t.Helper()
	_, err := Load(fixture(name))
	if err == nil {
		t.Fatalf("%s: invalid config was accepted", name)
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("%s: error is not a ValidationError: %v", name, err)
	}
	if got := qferr.CodeOf(err); got != qferr.CodeConfigError {
		t.Errorf("%s: code = %s, want %s", name, got, qferr.CodeConfigError)
	}
	return ve
}

// TestGoldenFixtures is the golden config test the brief asks for: every
// fixture, and the EXACT set of rules it must break.
//
// Asserting the exact set rather than "contains" is the point. A validator that
// grows a spurious extra violation is as broken as one that misses a real one,
// and only an exact comparison catches the first case.
func TestGoldenFixtures(t *testing.T) {
	cases := []struct {
		file  string
		rules []Rule
	}{
		{"invalid-no-entities.json", []Rule{RuleNoEntities}},
		{"invalid-unnamed-entity.json", []Rule{RuleMissingEntityName}},
		{"invalid-duplicate-entity.json", []Rule{RuleDuplicateEntity}},
		{"invalid-unknown-entity.json", []Rule{RuleUnknownEntity}},
		{"invalid-duplicate-field.json", []Rule{RuleDuplicateField}},
		{"invalid-field-type.json", []Rule{RuleUnsupportedFieldType}},
		{"invalid-operator-type.json", []Rule{RuleOperatorTypeMismatch}},
		{"invalid-aggregation-type.json", []Rule{RuleAggregationMismatch}},
		{"invalid-unknown-operator.json", []Rule{RuleUnknownOperator}},
		{"invalid-identifier.json", []Rule{RuleInvalidIdentifier}},
		{"invalid-value-alias.json", []Rule{RuleMalformedValueAlias}},
		{"invalid-enum.json", []Rule{RuleEnumValues}},
		{"invalid-missing-phrases.json", []Rule{RuleMissingPhrases}},
		{"invalid-relationship.json", []Rule{RuleInvalidCardinality, RuleUnknownRelationTarget}},
		{"invalid-relationship-types.json", []Rule{RuleRelationshipTypes}},
		{"invalid-query-pattern.json", []Rule{RuleDuplicatePattern, RuleUnknownEntity, RuleUnknownPatternIntent}},
		{"invalid-security-limits.json", []Rule{RuleInvalidLimit}},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			ve := loadInvalid(t, tc.file)
			got := ve.Rules()
			if len(got) != len(tc.rules) {
				t.Fatalf("rules = %v, want %v", got, tc.rules)
			}
			for i := range got {
				if got[i] != tc.rules[i] {
					t.Fatalf("rules = %v, want %v", got, tc.rules)
				}
			}
		})
	}
}

func TestValidFixturesAreAccepted(t *testing.T) {
	for _, name := range []string{"customers.json", "commerce.json"} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fixture(name)); err != nil {
				t.Fatalf("valid config rejected:\n%v", err)
			}
		})
	}
}

// TestEveryRuleHasAFixture keeps the fixture set honest. Without it, a rule
// added later could sit untested indefinitely while the suite stayed green.
func TestEveryRuleHasAFixture(t *testing.T) {
	// Rules reachable only from a config that cannot be written to a file, or
	// that another rule always masks first, are listed here with the test that
	// does cover them.
	coveredElsewhere := map[Rule]string{
		RuleMissingFieldName:      "TestFieldWithoutNameOrEntity",
		RuleMissingFieldEntity:    "TestFieldWithoutNameOrEntity",
		RuleUnknownAggregation:    "TestUnknownAggregationKey",
		RuleDuplicateRelationship: "TestDuplicateRelationshipName",
	}

	all := []Rule{
		RuleNoEntities, RuleMissingEntityName, RuleDuplicateEntity,
		RuleMissingFieldName, RuleMissingFieldEntity, RuleUnknownEntity,
		RuleDuplicateField, RuleUnsupportedFieldType, RuleOperatorTypeMismatch,
		RuleUnknownOperator, RuleAggregationMismatch, RuleUnknownAggregation,
		RuleInvalidIdentifier, RuleMalformedValueAlias, RuleEnumValues,
		RuleMissingPhrases, RuleDuplicateRelationship, RuleUnknownRelationTarget,
		RuleInvalidCardinality, RuleRelationshipTypes, RuleDuplicatePattern,
		RuleUnknownPatternIntent, RuleInvalidLimit,
	}

	seen := make(map[Rule]bool)
	for _, f := range []string{
		"invalid-no-entities.json", "invalid-unnamed-entity.json",
		"invalid-duplicate-entity.json", "invalid-unknown-entity.json",
		"invalid-duplicate-field.json", "invalid-field-type.json",
		"invalid-operator-type.json", "invalid-aggregation-type.json",
		"invalid-unknown-operator.json", "invalid-identifier.json",
		"invalid-value-alias.json", "invalid-enum.json",
		"invalid-missing-phrases.json", "invalid-relationship.json",
		"invalid-relationship-types.json", "invalid-query-pattern.json",
		"invalid-security-limits.json",
	} {
		for _, r := range loadInvalid(t, f).Rules() {
			seen[r] = true
		}
	}

	for _, r := range all {
		if seen[r] {
			continue
		}
		if where, ok := coveredElsewhere[r]; ok {
			t.Logf("rule %s is covered by %s rather than a fixture", r, where)
			continue
		}
		t.Errorf("rule %s has no fixture and no recorded alternative coverage", r)
	}
}

func TestFieldWithoutNameOrEntity(t *testing.T) {
	cfg := &Config{
		Entities: []Entity{{Name: "customer", Table: "customers"}},
		Fields: []Field{
			{Entity: "customer", Type: TypeString},
			{Name: "orphan", Type: TypeString},
		},
	}
	cfg.Normalize()

	var ve *ValidationError
	if !errors.As(Validate(cfg), &ve) {
		t.Fatal("config was accepted")
	}
	if !ve.HasRule(RuleMissingFieldName) {
		t.Errorf("rules = %v, want %s", ve.Rules(), RuleMissingFieldName)
	}
	if !ve.HasRule(RuleMissingFieldEntity) {
		t.Errorf("rules = %v, want %s", ve.Rules(), RuleMissingFieldEntity)
	}
}

func TestUnknownAggregationKey(t *testing.T) {
	cfg := &Config{
		Entities:           []Entity{{Name: "customer", Table: "customers"}},
		Fields:             []Field{{Entity: "customer", Name: "name", Type: TypeString}},
		AggregationPhrases: map[Aggregation][]string{"median": {"median"}},
	}
	cfg.Normalize()

	var ve *ValidationError
	if !errors.As(Validate(cfg), &ve) {
		t.Fatal("config was accepted")
	}
	if !ve.HasRule(RuleUnknownAggregation) {
		t.Errorf("rules = %v, want %s", ve.Rules(), RuleUnknownAggregation)
	}
}

func TestDuplicateRelationshipName(t *testing.T) {
	rel := Relationship{
		Name:        "customer_orders",
		From:        Endpoint{Entity: "customer", Field: "id"},
		To:          Endpoint{Entity: "order", Field: "customer_id"},
		Cardinality: OneToMany,
	}
	cfg := &Config{
		Entities: []Entity{{Name: "customer", Table: "customers"}, {Name: "order", Table: "orders"}},
		Fields: []Field{
			{Entity: "customer", Name: "id", Type: TypeNumber},
			{Entity: "order", Name: "customer_id", Type: TypeNumber},
		},
		Relationships: []Relationship{rel, rel},
	}
	cfg.Normalize()

	var ve *ValidationError
	if !errors.As(Validate(cfg), &ve) {
		t.Fatal("config was accepted")
	}
	if !ve.HasRule(RuleDuplicateRelationship) {
		t.Errorf("rules = %v, want %s", ve.Rules(), RuleDuplicateRelationship)
	}
}

// TestRejectionIsDeterministic is the Phase 1 exit criterion in test form:
// config behaviour must be deterministic. Several rules walk Go maps, whose
// iteration order is randomised per run, so this would fail without the
// explicit sorting in Validate.
func TestRejectionIsDeterministic(t *testing.T) {
	// Chosen because it breaks four rules across two map-walking checks
	// (value aliases and field iteration), which is where ordering would drift.
	const file = "invalid-value-alias.json"

	_, first := Load(fixture(file))
	if first == nil {
		t.Fatal("invalid config was accepted")
	}
	for i := 1; i < 25; i++ {
		_, err := Load(fixture(file))
		if err == nil {
			t.Fatal("invalid config was accepted")
		}
		if err.Error() != first.Error() {
			t.Fatalf("message varies between runs:\n  run 0:\n%s\n  run %d:\n%s", first.Error(), i, err.Error())
		}
	}
}

// TestAllViolationsReportedAtOnce guards the choice to collect rather than
// fail fast. A validator that stopped at the first problem would turn a config
// with four mistakes into four edit-run cycles.
func TestAllViolationsReportedAtOnce(t *testing.T) {
	ve := loadInvalid(t, "invalid-value-alias.json")
	if len(ve.Violations) < 4 {
		t.Errorf("got %d violations, want at least 4: %v", len(ve.Violations), ve.Violations)
	}
	msg := ve.Error()
	if strings.Count(msg, "\n  - ") != len(ve.Violations) {
		t.Errorf("rendered message lists %d violations but the error holds %d:\n%s",
			strings.Count(msg, "\n  - "), len(ve.Violations), msg)
	}
}

// TestIdentifierLengthLimit covers the truncation hazard: PostgreSQL silently
// cuts identifiers at 63 characters, so two distinct long columns can collide
// into one and return wrong rows rather than an error.
func TestIdentifierLengthLimit(t *testing.T) {
	long := strings.Repeat("a", maxIdentifierLength+1)
	cfg := &Config{
		Entities: []Entity{{Name: "customer", Table: long}},
		Fields:   []Field{{Entity: "customer", Name: "name", Type: TypeString}},
	}
	cfg.Normalize()

	var ve *ValidationError
	if !errors.As(Validate(cfg), &ve) {
		t.Fatal("over-long identifier was accepted")
	}
	if !ve.HasRule(RuleInvalidIdentifier) {
		t.Errorf("rules = %v, want %s", ve.Rules(), RuleInvalidIdentifier)
	}

	// The boundary itself must be accepted, or the rule is off by one.
	cfg.Entities[0].Table = strings.Repeat("a", maxIdentifierLength)
	if err := Validate(cfg); err != nil {
		t.Errorf("a %d-character identifier should be accepted: %v", maxIdentifierLength, err)
	}
}

// TestInjectionShapedIdentifierIsRejected is the config-level half of section
// 16.2. User text can never become an identifier, but a config author could
// still write one, and the compiler trusts config identifiers by construction.
func TestInjectionShapedIdentifierIsRejected(t *testing.T) {
	for _, table := range []string{
		"customers; DROP TABLE users --",
		`customers" ; --`,
		"customers users",
		"1customers",
		"customers-archive",
		"",
	} {
		cfg := &Config{
			Entities: []Entity{{Name: "customer", Table: table}},
			Fields:   []Field{{Entity: "customer", Name: "name", Type: TypeString}},
		}
		// Normalize deliberately NOT called for the empty case, so that the
		// blank table is not defaulted away before it can be checked.
		if table != "" {
			cfg.Normalize()
		}
		if err := Validate(cfg); err == nil {
			t.Errorf("table %q was accepted as an identifier", table)
		}
	}
}

// Phase 6 (decision 015): a top_n pattern's direction is config, validated
// when present. Absent is legal at load; the planner refuses to guess it.
func TestPatternOrder(t *testing.T) {
	base := func(intent PatternIntent, order SortOrder) *Config {
		cfg := &Config{
			Entities:        []Entity{{Name: "customer"}},
			Fields:          []Field{{Entity: "customer", Name: "spent", Type: TypeNumber}},
			OperatorPhrases: map[Operator][]string{OpGt: {"over"}},
			QueryPatterns:   []QueryPattern{{Name: "p", Intent: intent, Phrases: []string{"top N"}, Order: order}},
		}
		cfg.Normalize()
		return cfg
	}
	for _, c := range []struct {
		intent PatternIntent
		order  SortOrder
		ok     bool
	}{
		{IntentTopN, OrderDesc, true},
		{IntentTopN, OrderAsc, true},
		{IntentTopN, "", true},
		{IntentTopN, "descending", false},
		{IntentCount, OrderDesc, false},
	} {
		err := Validate(base(c.intent, c.order))
		var ve *ValidationError
		bad := errors.As(err, &ve) && containsRule(ve.Rules(), RuleInvalidPatternOrder)
		if bad == c.ok {
			t.Errorf("intent %s order %q: err = %v, want ok=%v", c.intent, c.order, err, c.ok)
		}
	}
}

func containsRule(rules []Rule, r Rule) bool {
	for _, x := range rules {
		if x == r {
			return true
		}
	}
	return false
}

// Phase 7: optional per-schema ranking thresholds, bounded like ranking's own
// policy validation.
func TestRankingThresholds(t *testing.T) {
	f := func(x float64) *float64 { return &x }
	for _, c := range []struct {
		score, margin *float64
		ok            bool
	}{
		{nil, nil, true},
		{f(0.8), f(0.15), true},
		{f(2), f(0), true},
		{f(0), nil, false},
		{f(2.5), nil, false},
		{nil, f(-0.1), false},
		{nil, f(1.5), false},
	} {
		cfg := &Config{
			Entities: []Entity{{Name: "customer"}},
			Fields:   []Field{{Entity: "customer", Name: "spent", Type: TypeNumber}},
			Ranking:  RankingThresholds{MinScore: c.score, MinMargin: c.margin},
		}
		cfg.Normalize()
		err := Validate(cfg)
		var ve *ValidationError
		bad := errors.As(err, &ve) && containsRule(ve.Rules(), RuleInvalidRanking)
		if bad == c.ok {
			t.Errorf("score=%v margin=%v: err = %v, want ok=%v", c.score, c.margin, err, c.ok)
		}
	}
}
