// Package readonly is section 16.1's explicit read-only rejection (Phase 8,
// decision 021).
//
// The engine is read-only BY CONSTRUCTION: the compiler's grammar has one
// statement, SELECT, and no plan can express anything else. A write request
// was therefore always refused — before Phase 8, by binding's rule that an
// unexplained word refuses the sentence (UNSUPPORTED_QUERY). This package
// makes the refusal EXPLICIT and gives it its real reason: a sentence that
// uses an SQL write keyword is SECURITY_REJECTED, "the engine only reads".
//
// THE RULE. A token whose folded form is one of the write keywords below
// refuses the request, UNLESS the configuration itself explains the word
// literally: the token lies in an accepted span of the reading, and that
// span's accepted target has a configured name, synonym or phrase that
// contains the word AND whose every word appears in the sentence. A field
// with the synonym "update date", in "customers by update date", is a read.
//
// Similarity is not an explanation. "update date" retrieving created_at on
// meaning alone does not make "update" part of a configured phrase, and a
// write keyword absorbed into a reading on similarity is exactly the case this
// rule exists for — the sentence then asks for something the reading does
// not say. The same line decision 019 draws for values: similarity discovers,
// only literal evidence authorises.
//
// THE KEYWORDS are the SQL statements section 16.1 names, plus MERGE: a closed
// list of SQL vocabulary, not a list of English words. Matching is on the
// bare keyword, after ranking's light fold (case, one plural s): "delete" and
// "deletes" match; "deleted", "created" and "updating" do not. Inflected forms
// are ordinary read vocabulary — "customers created last year" — and treating
// them as writes would refuse reads that the rule has no reason to touch.
// Natural-language write verbs that are not SQL keywords ("remove", "erase")
// are not on the list; they stay refused as unexplained words, as before.
//
// It runs after ranking and before ranking's verdict, in the shared path every
// sentence mode uses (pipeline.rank), so "SELECT * FROM users; DROP TABLE
// users" is refused for writing, not merely for matching nothing.
package readonly

import (
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
)

// Keywords maps each folded write keyword to how the refusal names it. The
// folded form is what ranking.FoldWords produces for the bare keyword.
var Keywords = map[string]string{
	"insert":   "INSERT",
	"update":   "UPDATE",
	"delete":   "DELETE",
	"drop":     "DROP",
	"alter":    "ALTER",
	"truncate": "TRUNCATE",
	"create":   "CREATE",
	"grant":    "GRANT",
	"revoke":   "REVOKE",
	"copy":     "COPY",
	"call":     "CALL",
	"merge":    "MERGE",
}

// Check refuses a sentence that uses a write keyword the configuration does
// not literally explain. The message names the keyword from the closed list
// above and the token position; it never quotes the sentence (decision 013).
func Check(a nlp.Analysis, r ranking.Result) error {
	sentence := map[string]bool{}
	for _, t := range a.Tokens {
		for _, w := range ranking.FoldWords(t.Norm) {
			sentence[w] = true
		}
	}
	for _, t := range a.Tokens {
		if t.Kind != nlp.KindWord {
			continue
		}
		folded := ranking.FoldWords(t.Norm)
		if len(folded) != 1 {
			continue
		}
		name, write := Keywords[folded[0]]
		if !write || explained(t.Index, folded[0], r, sentence) {
			continue
		}
		return qferr.New(qferr.CodeSecurityRejected,
			"the word at tokens[%d:%d] is the write keyword %s; the engine only reads, and no configured phrase in the sentence explains the word",
			t.Index, t.Index+1, name)
	}
	return nil
}

// explained reports whether the token at index is part of an accepted span
// whose accepted target has a configured surface that contains word and is
// spelled out in full by the sentence.
func explained(index int, word string, r ranking.Result, sentence map[string]bool) bool {
	for _, c := range r.Cover {
		s := r.Spans[c]
		if s.Decision != ranking.Accept || index < s.Span.Start || index >= s.Span.End {
			continue
		}
		top, ok := s.TopCandidate()
		if !ok {
			return false
		}
		for _, sf := range top.Entry.Surfaces {
			if sf.Kind == corpus.SurfaceDescription {
				continue
			}
			words := ranking.FoldWords(sf.Text)
			if contains(words, word) && allIn(words, sentence) {
				return true
			}
		}
		return false
	}
	return false
}

func contains(words []string, w string) bool {
	for _, x := range words {
		if x == w {
			return true
		}
	}
	return false
}

func allIn(words []string, set map[string]bool) bool {
	for _, w := range words {
		if !set[w] {
			return false
		}
	}
	return len(words) > 0
}
