package queryforge

// Catastrophic-backtracking (ReDoS) shape check for the `regex` operator.
//
// SECURITY-T.md S-3. The `regex` operator forwards its pattern to the datastore
// untouched — Postgres `~` and Mongo `$regex` both evaluate it in the DATABASE
// process, once per candidate row. Two gates already stand in front of it: the
// per-field policy (allowRegexOn / denyRegexOn) and a pattern-length cap. Neither
// helps against the shape that actually costs money, because it is short:
//
//	(a+)+$        24 characters, exponential in the length of the subject
//	(x+x+)+y      well under any length cap
//	([a-z]*)*!    likewise
//
// The common structure is a QUANTIFIED GROUP THAT ITSELF CONTAINS AN UNBOUNDED
// QUANTIFIER. That gives the engine two independent ways to split the same run of
// characters, so a non-matching subject forces it to try every partition — 2^n
// paths. This file rejects exactly that structure, before the pattern leaves the
// library.
//
// Scope, stated plainly, because a guard that is believed to do more than it does
// is worse than none:
//
//   - This is a STRUCTURAL check, not a decision procedure. Deciding whether an
//     arbitrary pattern backtracks catastrophically is not something to attempt
//     in a validator, and a full analysis would need the pattern parsed per
//     dialect. It catches the nested-quantifier family and says nothing about
//     the rest.
//   - It is deliberately not a substitute for a statement timeout. The library
//     does not execute the query, so it cannot impose one; SECURITY.md tells the
//     caller to. This narrows the window, it does not close it.
//   - It is cheap: one left-to-right pass, no allocation beyond a small stack,
//     no regexp compilation. It runs on patterns that already passed the length
//     cap, so the input is bounded before this ever sees it.
//
// Alternation overlap — `(a|a)*`, `(a|ab)*` — is the other classic ReDoS family
// and is NOT detected. Deciding whether two branches can match the same text is
// a language-equivalence question; a check crude enough to be cheap would reject
// ordinary patterns like `(cat|dog)*`. Left out on purpose rather than
// implemented badly.

import "strings"

// unsafeRegexShape reports whether the pattern contains a quantified group whose
// body also contains an unbounded quantifier, and returns the offending fragment
// for the error message.
//
// Returns ("", false) for every pattern it does not object to, including
// malformed ones: this is a safety screen, not a syntax checker. A pattern with
// unbalanced parentheses is the database's error to report, and inventing a
// second opinion here would reject patterns the datastore accepts.
func unsafeRegexShape(pat string) (fragment string, unsafe bool) {
	// One entry per currently-open group. `start` is the index of its "(" so the
	// fragment can be quoted back; `hasUnbounded` records whether an unbounded
	// quantifier has been seen anywhere inside it so far.
	type group struct {
		start        int
		hasUnbounded bool
	}
	var stack []group

	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '\\':
			// An escape covers the next byte, whatever it is. `\+` is a literal
			// plus, not a quantifier, and `\(` opens no group. Skipping the pair
			// is the whole handling this needs.
			i++

		case '[':
			// Inside a character class every metacharacter is literal: `[+*]`
			// matches a plus or a star and quantifies nothing. Skip to the
			// unescaped ']', remembering that a ']' in first position (or first
			// after a leading '^') is itself a literal, per POSIX and PCRE.
			i = endOfCharClass(pat, i)

		case '(':
			stack = append(stack, group{start: i})

		case ')':
			if len(stack) == 0 {
				continue // unbalanced; not this function's problem — see doc comment
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]

			// The quantifier, if any, is what immediately follows the ")".
			q, next := quantifierAt(pat, i+1)

			// THE FINDING: an unbounded quantifier applied to a group that
			// already contains one. `(a+)+` — the outer + can hand the inner +
			// any split of the same characters, and on a non-match the engine
			// tries all of them.
			if q == quantUnbounded && top.hasUnbounded {
				return pat[top.start:next], true
			}

			// Propagate upward. A group is "unbounded" to its parent when it
			// carries an unbounded quantifier of its own, or when something
			// inside it was unbounded and the group is not the thing that bounds
			// it — a group repeated a bounded number of times, `(a+){2}`, still
			// exposes its inner `+` to whatever quantifies an enclosing group.
			if len(stack) > 0 {
				parent := &stack[len(stack)-1]
				if q == quantUnbounded || top.hasUnbounded {
					parent.hasUnbounded = true
				}
			}
			i = next - 1 // skip the quantifier we just consumed; the loop re-increments

		case '*', '+':
			// An unbounded quantifier on a plain atom (`a+`, `.*`). It matters
			// only as evidence about the group it sits in.
			if len(stack) > 0 {
				stack[len(stack)-1].hasUnbounded = true
			}

		case '{':
			// `{n,}` is unbounded; `{n}` and `{n,m}` are not. A brace that is not
			// a well-formed quantifier is a literal and is ignored.
			q, next := quantifierAt(pat, i)
			if q == quantUnbounded && len(stack) > 0 {
				stack[len(stack)-1].hasUnbounded = true
			}
			if next > i {
				i = next - 1
			}
		}
	}
	return "", false
}

// The three things the byte after an atom can be.
const (
	quantNone      = iota // no quantifier
	quantBounded          // ? or {n} or {n,m} — repetition is capped
	quantUnbounded        // * or + or {n,} — repetition is not
)

// quantifierAt classifies the quantifier beginning at index i and returns the
// index just past it (past any lazy/possessive `?` or `+` modifier too, so the
// caller resumes at the next real token).
//
// Returns (quantNone, i) when there is no quantifier there.
func quantifierAt(pat string, i int) (kind int, next int) {
	if i >= len(pat) {
		return quantNone, i
	}
	switch pat[i] {
	case '*', '+':
		return quantUnbounded, skipQuantifierModifier(pat, i+1)
	case '?':
		return quantBounded, skipQuantifierModifier(pat, i+1)
	case '{':
		// Scan `{`digits[`,`[digits]]`}`. Anything else is a literal brace.
		j := i + 1
		start := j
		for j < len(pat) && pat[j] >= '0' && pat[j] <= '9' {
			j++
		}
		if j == start { // `{` not followed by a digit: literal
			return quantNone, i
		}
		if j < len(pat) && pat[j] == '}' { // {n}
			return quantBounded, skipQuantifierModifier(pat, j+1)
		}
		if j >= len(pat) || pat[j] != ',' { // malformed
			return quantNone, i
		}
		j++ // past the comma
		hadMax := false
		for j < len(pat) && pat[j] >= '0' && pat[j] <= '9' {
			j++
			hadMax = true
		}
		if j >= len(pat) || pat[j] != '}' { // malformed
			return quantNone, i
		}
		if hadMax { // {n,m}: capped
			return quantBounded, skipQuantifierModifier(pat, j+1)
		}
		return quantUnbounded, skipQuantifierModifier(pat, j+1) // {n,}
	}
	return quantNone, i
}

// skipQuantifierModifier steps over a lazy (`?`) or possessive (`+`) suffix.
//
// Laziness changes which match is preferred, not how many paths exist, so `(a+?)+?`
// is no safer than `(a+)+` and must not slip through by looking like a different
// token. A truly possessive quantifier does cut the backtracking, but PCRE-only
// syntax that Postgres rejects outright is not worth carving an exception for.
func skipQuantifierModifier(pat string, i int) int {
	if i < len(pat) && (pat[i] == '?' || pat[i] == '+') {
		return i + 1
	}
	return i
}

// endOfCharClass returns the index of the ']' closing the class that opens at
// index i, or the last index of the pattern when the class is never closed.
func endOfCharClass(pat string, i int) int {
	j := i + 1
	if j < len(pat) && pat[j] == '^' { // negated class: the ^ is not the payload
		j++
	}
	if j < len(pat) && pat[j] == ']' { // a leading ']' is a literal member
		j++
	}
	for ; j < len(pat); j++ {
		if pat[j] == '\\' {
			j++ // escaped: the next byte cannot close the class
			continue
		}
		if pat[j] == ']' {
			return j
		}
	}
	return len(pat) - 1 // unterminated; treat the rest as class content
}

// describeUnsafeRegex renders the validation message for a rejected pattern.
// Split out so the wording lives next to the reasoning that produced it.
func describeUnsafeRegex(fragment string) string {
	const maxFragment = 40 // the message is fed back to the model; keep it short
	shown := fragment
	if len(shown) > maxFragment {
		shown = shown[:maxFragment] + "…"
	}
	return "regex pattern " + strings.TrimSpace(shown) +
		" nests an unbounded quantifier inside a quantified group, which can take " +
		"exponential time to evaluate; rewrite it with a bounded repetition such as {1,20}"
}
