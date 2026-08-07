# Security Policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub Security Advisories](https://github.com/awsaman-ai/queryforge/security/advisories/new)
rather than opening a public issue.

Include what you did, what happened, and what you expected. A minimal config plus
the AST or question that triggers it is the most useful thing you can send.

## Scope

QueryForge translates natural language into database queries. It **never connects
to a database and never executes anything** — it returns a query string or
document and stops there. Whatever you do with that output is outside this
library's control, and securing execution (credentials, connection limits, row
level permissions) remains your responsibility.

Within that boundary, these are the properties the library intends to guarantee.
A demonstrated break in any of them is a security bug:

| Property | What a break looks like |
|---|---|
| **Read-only output** | Any input that causes a generator to emit something other than a read (`SELECT` / `find`). The AST has no mutation node, so this should be impossible by construction. |
| **No injection** | Any user-influenced value that reaches the statement text instead of being bound as a parameter. Only config-supplied identifiers may be written into a query directly. |
| **Field concealment** | Any route by which a field marked `queryable: false` or `returnable: false` reaches the caller — through the default projection, a hand-written AST, an error message, or introspection. |
| **Vocabulary bounds** | Any AST that passes validation while referencing a field, operator, or enum value the config does not define. |
| **Credential handling** | Any path that writes an API key into generated output, an error message, or a log line. Keys are read from the environment by name and must never appear in a config file. |

## What the caller must do

Two controls cannot live in this library, because the library does not execute
anything. Neither is optional.

### Set a statement timeout

The `regex` operator forwards its pattern to the datastore. Postgres `~` and
Mongo `$regex` both evaluate it **inside the database process, once per candidate
row**, and a short pattern can be exponentially expensive. QueryForge caps
pattern length (`policy.maxRegexLength`, 256 by default) and rejects patterns
that nest an unbounded quantifier inside a quantified group — `(a+)+`, `(x+x+)+y`
— which is the family that costs exponential time. That screen is structural and
narrow by design: it is not a decision procedure, and it says nothing about
alternation overlap such as `(a|ab)*`.

So bound the execution, where the cost is actually paid:

```go
// Postgres — per connection, or per transaction with SET LOCAL
db.Exec("SET statement_timeout = '5s'")

// MongoDB — per operation
opts := options.Find().SetMaxTime(5 * time.Second)
```

Prefer turning regex off entirely where you do not need it. It is opt-in per
field via `policy.allowRegexOn`; a field whose `operators` list omits `regex`
cannot be pattern-matched at all, which is the strongest form of this control.

### Bound the scope keys you pass

`Scope` keys are validated as identifiers (letters, digits, underscores,
optionally dot-separated) before they reach a generator, so a key carrying SQL or
a `$`-prefixed Mongo operator is rejected with `ErrScope`. That is a backstop,
not the design. Scope values must come from the **session or token**, never from
a request body — a caller who chooses their own tenant id has defeated the
isolation regardless of how the key is spelled.

## Things that are not vulnerabilities

- **A model producing a wrong-but-valid query.** The validator bounds what is
  *expressible*, not what is *correct*. Always show a user the query or the
  generated `Explain` text before acting on it.
- **Non-deterministic model output.** Different phrasings can yield different
  ASTs. The deterministic guarantee covers AST → query, not language → AST.
- **Denial of service through expensive queries.** The library warns about
  non-indexed filters and bounds the shape of a request — `maxLimit`,
  `maxNestingDepth`, `maxFilterNodes`, `maxListLength`, `maxValueLength`,
  `maxRegexLength` — but it cannot know your data volume, and a query that is
  small to state can be enormous to run. Enforce query cost at the database; see
  *Set a statement timeout* above.

## Supported versions

This project is pre-1.0. Fixes land on `main` and in the next tagged release;
older tags are not patched.
