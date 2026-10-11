package ranking

import "queryforge.local/v2experiment/internal/corpus"

// Exported for internal/configlint (Phase 8, decision 019 §4), which warns
// about phrases shared by targets that ranking will have to tell apart. The
// lint must use ranking's OWN definitions — of which readings are the same,
// and of how words are compared — or the warnings drift from what ranking
// actually does. These are thin wrappers; ranking's behaviour is unchanged.

// Agrees reports whether two corpus entries are the same reading (a value and
// its field, two values of one stored value, the count aggregation and a count
// pattern), so a phrase shared between them is no ambiguity.
func Agrees(a, b corpus.Entry) bool { return agrees(a, b) }

// FoldWords is ranking's lexical comparison form: lower case, identifiers
// split on _ and -, one plural s stripped.
func FoldWords(s string) []string { return foldWords(s) }
