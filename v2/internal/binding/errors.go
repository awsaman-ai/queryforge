package binding

import (
	"fmt"

	"queryforge.local/v2experiment/internal/qferr"
)

// phraseArg is the placeholder a message passes where the offending phrase
// belongs. Error() renders it as a token position, ErrorRevealed() as the
// quoted phrase — the same split as ranking.VerdictError (decision 013): user
// text stays out of the default error path.
const phraseArg = "{phrase}"

// Error is a binding refusal. It implements the CLI's revealer interface.
type Error struct {
	coded      *qferr.Error
	format     string
	args       []any
	start, end int
	phrase     qferr.Sensitive
	hasPhrase  bool
}

func (b *binder) issue(code qferr.Code, at int, format string, args ...any) error {
	it := b.items[at]
	e := &Error{format: format, args: args, start: it.start, end: it.end, phrase: qferr.Sensitive(it.text)}
	for _, a := range args {
		if s, ok := a.(string); ok && s == phraseArg {
			e.hasPhrase = true
		}
	}
	e.coded = qferr.New(code, "%s", e.render(false))
	return e
}

func (e *Error) render(reveal bool) string {
	args := make([]any, len(e.args))
	for i, a := range e.args {
		if s, ok := a.(string); ok && s == phraseArg {
			if reveal {
				args[i] = fmt.Sprintf("the phrase %q", e.phrase.Reveal())
			} else {
				args[i] = fmt.Sprintf("the phrase at tokens[%d:%d]", e.start, e.end)
			}
			continue
		}
		args[i] = a
	}
	return fmt.Sprintf(e.format, args...)
}

// Error is the redacted rendering.
func (e *Error) Error() string { return e.coded.Error() }

// Code is the stable failure category.
func (e *Error) Code() qferr.Code { return e.coded.Code }

// Unwrap exposes the coded error.
func (e *Error) Unwrap() error { return e.coded }

// ErrorRevealed quotes the phrase. DEBUG ONLY.
func (e *Error) ErrorRevealed() string { return e.render(true) }

// RedactedValueCount is 1 when the default rendering withheld the phrase.
func (e *Error) RedactedValueCount() int {
	if e.hasPhrase {
		return 1
	}
	return 0
}
