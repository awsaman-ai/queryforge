package queryforge

// Physical identifier safety.
//
// Values are bound; identifiers are not. Every table and column name this
// library emits is written into the statement text directly, because no SQL
// dialect lets a placeholder stand for an identifier. That is sound only for as
// long as identifiers really do come from a trusted place, and two of them did
// not:
//
//   - a scope key, which is a plain map key the calling application supplies and
//     which PhysicalName passes through verbatim when the config does not
//     declare it (the documented, common tenant-column case);
//   - a config `mapping` value or backend `table`, which is trusted in the sense
//     that an operator wrote it, but was never checked to be an identifier at
//     all.
//
// Both are closed here, at the two moments the string enters the library: scope
// normalization (scope.go) and config load (config.go). A string that is not a
// legal identifier path is rejected with an error naming the offender, rather
// than reaching a generator where it would become syntax.
//
// Quoting is the second half. It is deliberately conservative — see quoteIdent.

import "strings"

// validIdentPath reports whether s is a legal physical identifier path: one or
// more dot-separated segments, each beginning with a letter or underscore and
// continuing with letters, digits or underscores.
//
// The rule is intentionally narrower than what a database would accept. A column
// genuinely called `weird name` is legal in Postgres if quoted, but admitting it
// here would mean deciding at every call site whether a given identifier is
// quoted-safe; refusing it means the answer is always yes. Configs that need
// such a column can add a plain-named view, which is cheaper than the class of
// bug this rules out.
//
// The dotted form is allowed because Mongo paths ("items.sku", "address.city")
// are identifiers here too, and because a SQL mapping may legitimately qualify a
// column with its table.
func validIdentPath(s string) bool {
	if s == "" {
		return false
	}
	for _, seg := range strings.Split(s, ".") {
		if !validIdent(seg) {
			return false
		}
	}
	return true
}

// validIdent reports whether one segment matches [A-Za-z_][A-Za-z0-9_]*.
func validIdent(seg string) bool {
	if seg == "" {
		return false
	}
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z', ch == '_':
			// legal anywhere in an identifier
		case ch >= '0' && ch <= '9':
			if i == 0 {
				return false // an identifier may not start with a digit
			}
		default:
			return false
		}
	}
	return true
}

// quoteIdentPath renders an identifier path for a SQL dialect, quoting each
// segment that needs it and joining with dots.
func quoteIdentPath(s string, q identQuoter) string {
	if !strings.Contains(s, ".") {
		return q.quote(s)
	}
	segs := strings.Split(s, ".")
	for i, seg := range segs {
		segs[i] = q.quote(seg)
	}
	return strings.Join(segs, ".")
}

// identQuoter is a dialect's identifier-quoting rule.
type identQuoter struct {
	open, close string // the delimiters: "\"" for Postgres, "`" for MySQL
}

// quote returns the identifier as it should appear in the statement.
//
// It quotes only what must be quoted: a reserved word, or a name that is not a
// plain lowercase identifier. Everything else is emitted bare, byte-identical to
// what this library has always produced.
//
// Quoting unconditionally would have been simpler and is what most builders do,
// but it is not a free change on Postgres: an unquoted identifier is folded to
// lower case, so a config mapping `customerName` resolves today against a column
// physically named `customername` — which is what `CREATE TABLE … (customerName
// text)` actually creates. Emitting `"customerName"` would stop matching it. The
// conservative rule fixes the case that is broken (a reserved word is a parse
// error nothing catches at load time) without breaking the case that works.
//
// Identifiers reaching here have already passed validIdentPath, so there is no
// delimiter to escape; the replacement below is belt and braces for a caller
// that constructs a Config in Go and bypasses load-time validation.
func (q identQuoter) quote(s string) string {
	if !q.needsQuoting(s) {
		return s
	}
	return q.open + strings.ReplaceAll(s, q.close, q.close+q.close) + q.close
}

// needsQuoting reports whether the identifier cannot be written bare.
func (q identQuoter) needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	if sqlReservedWords[strings.ToLower(s)] {
		return true
	}
	// Anything outside plain lowercase [a-z_][a-z0-9_]* is left alone only when
	// it is a mixed-case name (see the note on folding above); genuinely illegal
	// characters cannot occur post-validation, but quote them if they do.
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch == '_':
		case ch >= 'A' && ch <= 'Z':
			// Mixed case: deliberately NOT quoted, so existing configs keep
			// resolving through the server's own folding rules.
		case ch >= '0' && ch <= '9':
			if i == 0 {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// sqlReservedWords are the words that cannot be used bare as an identifier.
//
// The list is the union that matters in practice: the Postgres reserved
// category plus the MySQL keywords that most often turn up as real column names
// (`order`, `key`, `rank`, `groups`, `read`, `lines`). One list serves both
// dialects — quoting a word only one of them reserves is harmless, while missing
// one is a parse error at the customer's database, found at run time.
var sqlReservedWords = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true,
	"array": true, "as": true, "asc": true, "asymmetric": true, "authorization": true,
	"between": true, "binary": true, "both": true, "by": true,
	"case": true, "cast": true, "check": true, "collate": true, "column": true,
	"condition": true, "constraint": true, "create": true, "cross": true,
	"current_date": true, "current_role": true, "current_time": true,
	"current_timestamp": true, "current_user": true,
	"database": true, "default": true, "deferrable": true, "delete": true, "desc": true,
	"describe": true, "distinct": true, "do": true, "drop": true,
	"else": true, "end": true, "except": true, "exists": true, "explain": true,
	"false": true, "fetch": true, "float": true, "for": true, "foreign": true,
	"from": true, "full": true, "function": true,
	"grant": true, "group": true, "groups": true, "having": true,
	"in": true, "index": true, "initially": true, "inner": true, "insert": true,
	"int": true, "integer": true, "intersect": true, "interval": true, "into": true, "is": true,
	"join": true, "key": true, "keys": true, "leading": true, "left": true,
	"like": true, "limit": true, "lines": true, "localtime": true, "localtimestamp": true, "lock": true,
	"match": true, "natural": true, "not": true, "null": true,
	"offset": true, "on": true, "only": true, "or": true, "order": true, "outer": true, "over": true,
	"partition": true, "placing": true, "primary": true, "range": true, "rank": true,
	"read": true, "references": true, "rename": true, "replace": true, "returning": true,
	"right": true, "rows": true, "schema": true, "select": true, "session_user": true,
	"show": true, "similar": true, "some": true, "symmetric": true, "system": true,
	"table": true, "then": true, "to": true, "trailing": true, "true": true,
	"union": true, "unique": true, "unlock": true, "update": true, "usage": true,
	"user": true, "using": true, "values": true, "variadic": true, "verbose": true,
	"when": true, "where": true, "window": true, "with": true, "write": true,
}
