// Package nlp turns a user sentence into clean candidate spans for semantic
// matching.
//
// SCOPE. This layer does not understand language and must not try. Section 11
// of the brief is explicit: the purpose is to produce spans, not meaning. What
// a span REFERS to — a field, an operator, a value — is decided by retrieval and
// ranking in Phases 4 and 5, which have the config and the schema graph to
// decide it with. The only things classified here are the ones that are
// lexical facts rather than semantic judgments: numbers, dates and connectives.
//
// Everything is deterministic and dependency-free. No model, no statistical
// tagger, no network. A failure in Phase 10's error taxonomy has to be
// attributable to a rule someone can read, which rules out a library whose
// mistakes we could not explain.
package nlp

import (
	"strings"
	"unicode"
)

// Normalize prepares raw input for tokenisation: it folds the characters that
// are typographically different but linguistically identical, and collapses
// whitespace.
//
// It deliberately does NOT case-fold. Case is dropped per-token during
// tokenisation instead, because the original casing is worth keeping on the
// token for later phases — an all-caps value or a capitalised proper noun is
// weak evidence a ranker may want, and throwing it away here would be
// irreversible.
//
// UNICODE: this is a pragmatic fold, not NFKC. Full normalisation needs
// golang.org/x/text, and v2 has no third-party dependencies. What is handled
// here is what actually arrives from a keyboard, a word processor or a
// copy-paste: smart quotes, dashes, non-breaking and zero-width spaces, and
// invisible control characters. Combining-accent sequences are NOT decomposed,
// so "café" written with a combining acute will not match one written
// precomposed. See docs/decisions/009-nlp-approach.md.
func Normalize(input string) string {
	var b strings.Builder
	b.Grow(len(input))

	lastWasSpace := true // leading space is trimmed by treating the start as one
	for _, r := range input {
		switch {
		case isIgnorable(r):
			// Zero-width joiners, bidi marks and other format characters carry
			// no meaning here and would otherwise split a word into two tokens
			// invisibly — the kind of bug that is impossible to see in a bug
			// report.
			continue

		case isSpace(r):
			if !lastWasSpace {
				b.WriteRune(' ')
				lastWasSpace = true
			}
			continue

		default:
			b.WriteRune(fold(r))
			lastWasSpace = false
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// fold maps typographic variants onto their plain ASCII equivalents.
//
// These matter because a config author types an apostrophe and a user pastes a
// smart quote, or the other way round. Without folding, "customer's" and
// "customer’s" are different strings and only one of them matches.
func fold(r rune) rune {
	switch r {
	case '‘', '’', '‚', '‛', '′', '´', '`':
		return '\'' // single quotes and primes
	case '“', '”', '„', '‟', '″':
		return '"' // double quotes
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-' // hyphens, dashes, minus sign
	case '⁄', '∕':
		return '/' // fraction and division slashes
	case '\u00A0', '\u202F', '\u2007':
		return ' ' // non-breaking spaces; also caught by isSpace, kept for clarity
	case '…':
		// An ellipsis becomes a single period rather than three, so it does not
		// look like sentence-ending punctuation repeated.
		return '.'
	}
	return r
}

// isSpace covers Unicode whitespace plus the separators that unicode.IsSpace
// misses.
func isSpace(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	switch r {
	case '\u00A0', '\u1680', '\u2007', '\u202F', '\u205F', '\u3000':
		return true
	}
	return false
}

// isIgnorable reports characters that should vanish entirely.
//
// Control and format characters are removed rather than turned into spaces:
// turning them into spaces would split a word, while removing them joins the
// halves back into the word the user meant. A soft hyphen inside "custo-mer"
// is the common case.
func isIgnorable(r rune) bool {
	if r == '\uFEFF' { // byte-order mark
		return true
	}
	if unicode.Is(unicode.Cf, r) { // format characters: ZWJ, ZWNJ, bidi marks
		return true
	}
	if unicode.IsControl(r) && !isSpace(r) {
		return true
	}
	return false
}

// FoldCase lowercases a token for matching. Separate from Normalize so the
// original casing survives on the token.
func FoldCase(s string) string { return strings.ToLower(s) }
