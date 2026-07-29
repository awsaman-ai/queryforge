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

## Things that are not vulnerabilities

- **A model producing a wrong-but-valid query.** The validator bounds what is
  *expressible*, not what is *correct*. Always show a user the query or the
  generated `Explain` text before acting on it.
- **Non-deterministic model output.** Different phrasings can yield different
  ASTs. The deterministic guarantee covers AST → query, not language → AST.
- **Denial of service through expensive queries.** The library warns about
  non-indexed filters and enforces `maxLimit` and `maxNestingDepth`, but it
  cannot know your data volume. Enforce query cost at the database.

## Supported versions

This project is pre-1.0. Fixes land on `main` and in the next tagged release;
older tags are not patched.
