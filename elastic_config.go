package queryforge

// Elasticsearch/OpenSearch config validation: nested-path shape, physical
// index/alias name safety, and business-rule index routing.
//
// The security property this file exists to hold: every index name
// QueryForge can ever resolve to is written by whoever owns the config —
// Default, a rule's or branch's Indexes, or IndexPattern's literal text — and
// the query supplies nothing but the VALUE of a field the config explicitly
// opted into routing (Field.RoutingField). See source_es.go for the runtime
// half of that contract.

import (
	"fmt"
	"strconv"
	"strings"
)

// validateElasticPaths checks the path shape of every declared
// elasticsearch/opensearch mapping on f, and — when NestedPath is set — that
// at least one of them actually sits inside it.
//
// Mirrors validateMongoPaths in shape (a malformed dot path or a nested
// declaration the mapping does not sit inside yields a query matching
// nothing) but is kept fully separate: Mongo's ElemMatch and ES's NestedPath
// solve the same problem for backends whose query shapes have nothing else in
// common.
func (c *Config) validateElasticPaths(f *Field) error {
	for backend, phys := range f.Mapping {
		if !isElasticBackend(backend) || phys == "" {
			continue
		}
		if !validDotPath(phys) {
			return fmt.Errorf("config: field %q has an invalid %s path %q "+
				"(expected dot-separated segments, e.g. \"items.sku\"; no empty segments)", f.Name, backend, phys)
		}
	}
	if f.NestedPath == "" {
		return nil
	}
	if !validDotPath(f.NestedPath) {
		return fmt.Errorf("config: field %q has an invalid nestedPath %q "+
			"(expected the dot path of the nested object, e.g. \"items\")", f.Name, f.NestedPath)
	}
	found := false
	for backend, phys := range f.Mapping {
		if !isElasticBackend(backend) || phys == "" {
			continue
		}
		found = true
		if rel := strings.TrimPrefix(phys, f.NestedPath+"."); rel == phys || rel == "" {
			return fmt.Errorf("config: field %q declares nestedPath %q but its %s mapping %q does not sit inside it "+
				"(expected e.g. nestedPath \"items\" with mapping \"items.sku\")", f.Name, f.NestedPath, backend, phys)
		}
	}
	if !found {
		return fmt.Errorf("config: field %q declares nestedPath %q but has no elasticsearch/opensearch mapping "+
			"pointing inside it — nestedPath only matters together with a mapping path under that object", f.Name, f.NestedPath)
	}
	return nil
}

// validESIndexName reports whether s is a usable Elasticsearch/OpenSearch
// index or alias name: the practical subset of the real rule that matters
// here — lowercase only, none of the characters ES reserves in a name, and
// not one of the two names ("." and "..") it refuses outright.
func validESIndexName(s string) bool {
	if s == "" || s == "." || s == ".." || len(s) > 255 {
		return false
	}
	switch s[0] {
	case '-', '_', '+':
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
		case ch == '-', ch == '_', ch == '.', ch == '+':
		default:
			return false
		}
	}
	return true
}

// extractPatternToken finds IndexPattern's single "{token}" placeholder,
// reporting false when there is none or more than one.
func extractPatternToken(pattern string) (string, bool) {
	open := strings.IndexByte(pattern, '{')
	close := strings.IndexByte(pattern, '}')
	if open < 0 || close < 0 || close <= open {
		return "", false
	}
	token := pattern[open+1 : close]
	if token == "" || strings.ContainsAny(token, "{}") {
		return "", false
	}
	if strings.ContainsAny(pattern[close+1:], "{}") {
		return "", false // a second placeholder
	}
	return token, true
}

// validESIndexPatternLiteral reports whether pattern's literal text (i.e.
// everything outside its one "{token}" placeholder) is safe to sit inside a
// resolved index name.
func validESIndexPatternLiteral(pattern, token string) bool {
	literal := strings.Replace(pattern, "{"+token+"}", "", 1)
	if literal == pattern {
		return false // token substring not actually present verbatim
	}
	for i := 0; i < len(literal); i++ {
		ch := literal[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9', ch == '-', ch == '_', ch == '.':
		default:
			return false
		}
	}
	return true
}

// dateToken returns the fixed IndexPattern token a date-routing granularity
// requires, e.g. YEAR needs "{yyyy}".
func dateToken(g DateGranularity) (string, bool) {
	switch g {
	case GranularityYear:
		return "yyyy", true
	case GranularityMonth:
		return "yyyy-MM", true
	case GranularityDay:
		return "yyyy-MM-dd", true
	default:
		return "", false
	}
}

// validateElasticSource checks one Elasticsearch/OpenSearch backend's source
// configuration.
func (c *Config) validateElasticSource(backend string, bc BackendConfig) error {
	if n := bc.elasticSourceCount(); n > 1 {
		return fmt.Errorf("config: backends.%s sets more than one source mode "+
			"(index, indexes, alias, and routing are mutually exclusive)", backend)
	}
	if bc.Index != "" && !validESIndexName(bc.Index) {
		return fmt.Errorf("config: backends.%s.index %q is not a valid index name", backend, bc.Index)
	}
	if bc.Alias != "" && !validESIndexName(bc.Alias) {
		return fmt.Errorf("config: backends.%s.alias %q is not a valid alias name", backend, bc.Alias)
	}
	seen := make(map[string]bool, len(bc.Indexes))
	for i, idx := range bc.Indexes {
		if !validESIndexName(idx) {
			return fmt.Errorf("config: backends.%s.indexes[%d] %q is not a valid index name", backend, i, idx)
		}
		if seen[idx] {
			return fmt.Errorf("config: backends.%s.indexes lists %q more than once", backend, idx)
		}
		seen[idx] = true
	}
	if bc.Routing != nil {
		return c.validateIndexRouting(backend, bc.Routing)
	}
	return nil
}

// validateIndexRouting checks a BUSINESS_RULE_INDEX config: a known
// strategy, well-formed and eligible routing fields, valid literal index
// names throughout, and a required fallback.
func (c *Config) validateIndexRouting(backend string, r *IndexRouting) error {
	where := fmt.Sprintf("backends.%s.routing", backend)
	if len(r.Default) == 0 {
		return fmt.Errorf("config: %s requires a non-empty default — the fallback used when the routing field "+
			"is absent or unresolvable, or (for rules/ifElse) nothing matches. QueryForge never falls back to "+
			"an unrestricted wildcard, so this is not optional", where)
	}
	for i, idx := range r.Default {
		if !validESIndexName(idx) {
			return fmt.Errorf("config: %s.default[%d] %q is not a valid index name", where, i, idx)
		}
	}

	routingField := func(name string) error {
		f, ok := c.fieldByName[name]
		if !ok {
			return fmt.Errorf("config: %s references field %q, which is not registered", where, name)
		}
		if !f.RoutingField {
			return fmt.Errorf("config: %s references field %q, which is not marked routingField:true", where, name)
		}
		return nil
	}

	switch r.Strategy {
	case RoutingPattern:
		if r.Field == "" {
			return fmt.Errorf("config: %s strategy %q requires field", where, r.Strategy)
		}
		if err := routingField(r.Field); err != nil {
			return err
		}
		if f := c.fieldByName[r.Field]; f.Type == FieldArray || f.Type == FieldBoolean {
			return fmt.Errorf("config: %s field %q has type %q, which pattern routing does not support "+
				"(use string, number, enum, or date)", where, r.Field, f.Type)
		}
		return c.validateIndexPattern(where, r.IndexPattern, "")

	case RoutingDate:
		if r.Field == "" {
			return fmt.Errorf("config: %s strategy %q requires field", where, r.Strategy)
		}
		if err := routingField(r.Field); err != nil {
			return err
		}
		if f := c.fieldByName[r.Field]; f.Type != FieldDate {
			return fmt.Errorf("config: %s field %q has type %q, but date routing requires type \"date\"",
				where, r.Field, f.Type)
		}
		wantToken, ok := dateToken(r.Granularity)
		if !ok {
			return fmt.Errorf("config: %s.granularity %q is invalid (expected YEAR, MONTH, or DAY)", where, r.Granularity)
		}
		return c.validateIndexPattern(where, r.IndexPattern, wantToken)

	case RoutingRules:
		if len(r.Rules) == 0 {
			return fmt.Errorf("config: %s strategy %q requires at least one rule", where, r.Strategy)
		}
		type dupKey struct {
			field, op, val string
			prio           int
		}
		seen := make(map[dupKey]bool, len(r.Rules))
		for i, rule := range r.Rules {
			condWhere := fmt.Sprintf("%s.rules[%d].when", where, i)
			if err := c.validateRoutingCondition(condWhere, rule.When, routingField); err != nil {
				return err
			}
			if err := validateRoutingIndexes(fmt.Sprintf("%s.rules[%d]", where, i), rule.Indexes); err != nil {
				return err
			}
			k := dupKey{rule.When.Field, string(rule.When.Operator), rule.When.Value, rule.Priority}
			if seen[k] {
				return fmt.Errorf("config: %s.rules[%d] duplicates an earlier rule's condition at the same priority — "+
					"identical conditions can never be disambiguated at query time", where, i)
			}
			seen[k] = true
		}

	case RoutingIfElse:
		if len(r.Branches) == 0 {
			return fmt.Errorf("config: %s strategy %q requires at least one branch", where, r.Strategy)
		}
		for i, br := range r.Branches {
			last := i == len(r.Branches)-1
			switch {
			case br.Else && br.If != nil:
				return fmt.Errorf("config: %s.branches[%d] sets both else and if", where, i)
			case br.Else && !last:
				return fmt.Errorf("config: %s.branches[%d] sets else but is not the last branch", where, i)
			case !br.Else && br.If == nil:
				return fmt.Errorf("config: %s.branches[%d] has neither if nor else", where, i)
			case !br.Else:
				if err := c.validateRoutingCondition(fmt.Sprintf("%s.branches[%d].if", where, i), *br.If, routingField); err != nil {
					return err
				}
			}
			if err := validateRoutingIndexes(fmt.Sprintf("%s.branches[%d]", where, i), br.Indexes); err != nil {
				return err
			}
		}

	default:
		return fmt.Errorf("config: %s has unknown strategy %q (expected pattern, date, rules, or ifElse)", where, r.Strategy)
	}
	return nil
}

// validateIndexPattern checks IndexPattern's shape: exactly one "{token}"
// placeholder (matching wantToken exactly, when non-empty — the "date"
// strategy's fixed vocabulary) and a safe literal portion.
func (c *Config) validateIndexPattern(where, pattern, wantToken string) error {
	token, ok := extractPatternToken(pattern)
	if !ok {
		return fmt.Errorf("config: %s.indexPattern %q must contain exactly one {token} placeholder", where, pattern)
	}
	if wantToken != "" && token != wantToken {
		return fmt.Errorf("config: %s.indexPattern %q has token {%s}, but this granularity requires {%s}",
			where, pattern, token, wantToken)
	}
	if !validESIndexPatternLiteral(pattern, token) {
		return fmt.Errorf("config: %s.indexPattern %q has an invalid literal portion "+
			"(outside {%s}, only lowercase letters, digits, '-', '_', '.' are allowed)", where, pattern, token)
	}
	return nil
}

// validateRoutingIndexes checks one rule's or branch's literal Indexes list.
func validateRoutingIndexes(where string, indexes []string) error {
	if len(indexes) == 0 {
		return fmt.Errorf("config: %s has no indexes", where)
	}
	for i, idx := range indexes {
		if !validESIndexName(idx) {
			return fmt.Errorf("config: %s.indexes[%d] %q is not a valid index name", where, i, idx)
		}
	}
	return nil
}

// validateRoutingCondition checks one RoutingCondition: a registered,
// routing-eligible field; an operator this evaluator supports; and a value
// that parses under the field's own declared type.
func (c *Config) validateRoutingCondition(where string, cond RoutingCondition, routingField func(string) error) error {
	if cond.Field == "" {
		return fmt.Errorf("config: %s has no field", where)
	}
	if err := routingField(cond.Field); err != nil {
		return err
	}
	switch cond.Operator {
	case OpEquals, OpNotEquals, OpGt, OpGte, OpLt, OpLte:
	default:
		return fmt.Errorf("config: %s has operator %q, but routing conditions only support "+
			"equals, notEquals, gt, gte, lt, lte", where, cond.Operator)
	}
	f := c.fieldByName[cond.Field]
	if cond.Operator != OpEquals && cond.Operator != OpNotEquals && f.Type != FieldNumber && f.Type != FieldDate {
		return fmt.Errorf("config: %s uses operator %q on field %q (type %q) — "+
			"gt/gte/lt/lte routing conditions require a number or date field", where, cond.Operator, cond.Field, f.Type)
	}
	if cond.Value == "" {
		return fmt.Errorf("config: %s has no value", where)
	}
	switch f.Type {
	case FieldNumber:
		if _, err := strconv.ParseFloat(cond.Value, 64); err != nil {
			return fmt.Errorf("config: %s value %q is not a valid number for field %q", where, cond.Value, cond.Field)
		}
	case FieldDate:
		if !isDateString(cond.Value) {
			return fmt.Errorf("config: %s value %q is not a valid date for field %q", where, cond.Value, cond.Field)
		}
	case FieldEnum:
		if !f.hasEnumValue(cond.Value) {
			return fmt.Errorf("config: %s value %q is not one of field %q's declared values", where, cond.Value, cond.Field)
		}
	}
	return nil
}
