# QueryForge for Java

Turn a sentence into a parameterized database query, with a validated AST in between.

```xml
<dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.1.2</version>
</dependency>
<!-- plus the engine binary for the platform you run on; see "Platform binaries" -->
<dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.1.2</version>
    <classifier>linux-amd64</classifier>
</dependency>
```

```java
QueryForge forge = QueryForge.mysql(Paths.get("orders.config.json"));
String sql = forge.query("delivered orders over $100 last month").toSql();
```

No server to run, no Go toolchain to install, and **zero runtime dependencies** — not even a JSON
library. The engine ships as a native binary inside the jar; this SDK extracts it, spawns it as a
local subprocess, and turns its reply into Java objects.

---

## How it fits together

QueryForge's engine is written in Go. Everything that decides what a query *means* — the parser,
the AST, validation, the dialects, SQL and Mongo generation — lives there and nowhere else. This
SDK is a wrapper: it validates your arguments, extracts the right binary for your platform, sends
one JSON request, and maps the reply onto Java types.

That is what makes the guarantee worth having: this SDK, the Python one, and the engine's own Go
API produce **byte-identical output** for the same input, because there is only one implementation.

### Why no dependencies

A thin wrapper that drags in Jackson forces its version on every application that uses it, and a
Jackson conflict inside someone else's Spring app is a far worse experience than the few hundred
lines of hand-rolled JSON in `Json.java`. The published artifact needs no dependency resolution at
all, which is also one fewer thing for an enterprise review to sign off.

---

## The two halves of the API

### `query(text)` — the full pipeline

Costs one model call. Needs a `model` block in your config and the API key exported under the name
that block's `apiKeyEnv` gives.

```java
QueryForge forge = QueryForge.postgres(Paths.get("orders.config.json"));
PendingQuery pending = forge.query("cancelled orders from ACME this week");

String sql = pending.toSql();        // SELECT ... WHERE (status = $1 AND ...)
List<Object> args = pending.toArgs();
String prose = pending.explain();
```

Nothing runs until a terminal method is called, and the answer is cached — reading `toSql()` and
then `explain()` off the same object costs **one** model call, not two.

### `generate(ast)` / `validate(ast)` — deterministic

No model call, no network, no API key. Use these to re-compile a stored AST for a second backend,
and in your tests.

```java
QueryForgeResult result = forge.generate(storedAst);
Map<String, Object> doc = QueryForge.mongo(config).generate(storedAst).getDoc();
```

---

## Executing the query

Values are **never inlined** into the statement — that is the injection guarantee the library rests
on. Bind them:

```java
PendingQuery pending = forge.query(question);
try (PreparedStatement stmt = conn.prepareStatement(pending.toSql())) {
    List<Object> args = pending.toArgs();
    for (int i = 0; i < args.size(); i++) {
        stmt.setObject(i + 1, args.get(i));
    }
    try (ResultSet rs = stmt.executeQuery()) {
        // ...
    }
}
```

For MongoDB:

```java
Map<String, Object> doc = QueryForge.mongo(config).query("open orders").toMongo();
MongoCollection<Document> collection = db.getCollection((String) doc.get("collection"));
FindIterable<Document> found = collection.find(new Document((Map<String, Object>) doc.get("filter")));
```

---

## Scope: filters your application imposes

Subscription, tenant, user, enterprise ids — values that come from the session, not from the
question. They are AND-ed onto the query *after* validation, so they can only narrow the result,
and the model is never told they exist.

```java
QueryForge scoped = forge.withScope(Map.of("tenantId", request.getTenantId()));
String sql = scoped.query(userQuestion).toSql();          // every query is scoped

// or per-query, merging with any scope already set
String sql = forge.query(userQuestion).scope("ownerId", user.getId()).toSql();
```

The applied filters come back on the result so an audit log can record exactly what was forced
onto the query:

```java
for (ScopeFilter f : pending.result().getScope()) {
    log.info("{} {} {} (declared={})", f.getField(), f.getOperator(), f.getValue(), f.isDeclared());
}
```

---

## Errors

Every failure maps to a distinct exception. They are all **unchecked**, so a fluent chain does not
need a `try` around every call — handle them once at your request boundary.

```java
try {
    String sql = forge.query(question).toSql();
} catch (UnsupportedRequestException e) {
    return badRequest(e.getMessage());        // written to be shown to the asker
} catch (ValidationException e) {
    for (Detail d : e.getDetails()) {
        log.warn("{} {} {}", d.getCode(), d.getField(), d.getSuggestions());
    }
} catch (QueryForgeException e) {
    log.error("query failed [{}]", e.getCode(), e);
}
```

| Exception | Code | What to do |
|---|---|---|
| `InvalidRequestException` | `INVALID_REQUEST`, `UNKNOWN_OP` | Fix the calling code |
| `InvalidConfigException` | `INVALID_CONFIG` | Fix the config file |
| `UnknownBackendException` | `UNKNOWN_BACKEND` | Use `sql`, `mysql` or `mongo` |
| `InvalidScopeException` | `INVALID_SCOPE` | Fix the scope map — an application bug |
| `ValidationException` | `VALIDATION_FAILED` | Register the field, or rephrase |
| `UnsupportedRequestException` | `UNSUPPORTED_REQUEST` | Show the message; the question needs rephrasing |
| `ModelOutputException` | `MODEL_OUTPUT` | Retry, or switch models |
| `ModelTransportException` | `MODEL_TRANSPORT` | Check the API key and the endpoint |
| `GenerateException` | `GENERATE_FAILED` | The AST is legal but not compilable for this backend |
| `TimeoutException` | `TIMEOUT` | Raise the timeout |
| `BinaryNotFoundException` | `BINARY_NOT_FOUND` | Wrong platform artifact, or set the override |
| `ProtocolException` | `PROTOCOL_ERROR` | Broken install — the binary crashed or is the wrong version |

`QueryForgeException.getCode()` is available on all of them, so an error class introduced by a
newer engine still reaches you as the base class with its code intact.

---

## Configuration

```java
QueryForge.mysql(Paths.get("orders.config.json"));     // from a file
QueryForge.mysql(configMap);                            // from a Map
QueryForge.mysql(QueryForge.parseConfig(jsonText));     // from JSON text
```

Instance-level options:

```java
QueryForge forge = QueryForge.postgres(config)
        .withScope(Map.of("tenantId", "t1"))
        .withTimeout(30_000);
```

Per-query options:

```java
forge.query(text)
     .timeout(10_000)     // millis, model call included
     .maxRepairs(0)       // 0 = one attempt, no repairs
     .includeRaw()        // return the model's verbatim reply
     .scopeInAst()        // report the effective AST, scope included
     .toSql();
```

---

## Backends

| Factory | Engine id | Produces |
|---|---|---|
| `QueryForge.postgres(cfg)` / `QueryForge.sql(cfg)` | `sql` | PostgreSQL, `$1` placeholders |
| `QueryForge.mysql(cfg)` | `mysql` | MySQL, `?` placeholders |
| `QueryForge.mongo(cfg)` | `mongo` | A query document |

---

## Platform binaries

The main artifact carries only classes (~35 KB). One classifier jar per platform carries one
binary (~2.5 MB each), so you add two dependencies: the classes, and the binary for your platform.

```xml
<dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.1.2</version>
</dependency>
<dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.1.2</version>
    <classifier>linux-amd64</classifier>
</dependency>
```

| Platform | Classifier |
|---|---|
| Linux x86-64 | `linux-amd64` |
| Linux ARM64 | `linux-arm64` |
| macOS Intel | `darwin-amd64` |
| macOS Apple Silicon | `darwin-arm64` |
| Windows x86-64 | `windows-amd64` |

Name the classifier that matches where the code will **run**, which is not always where it is
built — a container image assembled on an Apple Silicon laptop wants `linux-amd64`. If you would
rather not hardcode it, a profile in your own pom can select one per platform; the classifier
cannot be chosen for you from inside QueryForge's own pom, because a profile there is evaluated
when QueryForge itself is built and would make it depend on itself.

The binary is extracted from the jar to a content-addressed path under the temp directory on first
use and reused afterwards, so different engine versions on one machine never overwrite each other.

---

## Environment

| Setting | Effect |
|---|---|
| `-Dqueryforge.binary=/path` or `QUERYFORGE_BINARY` | Run this executable instead of the bundled one. Reported, never silently ignored, if it does not work. |
| `-Dqueryforge.cacheDir=/path` or `QUERYFORGE_CACHE_DIR` | Where to extract the binary. Useful when the temp directory is mounted `noexec`. |
| whatever your config's `apiKeyEnv` names | The model API key. Never put the key in the config file. |

The system property wins over the environment variable, so a JVM launch flag can override an
environment inherited from a container image.

Check an installation without needing a config or a key:

```java
System.out.println(QueryForge.engineVersion());   // {success=true, protocol=1.0, ...}
System.out.println(QueryForge.binaryPath());
System.out.println(QueryForge.platformTag());     // darwin-arm64
```

---

## Requirements

Java 11 or later. The floor is deliberately low: it is imposed on every consumer, 11 is still the
most widely deployed LTS in enterprise Java, and nothing here needs a later language feature.

---

## Development

```bash
cd sdk-java && mvn test
```

The integration tests build the engine themselves with `go build` and skip cleanly when no Go
toolchain is present. `QueryForgeSdkTest` runs against a scripted fake to cover every error code,
crashes, corrupted output and protocol mismatches; `JsonTest` covers the hand-rolled codec.

## License

Apache License 2.0 — see [LICENSE](../LICENSE).
