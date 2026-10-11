package binding

import (
	"errors"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
)

// Decision 022 §1: a meaningful qualifier in a candidate phrase cannot be
// silently discarded. Scripted scores are MiniLM's real ones for the Phase 8
// findings (P10's `--rank --debug`), so each refusal is shown against the
// reading ranking really accepts.

var (
	hProduct  = []hit{{"entity:product", 1.0, "product"}, {"relationship:order_product", 1.0, "product"}}
	hPrice    = []hit{{"field:product.price", 1.0, "price"}, {"entity:order", 0.685, "sale"}, {"entity:product", 0.538, "item"}}
	hDiscount = []hit{{"field:product.price", 0.561, "price"}, {"entity:order", 0.586, "sale"}, {"entity:product", 0.414, "goods"}}
)

// accepted reports whether ranking accepted the span with this text as id:
// the reading the old mechanism would have compiled.
func accepted(res ranking.Result, text, id string) bool {
	for _, c := range res.Cover {
		s := res.Spans[c]
		if top, ok := s.TopCandidate(); ok && s.Span.Norm == text && s.Decision == ranking.Accept && top.Entry.ID == id {
			return true
		}
	}
	return false
}

// refusedFor asserts an UNSUPPORTED_QUERY naming the qualifier rule, the
// word's position and the target.
func refusedFor(t *testing.T, err error, word, target string) {
	t.Helper()
	if err == nil || qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Fatalf("err = %v, want UNSUPPORTED_QUERY", err)
	}
	msg := err.Error()
	for _, want := range []string{"the word at " + word, "the phrase as a whole is none of", "has no other role in the reading", target} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
}

// P10, exactly. Ranking accepts "discount" as product.price (similarity
// 0.561 + entity context) beside "price": the verdict is OK, so before the
// gate the query compiled to SELECT price. The rule refuses it for its own
// stated reason.
func TestP10DiscountPriceIsRefused(t *testing.T) {
	reg := registry(t, "commerce.json")
	hits := map[string][]hit{"product": hProduct, "discount": hDiscount, "price": hPrice}
	_, res := rankReg(t, reg, "show product discount price", hits)
	if res.Err() != nil || !accepted(res, "discount", "field:product.price") || !accepted(res, "price", "field:product.price") {
		t.Fatalf("premise: ranking must accept both spans as product.price (verdict %v)", res.Err())
	}
	_, err := bindReg(t, reg, "show product discount price", hits)
	refusedFor(t, err, "tokens[2:3]", "field:product.price")

	// The same phrase without the entity, and with the head first.
	_, err = bind(t, "commerce.json", "show discount price", map[string][]hit{"discount": hDiscount, "price": hPrice})
	refusedFor(t, err, "tokens[1:2]", "field:product.price")
	_, err = bind(t, "commerce.json", "show price discount", map[string][]hit{"discount": hDiscount, "price": hPrice})
	refusedFor(t, err, "tokens[2:3]", "field:product.price")
}

// P11, exactly: "list" is a Phase 3 function word, so nothing ever saw it.
// In the middle of the sentence it is a request verb in noun position.
func TestP11ListPriceIsRefused(t *testing.T) {
	reg := registry(t, "commerce.json")
	hits := map[string][]hit{"product": hProduct, "price": hPrice}
	_, res := rankReg(t, reg, "show product list price", hits)
	if res.Err() != nil || len(res.Unexplained) != 0 {
		t.Fatalf("premise: ranking accepts the sentence with nothing unexplained (verdict %v, unexplained %v)", res.Err(), res.Unexplained)
	}
	_, err := bindReg(t, reg, "show product list price", hits)
	if err == nil || qferr.CodeOf(err) != qferr.CodeUnsupportedQuery ||
		!strings.Contains(err.Error(), "the word at tokens[2:3] is a request verb inside the sentence") {
		t.Fatalf("err = %v", err)
	}
	for _, s := range []string{"customers who return orders", "show display name of products", "products with list price below 50"} {
		h := map[string][]hit{"customers": hCustomers, "orders": hOrders, "products": hProduct, "name": {{"field:product.name", 1.0, "name"}}, "price": hPrice, "below": {{"operator:lt", 1.0, "below"}}}
		for k := range h {
			if !strings.Contains(" "+s+" ", " "+k+" ") {
				delete(h, k)
			}
		}
		if _, err := bind(t, "commerce.json", s, h); err == nil || !strings.Contains(err.Error(), "request verb inside the sentence") {
			t.Errorf("%q: err = %v", s, err)
		}
	}
}

// One span holding a complete surface plus an extra word: the lexical-0.5
// shape the finding was first recorded as.
func TestAWordInsideTheSpanIsNotDiscarded(t *testing.T) {
	for _, c := range []struct {
		sentence, span, target, word string
		hits                         []hit
	}{
		{"show discount price", "discount price", "field:product.price", "tokens[1:2]", []hit{{"field:product.price", 0.95, "price"}}},
		{"show total amount", "total amount", "field:order.amount", "tokens[1:2]", []hit{{"field:order.amount", 0.95, "amount"}}},
		{"vip customers", "vip customers", "entity:customer", "tokens[0:1]", []hit{{"entity:customer", 0.95, "customer"}}},
		{"customers from south India", "south india", "value:customer.country:alias:<redacted>", "tokens[2:3]", []hit{{"value:customer.country:alias:India", 0.95, "India"}}},
		// Words of two DIFFERENT surfaces of one target ("cost", "price")
		// make a phrase the config does not have.
		{"show cost price", "cost price", "field:product.price", "tokens[1:2]", []hit{{"field:product.price", 0.95, "cost"}}},
	} {
		hits := map[string][]hit{c.span: c.hits}
		if strings.HasPrefix(c.sentence, "customers") {
			hits["customers"] = hCustomers
		}
		reg := registry(t, "commerce.json")
		_, res := rankReg(t, reg, c.sentence, hits)
		if !accepted(res, c.span, c.hits[0].id) {
			t.Fatalf("%q: premise: ranking must accept %q", c.sentence, c.span)
		}
		_, err := bindReg(t, reg, c.sentence, hits)
		refusedFor(t, err, c.word, c.target)
		if err != nil && strings.Contains(err.Error(), "India") {
			t.Errorf("%q: the refusal names a config value: %v", c.sentence, err)
		}
	}
}

// What the rule must leave alone: the config's own words, the owner entity,
// a relationship's endpoints, a value's field, a word with its own reading,
// paraphrase with no complete surface, and request verbs in request position.
func TestWordsWithARoleAreNotRefused(t *testing.T) {
	for _, c := range []struct {
		sentence string
		hits     map[string][]hit
	}{
		// The phrase is one of the target's configured surfaces.
		{"show unit price", map[string][]hit{"unit price": {{"field:product.price", 1.0, "unit price"}}}},
		{"show product unit price", map[string][]hit{"product unit price": {{"field:product.price", 0.95, "unit price"}}}},
		// The owner entity inside the span.
		{"show customer amount spent", map[string][]hit{"customer amount spent": {{"field:customer.total_spent", 0.95, "amount spent"}}}},
		// A relationship's endpoint.
		{"customers who placed orders", map[string][]hit{"customers": hCustomers, "placed orders": {{"relationship:customer_orders", 0.95, "placed"}}}},
		// A value beside its own field, as one phrase.
		{"customers with country India", map[string][]hit{"customers": hCustomers, "country": {{"field:customer.country", 1.0, "country"}}, "india": hIndia}},
		// A word with its own reading.
		{"average price", map[string][]hit{"average": {{"aggregation:avg", 1.0, "average"}}, "price": hPrice}},
		// Paraphrase: no complete surface of total_spent in "money spent".
		{"customers with money spent over 100", map[string][]hit{"customers": hCustomers, "money spent": {{"field:customer.total_spent", 0.814, "amount spent"}}, "over": hOver}},
		// Request verbs framing the request.
		{"list customers from India", map[string][]hit{"customers": hCustomers, "india": hIndia}},
		{"please show customers from India", map[string][]hit{"customers": hCustomers, "india": hIndia}},
		{"give me a list of customers from India", map[string][]hit{"customers": hCustomers, "india": hIndia}},
		// Exact phrase.
		{"show product price", map[string][]hit{"product price": {{"field:product.price", 1.0, "product price"}}}},
	} {
		if _, err := bind(t, "commerce.json", c.sentence, c.hits); err != nil {
			t.Errorf("%q: %v", c.sentence, err)
		}
	}
}

// A refusal of this rule is a qferr coded error that never carries a config
// value or the user's word (decision 013): positions and targets only.
func TestQualifierRefusalIsStructural(t *testing.T) {
	_, err := bind(t, "commerce.json", "show product zorblatt price", map[string][]hit{
		"product": hProduct, "zorblatt": {{"field:product.price", 0.6, "price"}}, "price": hPrice})
	var qe *qferr.Error
	if !errors.As(err, &qe) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "zorblatt") || strings.Contains(strings.ToLower(err.Error()), "zorblatt") {
		t.Errorf("the refusal quotes the user's word: %v", err)
	}
}

// Decision 022 §2 at binding: a malformed number refuses the sentence.
func TestMalformedNumberIsRefusedAtBinding(t *testing.T) {
	_, err := bind(t, "commerce.json", "customers who spent more than 12x", map[string][]hit{
		"customers": hCustomers, "spent": hSpent, "more than": hMoreThan})
	if err == nil || qferr.CodeOf(err) != qferr.CodeValueResolutionFailed || !strings.Contains(err.Error(), "tokens[5:6]") {
		t.Errorf("err = %v", err)
	}
}

// A request verb INSIDE an accepted span is not explained by ranking's
// acceptance (ranking read it as a function word): "return date" accepted as
// order.placed_at on "date" and the spelled-out "order date" alone. Only a
// configured phrase containing the verb explains it.
func TestARequestVerbInsideASpanNeedsTheConfig(t *testing.T) {
	reg := registry(t, "commerce.json")
	hits := map[string][]hit{"orders": hOrders, "return date": {{"field:order.placed_at", 0.9, "order date"}}}
	_, res := rankReg(t, reg, "orders by return date", hits)
	if res.Err() != nil || !accepted(res, "return date", "field:order.placed_at") {
		t.Fatalf("premise: ranking must accept \"return date\" as placed_at (verdict %v)", res.Err())
	}
	_, err := bindReg(t, reg, "orders by return date", hits)
	if err == nil || !strings.Contains(err.Error(), "tokens[2:3] is a request verb inside the sentence") ||
		!strings.Contains(err.Error(), "no configured phrase in the reading contains it") {
		t.Errorf("err = %v", err)
	}

	// With the config's own "list price", the phrase is the config's.
	cfg := *reg.Config()
	cfg.Fields = append([]config.Field(nil), cfg.Fields...)
	for i := range cfg.Fields {
		if cfg.Fields[i].Entity == "product" && cfg.Fields[i].Name == "price" {
			cfg.Fields[i].Synonyms = append(append([]string(nil), cfg.Fields[i].Synonyms...), "list price")
		}
	}
	withList := config.NewSchemaRegistry(&cfg)
	b, err := bindReg(t, withList, "show product list price", map[string][]hit{
		"product": hProduct, "list price": {{"field:product.price", 1.0, "list price"}}})
	if err != nil {
		t.Fatalf("a configured \"list price\" was refused: %v", err)
	}
	if len(b.Selections) != 1 || b.Selections[0].Field.String() != "product.price" {
		t.Errorf("binding:\n%s", b.Explain(true))
	}
}

// ===========================================================================
// Decision 022 §5: an independent span may not resolve by similarity alone to
// a target the sentence already names by the config's own words (G33, G34).
// ===========================================================================

func TestSimilarityMayNotDuplicateAnExplicitTarget(t *testing.T) {
	for _, c := range []struct {
		sentence   string
		hits       map[string][]hit
		span, said string
	}{
		{"products with price and discount above 10", map[string][]hit{
			"products": hProduct, "price": hPrice, "discount": hDiscount, "above": hAbove},
			"tokens[4:5]", "tokens[2:3]"},
		{"price of discount", map[string][]hit{"price": hPrice, "discount": hDiscount},
			"tokens[2:3]", "tokens[0:1]"},
	} {
		reg := registry(t, "commerce.json")
		_, res := rankReg(t, reg, c.sentence, c.hits)
		if res.Err() != nil || !accepted(res, "discount", "field:product.price") {
			t.Fatalf("%q: premise: ranking must accept the similarity-only span (verdict %v)", c.sentence, res.Err())
		}
		_, err := bindReg(t, reg, c.sentence, c.hits)
		if err == nil || qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
			t.Fatalf("%q: err = %v", c.sentence, err)
		}
		for _, want := range []string{
			"the phrase at " + c.span + " resolves to field:product.price by similarity alone",
			"already names field:product.price at " + c.said,
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: message lacks %q: %v", c.sentence, want, err)
			}
		}
	}
}

// Paraphrase with independent configured evidence is untouched, however often
// the target is named; so is a paraphrase of a target named nowhere else.
func TestParaphraseWithConfiguredEvidenceSurvives(t *testing.T) {
	for _, c := range []struct {
		sentence string
		hits     map[string][]hit
	}{
		// Both phrases hold "spent", a configured word of total_spent.
		{"customers with amount spent over 100 and total spent below 50", map[string][]hit{
			"customers": hCustomers, "amount spent": {{"field:customer.total_spent", 1.0, "amount spent"}},
			"over": hOver, "total spent": {{"field:customer.total_spent", 1.0, "total spent"}}, "below": {{"operator:lt", 1.0, "below"}}}},
		// Phase 7's V03: "located" is customer.country by similarity alone, and
		// "India" is one of its VALUES — a different target, so the reading
		// stands. The wider rule refused this, and was rejected for it.
		{"clients located in India", map[string][]hit{
			"clients": hCustomers, "located": {{"field:customer.country", 0.78, "location"}}, "india": hIndia}},
		// A paraphrase of a target the sentence names nowhere else.
		{"customers registered last year", map[string][]hit{
			"customers": hCustomers, "registered": {{"field:customer.created_at", 0.75, "registration date"}}}},
	} {
		if _, err := bind(t, "commerce.json", c.sentence, c.hits); err != nil {
			t.Errorf("%q: %v", c.sentence, err)
		}
	}
}

// ===========================================================================
// Decision 022 §6: the config, not the position, decides whether the opening
// request verb is the request frame (the gate's `list price of products`).
// ===========================================================================

// withListPrice is commerce plus a field whose synonym starts with a request
// verb — the shape the investigation turned on.
func withListPrice(t *testing.T) *config.SchemaRegistry {
	t.Helper()
	reg := registry(t, "commerce.json")
	cfg := *reg.Config()
	cfg.Fields = append(append([]config.Field(nil), cfg.Fields...), config.Field{
		Entity: "product", Name: "list_price", Type: config.TypeNumber,
		Description: "The published price before any discount.",
		Synonyms:    []string{"list price"},
	})
	return config.NewSchemaRegistry(&cfg)
}

func TestOpeningRequestVerbIsTheFrameUnlessTheConfigClaimsIt(t *testing.T) {
	hListPrice := []hit{{"field:product.list_price", 1.0, "list price"}}

	// No configured surface holds "list": the verb opens the request, and the
	// sentence reads as the command it looks like.
	b := mustBind(t, "commerce.json", "list price of products", map[string][]hit{
		"price": hPrice, "products": hProduct})
	if len(b.Selections) != 1 || b.Selections[0].Field.String() != "product.price" {
		t.Errorf("commerce: binding:\n%s", b.Explain(true))
	}

	// With "list price" configured and read, the configured field wins.
	reg := withListPrice(t)
	b, err := bindReg(t, reg, "list price of products", map[string][]hit{
		"list price": hListPrice, "products": hProduct})
	if err != nil || len(b.Selections) != 1 || b.Selections[0].Field.String() != "product.list_price" {
		t.Errorf("configured: %v\n%s", err, b.Explain(true))
	}

	// THE GUARANTEE. The config holds "list price" and the sentence spells it
	// out, but the reading did not use it: the verb may not be swallowed as
	// the frame, so the sentence is refused instead of answered from a reading
	// that dropped the word.
	_, err = bindReg(t, reg, "list price of products", map[string][]hit{
		"price": hPrice, "products": hProduct})
	if err == nil || !strings.Contains(err.Error(), "tokens[0:1] is a request verb") {
		t.Errorf("guarantee: err = %v", err)
	}

	// A surface is claimed only where it RUNS from the verb: "list products by
	// price" holds both words, and is an ordinary command.
	if _, err := bindReg(t, reg, "list products by price", map[string][]hit{
		"products": hProduct, "price": hPrice}); err != nil {
		t.Errorf("list products by price: %v", err)
	}
	// And an unrelated request verb is unaffected.
	if _, err := bindReg(t, reg, "list customers from India", map[string][]hit{
		"customers": hCustomers, "india": hIndia}); err != nil {
		t.Errorf("list customers from India: %v", err)
	}
}

// The refusal names the FIRST explicit mention, so the message does not
// depend on how many times the sentence names the target (found by mutation:
// keying the "already seen" check differently made the last one win, and no
// test had two explicit mentions).
func TestRefusalNamesTheFirstExplicitMention(t *testing.T) {
	hits := map[string][]hit{
		"price":    hPrice,
		"cost":     {{"field:product.price", 1.0, "cost"}, {"entity:order", 0.4, "sale"}},
		"discount": hDiscount,
	}
	reg := registry(t, "commerce.json")
	_, res := rankReg(t, reg, "show price and cost and discount", hits)
	if !accepted(res, "price", "field:product.price") || !accepted(res, "cost", "field:product.price") {
		t.Fatalf("premise: both explicit spans must be accepted (verdict %v)", res.Err())
	}
	_, err := bindReg(t, reg, "show price and cost and discount", hits)
	if err == nil || !strings.Contains(err.Error(), "already names field:product.price at tokens[1:2]") {
		t.Errorf("err = %v, want the first mention (tokens[1:2])", err)
	}
}

// A DESCRIPTION is prose, not the config's name for a target (decision 019),
// so it cannot make a similarity-only phrase count as naming one. commerce's
// total_spent is described as "Total amount of money spent by the customer",
// and "money" must still not stand for it beside the configured phrase.
func TestDescriptionsDoNotNameATarget(t *testing.T) {
	hits := map[string][]hit{
		"customers":   hCustomers,
		"money":       {{"field:customer.total_spent", 0.9, "spending"}, {"field:order.amount", 0.4, "amount"}},
		"over":        hOver,
		"total spent": {{"field:customer.total_spent", 1.0, "total spent"}, {"field:order.amount", 0.4, "amount"}},
		"below":       {{"operator:lt", 1.0, "below"}, {"operator:gt", 0.4, "above"}},
	}
	reg := registry(t, "commerce.json")
	_, res := rankReg(t, reg, "customers with money over 100 and total spent below 50", hits)
	if !accepted(res, "money", "field:customer.total_spent") {
		t.Fatalf("premise: ranking must accept the similarity-only span (verdict %v)", res.Err())
	}
	_, err := bindReg(t, reg, "customers with money over 100 and total spent below 50", hits)
	if err == nil || !strings.Contains(err.Error(), "resolves to field:customer.total_spent by similarity alone") {
		t.Errorf("err = %v", err)
	}
}

// §6 claims the opening verb only for a COMPOUND — a surface of more than one
// word. A one-word surface that happens to be a request verb is that word on
// its own, not a phrase the verb opens (found by mutation).
func TestAOneWordSurfaceDoesNotClaimTheOpeningVerb(t *testing.T) {
	reg := registry(t, "commerce.json")
	cfg := *reg.Config()
	cfg.Entities = append([]config.Entity(nil), cfg.Entities...)
	for i := range cfg.Entities {
		if cfg.Entities[i].Name == "product" {
			cfg.Entities[i].Synonyms = append(append([]string(nil), cfg.Entities[i].Synonyms...), "list")
		}
	}
	withList := config.NewSchemaRegistry(&cfg)
	if _, err := bindReg(t, withList, "list customers from India", map[string][]hit{
		"customers": hCustomers, "india": hIndia}); err != nil {
		t.Errorf("a one-word surface claimed the opening verb: %v", err)
	}
}
