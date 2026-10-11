package nlp

import (
	"strings"
	"unicode"
)

// TokenKind classifies a token lexically — by what its characters are, not by
// what it means. Meaning is Phase 4's job.
type TokenKind string

const (
	// KindWord is a run of letters, possibly with internal apostrophes or
	// hyphens ("customer's", "e-mail").
	KindWord TokenKind = "word"
	// KindNumber is a run that begins with a digit ("50000", "50,000", "1.5",
	// "30th"). Parsing it into a value is numbers.go's job.
	KindNumber TokenKind = "number"
	// KindPunct is punctuation or a symbol. Kept rather than discarded: a
	// comma separates list items and a question mark ends a question, and both
	// are evidence about where a span may legitimately end.
	KindPunct TokenKind = "punct"
)

// Token is one lexical unit with its position in the normalised input.
type Token struct {
	// Text is the token as written, with original casing.
	Text string
	// Norm is Text case-folded, which is what matching uses.
	Norm string
	// Start and End are byte offsets into the normalised input. Kept so a span
	// can be reported against the text the user sees — a candidate the engine
	// rejects is much easier to argue about when it can be pointed at.
	Start, End int
	// Index is the token's position in the sequence.
	Index int
	Kind  TokenKind
	// Stop marks a function word. Not removed — "from India" and "more than"
	// both depend on one — but marked, so span generation can refuse a span
	// made of nothing else.
	Stop bool
}

// IsWord reports whether the token is a word.
func (t Token) IsWord() bool { return t.Kind == KindWord }

// Tokenize splits normalised input into tokens.
//
// Hand-rolled rather than a regexp for one reason that matters: it keeps byte
// offsets exact through multi-byte runes, so a span always points at the right
// place in the original text. A regexp-based splitter that works on runes
// tends to report rune indices, which are wrong the moment anyone pastes an
// accented character.
func Tokenize(normalized string) []Token {
	var tokens []Token
	runes := []rune(normalized)

	// byteAt maps a rune index to its byte offset, computed once.
	byteAt := make([]int, len(runes)+1)
	off := 0
	for i, r := range runes {
		byteAt[i] = off
		off += len(string(r))
	}
	byteAt[len(runes)] = off

	i := 0
	for i < len(runes) {
		r := runes[i]

		switch {
		case isSpace(r):
			i++

		case unicode.IsDigit(r):
			start := i
			for i < len(runes) && isNumberRune(runes, i) {
				i++
			}
			tokens = append(tokens, newToken(normalized, byteAt, start, i, KindNumber, len(tokens)))

		case unicode.IsLetter(r):
			start := i
			for i < len(runes) && isWordRune(runes, i) {
				i++
			}
			tokens = append(tokens, newToken(normalized, byteAt, start, i, KindWord, len(tokens)))

		default:
			// One token per punctuation rune. Grouping them would make "?!" a
			// single token with no useful meaning.
			tokens = append(tokens, newToken(normalized, byteAt, i, i+1, KindPunct, len(tokens)))
			i++
		}
	}
	return tokens
}

func newToken(src string, byteAt []int, startRune, endRune int, kind TokenKind, index int) Token {
	start, end := byteAt[startRune], byteAt[endRune]
	text := src[start:end]
	norm := FoldCase(text)
	return Token{
		Text:  text,
		Norm:  norm,
		Start: start,
		End:   end,
		Index: index,
		Kind:  kind,
		Stop:  stopWords[norm],
	}
}

// isWordRune decides whether the rune at i continues the current word.
//
// An apostrophe or hyphen only continues a word when a letter follows, so
// "customer's" is one token while "customers'" keeps its trailing apostrophe
// as punctuation, and "well-known" stays whole while a dash used as a dash does
// not swallow the next word.
func isWordRune(runes []rune, i int) bool {
	r := runes[i]
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	if r == '\'' || r == '-' {
		return i+1 < len(runes) && unicode.IsLetter(runes[i+1])
	}
	return false
}

// isNumberRune decides whether the rune at i continues the current number.
//
// A comma or period continues a number only when a digit follows, so "50,000"
// and "1.5" stay whole while the period ending "spent 50." does not get
// swallowed. A trailing letter run is absorbed so "50k", "1st" and "30th"
// arrive as one token for numbers.go to interpret.
func isNumberRune(runes []rune, i int) bool {
	r := runes[i]
	if unicode.IsDigit(r) || unicode.IsLetter(r) {
		return true
	}
	if r == ',' || r == '.' {
		return i+1 < len(runes) && unicode.IsDigit(runes[i+1])
	}
	return false
}

// stopWords are function words: they carry grammar rather than content.
//
// They are marked, never removed. "from India" and "more than 50000" both hinge
// on one, and a pipeline that stripped them would lose the operator phrases the
// config is built around. What the mark buys is the ability to refuse a
// candidate span consisting of nothing but function words, which can never
// match a schema object and would only add noise for the ranker to reject.
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"do": true, "does": true, "did": true,
	"has": true, "have": true, "had": true,
	"of": true, "to": true, "in": true, "on": true, "at": true, "by": true,
	"for": true, "with": true, "as": true, "into": true, "onto": true,
	"from": true,
	"that": true, "which": true, "who": true, "whom": true, "whose": true,
	"this": true, "these": true, "those": true, "it": true, "its": true,
	"me": true, "my": true, "we": true, "our": true, "us": true,
	"show": true, "list": true, "give": true, "get": true, "find": true,
	"display": true, "fetch": true, "return": true, "select": true,
	"please": true, "want": true, "need": true, "would": true, "could": true,
	"can": true, "i": true, "all": true, "any": true, "some": true,
	"there": true, "here": true, "then": true, "than": false, // "than" is operator vocabulary
}

// requestVerbs are the function words above that are imperative verbs of
// request: "show customers", "list orders", "find customers from India".
//
// They are function words only in REQUEST position. The same words are
// content inside a noun phrase — "list price", "display name", "return date",
// "select customers" — and there, reading them as grammar discards a
// qualifier: "show product list price" compiled to product.price (Phase 8
// finding P11). Phase 3 still marks them Stop everywhere, because what is a
// request position is a question about the whole sentence, not about a token;
// the Phase 8 completeness rule in binding decides it (decision 022 §1).
//
// A closed list, and every entry must also be in stopWords (a test holds the
// two together).
var requestVerbs = map[string]bool{
	"show": true, "list": true, "give": true, "get": true, "find": true,
	"display": true, "fetch": true, "return": true, "select": true,
}

// IsRequestVerb reports whether a folded token is one of the request verbs.
func IsRequestVerb(norm string) bool { return requestVerbs[norm] }

// Text reassembles tokens into a string, for reporting a span.
func Text(tokens []Token) string {
	var b strings.Builder
	for i, t := range tokens {
		if i > 0 && t.Kind != KindPunct {
			b.WriteByte(' ')
		}
		b.WriteString(t.Text)
	}
	return b.String()
}
