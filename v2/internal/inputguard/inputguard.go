// Package inputguard is the request's input boundary (Phase 8 gate, decision
// 022 \u00A73): a request is ONE query, so SQL statement and comment syntax in it
// is refused before anything reads it.
//
// THE FINDING (I07). "customers from India; select * from orders" compiled
// to customers from India who have orders. Phase 3 reads ";" as punctuation
// and "select" as a function word, so two requests merged into one reading.
// The SQL was safe — one SELECT, every value a parameter — and it answered a
// question nobody asked.
//
// THE RULE is about syntax, not vocabulary. It never looks for a keyword
// ("SELECT", "DROP"): that is a list an attacker writes around, and the
// read-only rule (decision 021 \u00A72) already owns the write keywords. It
// refuses the four delimiters that separate statements and open or close
// comments in SQL:
//
//	;    statement separator
//	--   line comment
//	/*   block comment opener
//	*/   block comment closer
//
// A request that contains one is SECURITY_REJECTED at the input stage, before
// extraction, retrieval or any later stage runs, so it produces no reading,
// no plan and no query. Nothing is stripped and the rest read: a request with
// its separator removed is a different request (the same reason
// maxInputLength refuses rather than truncates).
//
// WHY REFUSE WHAT COULD NOT REACH THE SQL. The compiler never copies user
// text into SQL (values are parameters, identifiers come from the config, and
// the output guard checks the finished text), so none of these characters can
// terminate a statement or comment one out. They are refused because of what
// they mean in the REQUEST: a separator says "here a second request begins",
// and a comment marker says "ignore what follows" — and the pipeline, which
// reads every word, would do neither. Either way the query would not be the
// question. Refusing is the only answer that is true to both readings.
//
// WHAT IS MATCHED. The request after nlp.Normalize — the text every later
// stage reads — so a separator cannot hide behind a zero-width character
// ("-\u200B-") or a typographic dash Normalize folds ("——"). Then each
// character whose Unicode compatibility form (NFKC) is one of ";", "-", "/"
// or "*" is read as that character: the fullwidth and small forms, and U+037E
// GREEK QUESTION MARK, which NFC itself maps to ";". The table below is that
// set, derived from Unicode 16.0 data, not chosen by hand; a test pins it.
//
// NOT matched: "#" (a comment only in MySQL; in PostgreSQL, the dialect the
// compiler emits, it is an operator, and in English it is "number"), a single
// "-" or "/" or "*" (a hyphen, a slash, a star), and spaced forms ("- -",
// "/ *"), which no SQL lexer reads as a delimiter either.
package inputguard

import (
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
)

// compat maps each character whose NFKC form is one of the delimiter
// characters to that character (Unicode 16.0).
var compat = map[rune]rune{
	'\u037E': ';', // GREEK QUESTION MARK
	'\uFE14': ';', // PRESENTATION FORM FOR VERTICAL SEMICOLON
	'\uFE54': ';', // SMALL SEMICOLON
	'\uFF1B': ';', // FULLWIDTH SEMICOLON
	'\uFE63': '-', // SMALL HYPHEN-MINUS
	'\uFF0D': '-', // FULLWIDTH HYPHEN-MINUS
	'\uFF0F': '/', // FULLWIDTH SOLIDUS
	'\uFE61': '*', // SMALL ASTERISK
	'\uFF0A': '*', // FULLWIDTH ASTERISK
}

// Delimiter names what was found, from a closed list: the refusal message
// uses these words and never the request's own text (decision 013).
type Delimiter string

const (
	StatementSeparator Delimiter = "a statement separator (;)"
	LineComment        Delimiter = "a line-comment marker (--)"
	BlockCommentOpen   Delimiter = "a block-comment opener (/*)"
	BlockCommentClose  Delimiter = "a block-comment closer (*/)"
)

// Find returns the first delimiter in the request and its position, in
// characters from the start of the normalised request; ok is false when there
// is none.
func Find(request string) (d Delimiter, at int, ok bool) {
	var prev rune
	i := 0
	for _, r := range nlp.Normalize(request) {
		if c, mapped := compat[r]; mapped {
			r = c
		}
		switch {
		case r == ';':
			return StatementSeparator, i, true
		case prev == '-' && r == '-':
			return LineComment, i - 1, true
		case prev == '/' && r == '*':
			return BlockCommentOpen, i - 1, true
		case prev == '*' && r == '/':
			return BlockCommentClose, i - 1, true
		}
		prev = r
		i++
	}
	return "", 0, false
}

// Check refuses a request that contains a delimiter (SECURITY_REJECTED).
func Check(request string) error {
	d, at, found := Find(request)
	if !found {
		return nil
	}
	return qferr.New(qferr.CodeSecurityRejected,
		"the request contains SQL statement syntax: %s at character %d of the normalised request. "+
			"A request is one query; statement separators and comment markers are refused, never read around",
		d, at)
}
