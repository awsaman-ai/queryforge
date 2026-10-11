package binding

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
)

// ===========================================================================
// The brief's manual-gate sentences (section 13)
// ===========================================================================

func TestGateCustomersFromIndia(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers from India", map[string][]hit{
		"customers": hCustomers, "india": hIndia,
	})
	if b.Root != "customer" || len(b.Conditions) != 1 || len(b.Joins) != 0 {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}
	c := b.Conditions[0]
	if c.Kind != CondCompare || c.Field.String() != "customer.country" || c.Op != config.OpEq ||
		c.Value.Kind != ValueAlias || c.Value.Stored.Reveal() != "IN" {
		t.Errorf("condition = %s", c.describe(true))
	}
}

func TestGateCustomersWhoBoughtLaptops(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers who bought laptops", map[string][]hit{
		"customers": hCustomers, "bought": hBought, "laptops": hLaptops,
	})
	if len(b.Conditions) != 1 {
		t.Fatalf("want exactly the category condition (no redundant exists):\n%s", b.Explain(true))
	}
	c := b.Conditions[0]
	if c.Field.String() != "product.category" || c.Value.Stored.Reveal() != "laptop" {
		t.Errorf("condition = %s", c.describe(true))
	}
	if got := c.Path.String(); got != "customer -[customer_orders]-> order -[order_product]-> product" {
		t.Errorf("path = %s", got)
	}
	if len(b.Joins) != 1 {
		t.Errorf("joins = %v", b.Joins)
	}
}

func TestGateCustomersWhoOrderedMoreThan3Times(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers who ordered more than 3 times", map[string][]hit{
		"customers": hCustomers, "ordered": hOrdered, "more than": hMoreThan,
	})
	if len(b.Conditions) != 1 {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}
	c := b.Conditions[0]
	if c.Kind != CondCount || c.Relationship != "customer_orders" || c.Op != config.OpGt || c.Value.Number.String() != "3" {
		t.Errorf("condition = %s", c.describe(true))
	}
	if !c.Path.ToMany() {
		t.Error("the counted path must fan out")
	}
	if c.Value.Unit.Reveal() != "times" {
		t.Errorf("unit = %q; \"times\" is kept as the count's unit, not left unexplained", c.Value.Unit.Reveal())
	}
}

func TestGateCustomersWhoseOrderAmountIsAbove10000(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers whose order amount is above 10000", map[string][]hit{
		"customers": hCustomers, "order amount": hOrderAmt, "above": hAbove,
	})
	if b.Root != "customer" || len(b.Conditions) != 1 {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}
	c := b.Conditions[0]
	if c.Field.String() != "order.amount" || c.Op != config.OpGt || c.Value.Number.String() != "10000" {
		t.Errorf("condition = %s", c.describe(true))
	}
	if c.Path.String() != "customer -[customer_orders]-> order" {
		t.Errorf("path = %s", c.Path)
	}
}

// The brief's own worked example (section 13).
func TestBriefExampleSpentMoreThan50000(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers who spent more than 50000", map[string][]hit{
		"customers": hCustomers, "spent": hSpent, "more than": hMoreThan,
	})
	c := b.Conditions[0]
	if c.Field.String() != "customer.total_spent" || c.Op != config.OpGt || c.Value.Number.String() != "50000" {
		t.Errorf("condition = %s", c.describe(true))
	}
}

// ===========================================================================
// Required rejection cases (section 13)
// ===========================================================================

func TestRejectCandidatesThatCannotBeConnected(t *testing.T) {
	_, err := bind(t, "multipath.json", "customers with warehouse capacity over 100", map[string][]hit{
		"customers":          {{"entity:customer", 0.9, ""}},
		"warehouse capacity": {{"field:warehouse.capacity", 1.0, "warehouse capacity"}},
		"over":               {{"operator:gt", 1.0, "over"}, {"operator:lt", 0.5, "under"}},
	})
	if qferr.CodeOf(err) != qferr.CodeRelationshipNotFound {
		t.Fatalf("got %v, want RELATIONSHIP_NOT_FOUND", err)
	}
}

func TestRejectEquallyPlausibleJoinPaths(t *testing.T) {
	hits := map[string][]hit{
		"orders":    {{"entity:order", 0.9, ""}},
		"customers": {{"entity:customer", 0.9, ""}},
		"ruritania": {{"value:customer.country:alias:Ruritania", 1.0, "Ruritania"}},
	}
	_, err := bind(t, "multipath.json", "orders from customers in Ruritania", hits)
	if qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Fatalf("got %v, want AMBIGUOUS_MATCH", err)
	}

	// ...and the verb that names one of them decides it.
	for rel, verb := range map[string]string{
		"order_shipping_customer": "shipped",
		"order_billing_customer":  "billed",
	} {
		h := map[string][]hit{}
		for k, v := range hits {
			h[k] = v
		}
		h[verb] = []hit{{"relationship:" + rel, 1.0, verb}}
		b, err := bind(t, "multipath.json", "orders "+verb+" to customers in Ruritania", h)
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if !b.Conditions[0].Path.Uses(rel) {
			t.Errorf("%s: path %s", verb, b.Conditions[0].Path)
		}
	}
}

func TestRejectFieldIncompatibleWithOperator(t *testing.T) {
	cases := []struct {
		name, sentence string
		hits           map[string][]hit
	}{
		{"number against a string field", "customer name over 5", map[string][]hit{
			"customer name": {{"field:customer.name", 1.0, "customer name"}},
			"over":          hOver,
		}},
		// total_spent declares eq/gt/gte/lt/lte; like is not among them. (The
		// first version used "one of" → in, and passed for the wrong reason:
		// Phase 3 reads the "one" as the number 1, so the operator never
		// formed — see TestOneOfCollidesWithTheNumberOne.)
		{"operator outside the field's allowlist", "customer spending contains 5", map[string][]hit{
			"customer spending": {{"field:customer.total_spent", 1.0, "customer spend"}},
			"contains":          {{"operator:like", 1.0, "contains"}},
		}},
		// "equals" binds to name, and the only value after it belongs to
		// country: name is compared with nothing.
		{"an operator whose only value belongs to another field", "customer name equals India", map[string][]hit{
			"customer name": {{"field:customer.name", 1.0, "customer name"}},
			"equals":        {{"operator:eq", 1.0, "equals"}},
			"india":         hIndia,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := bind(t, "commerce.json", c.sentence, c.hits)
			if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
				t.Errorf("got %v, want UNSUPPORTED_QUERY", err)
			}
		})
	}
}

// Without an operator, a value of another field is simply its own condition:
// "customer name India" shows names of customers in India. Recorded because
// the first draft of the test above expected a refusal here, and the binding
// is the better reading.
func TestValueOfAnotherFieldIsItsOwnCondition(t *testing.T) {
	b := mustBind(t, "commerce.json", "customer name India", map[string][]hit{
		"customer name": {{"field:customer.name", 1.0, "customer name"}},
		"india":         hIndia,
	})
	if len(b.Selections) != 1 || b.Selections[0].Field.String() != "customer.name" ||
		len(b.Conditions) != 1 || b.Conditions[0].Field.String() != "customer.country" {
		t.Errorf("binding:\n%s", b.Explain(true))
	}
}

func TestRejectAggregationInvalidForFieldType(t *testing.T) {
	_, err := bind(t, "commerce.json", "average customer name", map[string][]hit{
		"average":       {{"aggregation:avg", 1.0, "average"}},
		"customer name": {{"field:customer.name", 1.0, "customer name"}},
	})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery || !strings.Contains(err.Error(), "avg") {
		t.Errorf("got %v, want UNSUPPORTED_QUERY naming avg", err)
	}
}

func TestRejectValueThatCannotBeResolved(t *testing.T) {
	cases := []struct{ name, sentence, field string }{
		// "cancelled" is not a declared value or alias of the enum: guessing
		// the nearest (suspended? inactive?) is the silent substitution the
		// experiment exists to avoid.
		{"undeclared enum value", "customer status cancelled", "field:customer.status"},
		// Free text cannot become a number.
		{"text for a number field", "customer spending lots", "field:customer.total_spent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			words := strings.Fields(c.sentence)
			fieldPhrase := strings.Join(words[:2], " ")
			_, err := bind(t, "commerce.json", c.sentence, map[string][]hit{
				fieldPhrase: {{c.field, 1.0, ""}},
			})
			if qferr.CodeOf(err) != qferr.CodeValueResolutionFailed {
				t.Errorf("got %v, want VALUE_RESOLUTION_FAILED", err)
			}
		})
	}
}

// ===========================================================================
// Values
// ===========================================================================

// A typed enum value or alias resolves case-insensitively; a string field
// takes free text as a literal.
func TestLiteralResolution(t *testing.T) {
	cases := []struct {
		sentence, fieldPhrase, field string
		kind                         ValueKind
		stored                       string
	}{
		{"customer status ACTIVE", "customer status", "field:customer.status", ValueEnum, "active"},
		{"customer status blocked", "customer status", "field:customer.status", ValueAlias, "suspended"},
		{"customer name Smith", "customer name", "field:customer.name", ValueLiteral, "Smith"},
		{"customer name Mary Jane Smith", "customer name", "field:customer.name", ValueLiteral, "Mary Jane Smith"},
	}
	for _, c := range cases {
		t.Run(c.sentence, func(t *testing.T) {
			b := mustBind(t, "commerce.json", c.sentence, map[string][]hit{
				c.fieldPhrase: {{c.field, 1.0, ""}},
			})
			v := b.Conditions[0].Value
			if v.Kind != c.kind || v.Stored.Reveal() != c.stored {
				t.Errorf("value = %s %q, want %s %q", v.Kind, v.Stored.Reveal(), c.kind, c.stored)
			}
		})
	}
}

// A date with no field attaches to the root's only date field, marked
// inferred; a named date field is used as named.
func TestDateWindows(t *testing.T) {
	b := mustBind(t, "commerce.json", "orders last month", map[string][]hit{"orders": hOrders})
	c := b.Conditions[0]
	if c.Kind != CondWindow || c.Field.String() != "order.placed_at" || !c.Inferred {
		t.Errorf("inferred window = %s", c.describe(true))
	}

	b = mustBind(t, "commerce.json", "customers created last year", map[string][]hit{
		"customers": hCustomers,
		"created":   {{"field:customer.created_at", 0.683, "created at"}, {"field:customer.name", 0.3, "name"}},
	})
	c = b.Conditions[0]
	if c.Kind != CondWindow || c.Field.String() != "customer.created_at" || c.Inferred {
		t.Errorf("named window = %s", c.describe(true))
	}
}

// A comparison with no field: inferred when the root has exactly one field it
// can mean, refused as ambiguous when it has several.
func TestOrphanComparison(t *testing.T) {
	_, err := bind(t, "commerce.json", "orders over 100", map[string][]hit{"orders": hOrders, "over": hOver})
	if qferr.CodeOf(err) != qferr.CodeAmbiguousMatch || !strings.Contains(err.Error(), "order.amount") ||
		!strings.Contains(err.Error(), "order.id") {
		t.Errorf("got %v; order has two filterable number fields, so the field must not be guessed", err)
	}

	// Remove the choice: with only one number field filterable, it is inferred.
	cfg := loadConfig(t, "commerce.json")
	for i := range cfg.Fields {
		if cfg.Fields[i].Entity == "order" && cfg.Fields[i].Name == "id" {
			no := false
			cfg.Fields[i].Filterable = &no
		}
	}
	b, err := bindReg(t, config.NewSchemaRegistry(cfg), "orders over 100", map[string][]hit{"orders": hOrders, "over": hOver})
	if err != nil {
		t.Fatal(err)
	}
	if c := b.Conditions[0]; c.Field.String() != "order.amount" || !c.Inferred {
		t.Errorf("condition = %s", c.describe(true))
	}
}

// ===========================================================================
// Multi-condition binding
// ===========================================================================

func TestConditionsJoinWithAnd(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers from India who spent more than 50000", map[string][]hit{
		"customers": hCustomers, "india": hIndia, "spent": hSpent, "more than": hMoreThan,
	})
	if len(b.Conditions) != 2 || b.Conditions[0].Join != "" || b.Conditions[1].Join != "and" {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}
	if b.Conditions[0].Field.Field != "country" || b.Conditions[1].Field.Field != "total_spent" {
		t.Errorf("order = %s, %s", b.Conditions[0].Field, b.Conditions[1].Field)
	}
}

func TestConditionsJoinWithOr(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers from India or United Kingdom", map[string][]hit{
		"customers":      hCustomers,
		"india":          hIndia,
		"united kingdom": {{"value:customer.country:alias:United Kingdom", 1.0, "United Kingdom"}},
	})
	if len(b.Conditions) != 2 || b.Conditions[1].Join != "or" {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}
}

func TestMixedAndOrIsRefused(t *testing.T) {
	_, err := bind(t, "commerce.json", "customers from India or United Kingdom and spent over 5", map[string][]hit{
		"customers":      hCustomers,
		"india":          hIndia,
		"united kingdom": {{"value:customer.country:alias:United Kingdom", 1.0, "United Kingdom"}},
		"spent":          hSpent,
		"over":           hOver,
	})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery || !strings.Contains(err.Error(), "precedence") {
		t.Errorf("got %v", err)
	}
}

// Conditions on three entities at once, each on its own path.
func TestMultiEntityConditions(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers from India who bought laptops with order amount over 500", map[string][]hit{
		"customers": hCustomers, "india": hIndia, "bought": hBought, "laptops": hLaptops,
		"order amount": hOrderAmt, "over": hOver,
	})
	got := map[string]string{}
	for _, c := range b.Conditions {
		got[c.Field.String()] = c.Path.String()
	}
	want := map[string]string{
		"customer.country": "customer",
		"product.category": "customer -[customer_orders]-> order -[order_product]-> product",
		"order.amount":     "customer -[customer_orders]-> order",
	}
	for f, p := range want {
		if got[f] != p {
			t.Errorf("%s via %q, want %q\n%s", f, got[f], p, b.Explain(true))
		}
	}
	if len(b.Joins) != 2 {
		t.Errorf("joins = %d, want 2 distinct paths", len(b.Joins))
	}
}

func TestMaxFiltersIsEnforced(t *testing.T) {
	cfg := loadConfig(t, "commerce.json")
	cfg.Security.MaxFilters = 1
	_, err := bindReg(t, config.NewSchemaRegistry(cfg), "customers from India who spent more than 5", map[string][]hit{
		"customers": hCustomers, "india": hIndia, "spent": hSpent, "more than": hMoreThan,
	})
	if qferr.CodeOf(err) != qferr.CodeSecurityRejected {
		t.Errorf("got %v, want SECURITY_REJECTED", err)
	}
}

// ===========================================================================
// Negation
// ===========================================================================

func TestNegationComplementsTheOperator(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers not from India", map[string][]hit{
		"customers": hCustomers, "india": hIndia,
	})
	if c := b.Conditions[0]; c.Op != config.OpNeq || !c.Negated {
		t.Errorf("condition = %s", c.describe(true))
	}

	b = mustBind(t, "commerce.json", "customers who spent not more than 5", map[string][]hit{
		"customers": hCustomers, "spent": hSpent, "more than": hMoreThan,
	})
	if c := b.Conditions[0]; c.Op != config.OpLte || !c.Negated {
		t.Errorf("not more than: %s", c.describe(true))
	}
}

// "like" has no configured opposite; negating it is refused, not dropped.
func TestNegationWithoutAnOppositeIsRefused(t *testing.T) {
	_, err := bind(t, "commerce.json", "customer name not contains Smith", map[string][]hit{
		"customer name": {{"field:customer.name", 1.0, "customer name"}},
		"contains":      {{"operator:like", 1.0, "contains"}},
	})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("got %v", err)
	}
}

// ===========================================================================
// Relationships: count and exists
// ===========================================================================

// "customers who ordered" restricts (a customer may have none): exists.
// "orders placed by customers" does not (every order has one): a note.
func TestExistsFollowsCardinality(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers who ordered", map[string][]hit{
		"customers": hCustomers, "ordered": hOrdered,
	})
	if len(b.Conditions) != 1 || b.Conditions[0].Kind != CondExists || b.Conditions[0].Relationship != "customer_orders" {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}

	b = mustBind(t, "commerce.json", "orders placed by customers", map[string][]hit{
		"orders":    {{"entity:order", 0.9, ""}},
		"placed":    {{"relationship:customer_orders", 1.0, "placed"}},
		"customers": hCustomers,
	})
	if len(b.Conditions) != 0 || len(b.Notes) == 0 {
		t.Errorf("a to-one relationship adds no condition:\n%s", b.Explain(true))
	}
}

// Counting a to-one relationship is always 0 or 1: refused, not given a
// meaning.
func TestCountThroughToOneIsRefused(t *testing.T) {
	_, err := bind(t, "commerce.json", "orders ordered more than 3 times", map[string][]hit{
		"orders": {{"entity:order", 0.9, ""}}, "ordered": hOrdered, "more than": hMoreThan,
	})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("got %v", err)
	}
}

func TestCountNeedsAWholeNumber(t *testing.T) {
	_, err := bind(t, "commerce.json", "customers who ordered more than 2.5 times", map[string][]hit{
		"customers": hCustomers, "ordered": hOrdered, "more than": hMoreThan,
	})
	if qferr.CodeOf(err) != qferr.CodeValueResolutionFailed {
		t.Errorf("got %v", err)
	}
}

// A relationship mentioned but never traversed would be a dropped meaning.
func TestUntraversedRelationshipIsRefused(t *testing.T) {
	_, err := bind(t, "commerce.json", "customers zqx", map[string][]hit{
		"customers": hCustomers,
		"zqx":       {{"relationship:order_product", 1.0, "product"}},
	})
	if qferr.CodeOf(err) != qferr.CodeRelationshipNotFound {
		t.Errorf("got %v", err)
	}
}

// ===========================================================================
// Aggregates, patterns, selections
// ===========================================================================

func TestAggregates(t *testing.T) {
	b := mustBind(t, "commerce.json", "average order amount", map[string][]hit{
		"average": {{"aggregation:avg", 1.0, "average"}}, "order amount": hOrderAmt,
	})
	if len(b.Aggregates) != 1 || b.Aggregates[0].Func != config.AggAvg || b.Aggregates[0].Field.String() != "order.amount" {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}
	if len(b.Selections) != 0 {
		t.Error("the aggregated field is not also a selection")
	}

	b = mustBind(t, "commerce.json", "how many customers from India", map[string][]hit{
		"how many":  {{"aggregation:count", 1.0, "how many"}, {"pattern:how_many", 1.0, "how many"}},
		"customers": hCustomers, "india": hIndia,
	})
	if len(b.Aggregates) != 1 || b.Aggregates[0].Field != nil || b.Aggregates[0].Entity != "customer" {
		t.Errorf("count(*): %s", b.Explain(true))
	}
	if len(b.Conditions) != 1 {
		t.Errorf("the filter survives alongside the count: %s", b.Explain(true))
	}
}

func TestTopNPattern(t *testing.T) {
	b := mustBind(t, "commerce.json", "top 5 customers by spending", map[string][]hit{
		"top":       {{"pattern:top_n", 0.825, "top N"}, {"pattern:bottom_n", 0.4, "bottom N"}},
		"customers": hCustomers,
		"spending":  {{"field:customer.total_spent", 1.0, "spending"}},
	})
	p := b.Pattern
	if p == nil || p.N != 5 || p.Metric == nil || p.Metric.String() != "customer.total_spent" {
		t.Fatalf("binding:\n%s", b.Explain(true))
	}
	if len(b.Conditions) != 0 {
		t.Error("the 5 is the pattern's count, not a comparison value")
	}
}

func TestTopNNeedsAMetric(t *testing.T) {
	_, err := bind(t, "commerce.json", "top 5 customers", map[string][]hit{
		"top": {{"pattern:top_n", 0.825, "top N"}}, "customers": hCustomers,
	})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("got %v", err)
	}
}

// "recent" is a time-window pattern with no configured duration: a value
// that cannot be resolved. With a period, it binds.
func TestTimeWindowPattern(t *testing.T) {
	hRecent := []hit{{"pattern:recent", 1.0, "recent"}}
	_, err := bind(t, "commerce.json", "recent customers", map[string][]hit{"recent": hRecent, "customers": hCustomers})
	if qferr.CodeOf(err) != qferr.CodeValueResolutionFailed {
		t.Errorf("no period: got %v", err)
	}
	b := mustBind(t, "commerce.json", "customers recent last month", map[string][]hit{"recent": hRecent, "customers": hCustomers})
	if len(b.Conditions) != 1 || b.Conditions[0].Kind != CondWindow || b.Conditions[0].Field.String() != "customer.created_at" {
		t.Errorf("binding:\n%s", b.Explain(true))
	}
}

func TestSelections(t *testing.T) {
	b := mustBind(t, "commerce.json", "names of customers from India", map[string][]hit{
		"names":     {{"field:customer.name", 0.9, "name"}},
		"customers": hCustomers, "india": hIndia,
	})
	if len(b.Selections) != 1 || b.Selections[0].Field.String() != "customer.name" {
		t.Errorf("binding:\n%s", b.Explain(true))
	}
}

// ===========================================================================
// Strictness: nothing is silently dropped
// ===========================================================================

// "customers from Atlantis" would bind as ALL customers if the unexplained
// word were ignored — a broader answer than asked, with nothing in the output
// to show it.
func TestUnexplainedWordIsRefused(t *testing.T) {
	_, err := bind(t, "commerce.json", "customers from Atlantis", map[string][]hit{"customers": hCustomers})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "Atlantis") {
		t.Errorf("the default error quotes the user's word: %v", err)
	}
}

func TestOrphanNumberIsRefused(t *testing.T) {
	_, err := bind(t, "commerce.json", "customers 42", map[string][]hit{"customers": hCustomers})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("got %v", err)
	}
}

// FINDING (Phase 5): the configured in-phrase "one of" contains a word Phase 3
// parses as the spelled number 1. The span "one of" then swallows a number,
// takes the span-quality penalty, and the operator never forms. Pinned as a
// characterization test: the sentence is refused (safe), never bound with the
// value 1. Fixing it is a config-vocabulary vs number-word question recorded
// in docs/phase-5-results.md, not something to paper over here.
func TestOneOfCollidesWithTheNumberOne(t *testing.T) {
	_, err := bind(t, "commerce.json", "customer spending one of 5", map[string][]hit{
		"customer spending": {{"field:customer.total_spent", 1.0, "customer spend"}},
		"one of":            {{"operator:in", 1.0, "one of"}},
	})
	if err == nil {
		t.Fatal("bound a sentence whose operator Phase 3 read as a number")
	}
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("got %v", err)
	}
}

// A unit is exactly ONE word directly after the number. A second word is not
// a unit, and is refused.
func TestUnitRuleIsBounded(t *testing.T) {
	_, err := bind(t, "commerce.json", "customers who spent more than 5 dollars total", map[string][]hit{
		"customers": hCustomers, "spent": hSpent, "more than": hMoreThan,
		"total": {{"aggregation:sum", 0.4, "total"}},
	})
	// "total" scripted weak (rejected by ranking) so it stays unexplained.
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("got %v; only the first word after a number is its unit", err)
	}

	// Adjacency: "in" is a function word, not an item, so "cash" is the next
	// item after 5 — but not directly after it. Not a unit; refused.
	_, err = bind(t, "commerce.json", "customers who spent more than 5 in cash", map[string][]hit{
		"customers": hCustomers, "spent": hSpent, "more than": hMoreThan,
	})
	if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("got %v; a word separated from the number is not its unit", err)
	}
}

func TestAmbiguousReadingNeverReachesBinding(t *testing.T) {
	_, err := bind(t, "ambiguity.json", "amount", map[string][]hit{
		"amount": {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
	})
	if qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Errorf("got %v", err)
	}
}

func TestNoEntityIsNoSemanticMatch(t *testing.T) {
	_, err := bind(t, "commerce.json", "more than", map[string][]hit{"more than": hMoreThan})
	if qferr.CodeOf(err) != qferr.CodeNoSemanticMatch {
		t.Errorf("got %v", err)
	}
}

// ===========================================================================
// Determinism and redaction
// ===========================================================================

func TestBindingIsDeterministic(t *testing.T) {
	hits := map[string][]hit{
		"customers": hCustomers, "india": hIndia, "bought": hBought, "laptops": hLaptops,
		"order amount": hOrderAmt, "over": hOver,
	}
	first := mustBind(t, "commerce.json", "customers from India who bought laptops with order amount over 500", hits).Explain(true)
	for i := 0; i < 30; i++ {
		if got := mustBind(t, "commerce.json", "customers from India who bought laptops with order amount over 500", hits).Explain(true); got != first {
			t.Fatalf("run %d differs:\n%s\nvs\n%s", i, got, first)
		}
	}
}

func TestBindingRedactsStoredValuesAndLiterals(t *testing.T) {
	b := mustBind(t, "commerce.json", "customers from India", map[string][]hit{"customers": hCustomers, "india": hIndia})
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if got := fmt.Sprintf(verb, b); strings.Contains(got, `"IN"`) || strings.Contains(got, " IN ") {
			t.Errorf("%s leaks the stored value: %s", verb, got)
		}
	}
	if !strings.Contains(b.Explain(true), `"IN"`) {
		t.Error("Explain(true) should reveal the stored value")
	}

	lit := mustBind(t, "commerce.json", "customer name Smith", map[string][]hit{
		"customer name": {{"field:customer.name", 1.0, "customer name"}},
	})
	if strings.Contains(lit.String(), "Smith") {
		t.Errorf("a literal from the sentence leaked: %s", lit)
	}
}

func TestBindErrorRedactsThePhrase(t *testing.T) {
	_, err := bind(t, "commerce.json", "customer status cancelled", map[string][]hit{
		"customer status": {{"field:customer.status", 1.0, ""}},
	})
	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("not a *binding.Error: %T %v", err, err)
	}
	if strings.Contains(be.Error(), "cancelled") || !strings.Contains(be.Error(), "tokens[2:3]") {
		t.Errorf("default: %s", be.Error())
	}
	if !strings.Contains(be.ErrorRevealed(), `"cancelled"`) || be.RedactedValueCount() != 1 {
		t.Errorf("revealed: %s", be.ErrorRevealed())
	}
}

func loadConfig(t *testing.T, name string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
