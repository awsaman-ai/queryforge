package compiler

import (
	"strconv"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
)

// This file is the compiler's LAST step: an allowlist over the finished SQL
// text (Phase 8, sections 16.1–16.5, decision 021).
//
// Everything before it keeps the output safe by construction — identifiers
// come from the registry, keywords from switch statements, values become
// parameters. The guard does not trust that. It lexes the rendered text and
// accepts it only if every token belongs to the grammar's closed lexicon:
//
//	keywords       the fixed set render.go writes (SELECT ... NULLS LAST)
//	identifiers    double-quoted, and a configured table or column, or a
//	               generated alias t0, t1, ...
//	placeholders   $1, $2, ... in order, each once, one per parameter
//	integers       LIMIT's count and EXISTS's SELECT 1
//	punctuation    ( ) , . * and the comparison operators
//	one literal    '\' — ESCAPE's argument, the only quote the grammar has
//	one statement  the text starts with SELECT and its only ; is the last byte
//
// Anything else — a comment, a string literal, a second statement, an
// identifier the config does not have, a lowercase keyword, a stray byte — is
// refused as SECURITY_REJECTED. Such a refusal can only mean a compiler bug:
// the guard exists so that a bug is a refusal, never a query. It reports a
// byte offset and a category, never the offending text, which could hold
// user input if a bug had let it through.
//
// "Never unsafe SQL" (the section 16 gate) is then a checked property of every
// query the engine returns, not an argument about how it was built.

var guardKeywords = map[string]bool{
	"SELECT": true, "DISTINCT": true, "FROM": true, "AS": true, "LEFT": true, "JOIN": true,
	"ON": true, "WHERE": true, "AND": true, "OR": true, "NOT": true, "EXISTS": true,
	"IN": true, "LIKE": true, "ESCAPE": true, "GROUP": true, "BY": true, "HAVING": true,
	"ORDER": true, "ASC": true, "DESC": true, "NULLS": true, "LAST": true, "LIMIT": true,
	"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
}

// outputGuard is what Compile calls: guard itself, always, outside tests. It
// is a variable only so that a test can prove Compile calls it — every real
// query passes the guard, so without this no test could tell a Compile that
// skips it from one that does not (found by the Phase 8 mutation run).
var outputGuard = guard

func guardFail(at int, what string) error {
	return qferr.New(qferr.CodeSecurityRejected,
		"the compiled query failed the output check at byte %d: %s; it is never returned", at, what)
}

// guard checks rendered SQL against the lexicon. params is the number of
// parameters the query carries.
func guard(sql string, params int, reg *config.SchemaRegistry) error {
	idents := map[string]bool{}
	for _, e := range reg.Entities() {
		idents[e.Table] = true
	}
	for _, f := range reg.Config().Fields {
		idents[f.Column] = true
	}

	if len(sql) < len("SELECT ") || sql[:len("SELECT ")] != "SELECT " {
		return guardFail(0, "it does not start with SELECT")
	}
	if sql[len(sql)-1] != ';' {
		return guardFail(len(sql)-1, "it does not end with the one statement terminator")
	}

	nextParam := 1
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case c == ' ' || c == '\n':
			i++
		case c == ';':
			if i != len(sql)-1 {
				return guardFail(i, "a statement terminator before the end: a second statement")
			}
			i++
		case c == '"':
			end := i + 1
			for end < len(sql) && sql[end] != '"' {
				end++
			}
			if end >= len(sql) {
				return guardFail(i, "an unterminated quoted identifier")
			}
			name := sql[i+1 : end]
			if !plainIdent(name) || !(idents[name] || isAlias(name)) {
				return guardFail(i, "a quoted identifier that is not a configured table or column or a generated alias")
			}
			i = end + 1
		case c == '$':
			end := i + 1
			for end < len(sql) && isDigit(sql[end]) {
				end++
			}
			n, err := strconv.Atoi(sql[i+1 : end])
			if err != nil || n != nextParam {
				return guardFail(i, "a placeholder out of order")
			}
			nextParam++
			i = end
		case isDigit(c):
			for i < len(sql) && isDigit(sql[i]) {
				i++
			}
		case isUpper(c):
			end := i
			for end < len(sql) && isUpper(sql[end]) {
				end++
			}
			if !guardKeywords[sql[i:end]] {
				return guardFail(i, "a word that is not one of the grammar's keywords")
			}
			i = end
		case c == '\'':
			// The one literal: ESCAPE '\'.
			if i+3 > len(sql) || sql[i:i+3] != `'\'` || i < len("ESCAPE ") || sql[i-len("ESCAPE "):i] != "ESCAPE " {
				return guardFail(i, "a string literal (values are only ever parameters)")
			}
			i += 3
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-', c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			return guardFail(i, "a comment")
		case c == '<' || c == '>':
			i++
			if i < len(sql) && (sql[i] == '=' || (c == '<' && sql[i] == '>')) {
				i++
			}
		case c == '(' || c == ')' || c == ',' || c == '.' || c == '*' || c == '=':
			i++
		default:
			return guardFail(i, "a character outside the grammar's lexicon")
		}
	}
	if nextParam-1 != params {
		return guardFail(len(sql)-1, "the placeholders do not match the parameters")
	}
	return nil
}

func plainIdent(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && isDigit(c))) {
			return false
		}
	}
	return true
}

// isAlias reports a generated table alias: t followed by digits.
func isAlias(s string) bool {
	if len(s) < 2 || s[0] != 't' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
