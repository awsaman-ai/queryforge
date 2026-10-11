package config

import "testing"

// TestVocabularyListsMatchPredicates guards a drift hazard: ValidOperator and
// Operators() are written separately, so an operator added to one and forgotten
// in the other would silently make it unlistable in error messages — or worse,
// listable but rejected.
func TestVocabularyListsMatchPredicates(t *testing.T) {
	for _, op := range Operators() {
		if !ValidOperator(op) {
			t.Errorf("Operators() lists %q but ValidOperator rejects it", op)
		}
		if _, ok := anyTypeAllows(op); !ok {
			t.Errorf("operator %q is valid but no field type permits it, so it can never be emitted", op)
		}
	}
	for _, agg := range Aggregations() {
		if !ValidAggregation(agg) {
			t.Errorf("Aggregations() lists %q but ValidAggregation rejects it", agg)
		}
	}
	for _, c := range Cardinalities() {
		if !ValidCardinality(c) {
			t.Errorf("Cardinalities() lists %q but ValidCardinality rejects it", c)
		}
	}
	for _, i := range PatternIntents() {
		if !ValidPatternIntent(i) {
			t.Errorf("PatternIntents() lists %q but ValidPatternIntent rejects it", i)
		}
	}
	for _, ft := range FieldTypes() {
		if !ValidFieldType(ft) {
			t.Errorf("FieldTypes() lists %q but ValidFieldType rejects it", ft)
		}
		// Every type needs an aggregation matrix entry too, or COUNT silently
		// becomes unavailable on it.
		if len(AggregationsFor(ft)) == 0 {
			t.Errorf("field type %q has no aggregations; at least count should apply", ft)
		}
	}
}

func anyTypeAllows(op Operator) (FieldType, bool) {
	for _, ft := range FieldTypes() {
		if OperatorAllowed(ft, op) {
			return ft, true
		}
	}
	return "", false
}

// TestCompatibilityMatrix pins the deliberate omissions. These are judgment
// calls documented in vocabulary.go, and a future edit that quietly re-allows
// one should have to change this test and read the reasoning.
func TestCompatibilityMatrix(t *testing.T) {
	allowed := []struct {
		t  FieldType
		op Operator
	}{
		{TypeString, OpEq}, {TypeString, OpLike}, {TypeString, OpIn},
		{TypeNumber, OpGt}, {TypeNumber, OpLte}, {TypeNumber, OpIn},
		{TypeDate, OpGte}, {TypeDate, OpLt},
		{TypeBoolean, OpEq}, {TypeBoolean, OpNeq},
		{TypeEnum, OpEq}, {TypeEnum, OpIn},
	}
	for _, c := range allowed {
		if !OperatorAllowed(c.t, c.op) {
			t.Errorf("operator %q should be allowed on %q", c.op, c.t)
		}
	}

	refused := []struct {
		t   FieldType
		op  Operator
		why string
	}{
		{TypeString, OpGt, "lexicographic ordering on a name is not a question users mean to ask"},
		{TypeString, OpLte, "same"},
		{TypeNumber, OpLike, "substring matching a number is meaningless"},
		{TypeBoolean, OpGt, "booleans do not order"},
		{TypeBoolean, OpLike, "booleans do not have substrings"},
		{TypeEnum, OpLike, "an enum is a closed set; substring search defeats the point"},
		{TypeDate, OpLike, "dates are compared, not matched as text"},
	}
	for _, c := range refused {
		if OperatorAllowed(c.t, c.op) {
			t.Errorf("operator %q should be refused on %q: %s", c.op, c.t, c.why)
		}
	}

	aggRefused := []struct {
		t   FieldType
		agg Aggregation
	}{
		{TypeString, AggSum}, {TypeString, AggAvg}, {TypeString, AggMin},
		{TypeDate, AggSum}, {TypeDate, AggAvg},
		{TypeBoolean, AggSum}, {TypeEnum, AggMax},
	}
	for _, c := range aggRefused {
		if AggregationAllowed(c.t, c.agg) {
			t.Errorf("aggregation %q should be refused on %q", c.agg, c.t)
		}
	}

	// COUNT must work everywhere: it counts rows, not magnitudes.
	for _, ft := range FieldTypes() {
		if !AggregationAllowed(ft, AggCount) {
			t.Errorf("count should be allowed on %q", ft)
		}
	}
}

// TestManyToManyIsRefused pins the scope decision rather than leaving it as a
// gap someone fills in by reflex. Treating it as one-to-many would duplicate
// parent rows silently.
func TestManyToManyIsRefused(t *testing.T) {
	if ValidCardinality("many-to-many") {
		t.Error("many-to-many is out of scope for the first experiment")
	}
}

// TestMatrixAccessorsReturnCopies guards concurrency safety (section 22, rule 9).
// The matrices are package state shared by every request once a registry is
// built; handing out the backing array would let one caller corrupt it for all.
func TestMatrixAccessorsReturnCopies(t *testing.T) {
	ops := OperatorsFor(TypeNumber)
	if len(ops) == 0 {
		t.Fatal("number should allow some operators")
	}
	ops[0] = "clobbered"

	if got := OperatorsFor(TypeNumber); got[0] == "clobbered" {
		t.Error("OperatorsFor handed out the shared backing array")
	}

	aggs := AggregationsFor(TypeNumber)
	aggs[0] = "clobbered"
	if got := AggregationsFor(TypeNumber); got[0] == "clobbered" {
		t.Error("AggregationsFor handed out the shared backing array")
	}
}

// TestFieldNarrowingCannotWiden covers the rule that a field's declared
// operator list is a subset filter, never an escape hatch past the type matrix.
func TestFieldNarrowingCannotWiden(t *testing.T) {
	narrowed := &Field{Type: TypeNumber, Operators: []Operator{OpGt, OpLt}}
	if got := len(narrowed.AllowedOperators()); got != 2 {
		t.Errorf("declared subset should win, got %d operators", got)
	}

	open := &Field{Type: TypeBoolean}
	if got, want := len(open.AllowedOperators()), len(OperatorsFor(TypeBoolean)); got != want {
		t.Errorf("an empty list means everything the type allows: got %d, want %d", got, want)
	}

	// Widening past the matrix is rejected at config load, not silently honoured
	// here; this is the validator's job, and the check is worth stating twice.
	cfg := &Config{
		Entities: []Entity{{Name: "customer", Table: "customers"}},
		Fields:   []Field{{Entity: "customer", Name: "name", Type: TypeString, Operators: []Operator{OpGt}}},
	}
	cfg.Normalize()
	if err := Validate(cfg); err == nil {
		t.Error("a field declaring an operator outside its type matrix should be rejected")
	}
}
