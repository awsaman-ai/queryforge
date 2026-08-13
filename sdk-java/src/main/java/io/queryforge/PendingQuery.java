package io.queryforge;

import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * A question that has been described but not yet compiled.
 *
 * <p>Nothing runs until a terminal method ({@link #toSql()}, {@link #toMongo()},
 * {@link #result()}, …) is called, and the answer is cached afterwards — so reading
 * {@code toSql()} and then {@code explain()} off the same object costs one model call, not two.
 *
 * <p>Every builder returns a <em>new</em> instance rather than mutating this one, which is what
 * makes a partially-configured query safe to keep as a template:
 *
 * <pre>{@code
 * PendingQuery base = forge.query("open orders").scope(Map.of("tenantId", tenant));
 * String sql = base.toSql();
 * String prose = base.explain();   // same cached answer, no second model call
 * }</pre>
 *
 * <p>Instances are not thread-safe. Because the builders never mutate, sharing a
 * <em>configured but uncompiled</em> query across threads is safe; what is not safe is two
 * threads racing to call a terminal method on the same instance, which could spend two model
 * calls where one was intended.
 */
public final class PendingQuery {

    private final QueryForge forge;
    private final String text;
    private final Map<String, Object> scope;
    private final Map<String, Object> options;
    private final String requestId;

    private QueryForgeResult result; // memoized on the first terminal call

    PendingQuery(QueryForge forge, String text, Map<String, Object> scope, Map<String, Object> options) {
        this(forge, text, scope, options, null);
    }

    PendingQuery(
            QueryForge forge,
            String text,
            Map<String, Object> scope,
            Map<String, Object> options,
            String requestId) {
        this.forge = forge;
        this.text = text;
        this.scope = scope;
        this.options = options;
        this.requestId = requestId;
    }

    // ------------------------------------------------------------- builders

    /**
     * Adds filters the application imposes regardless of what was asked.
     *
     * <p>Subscription, tenant, user, enterprise ids — values that come from the session, not from
     * the question. They are AND-ed onto the query after validation, so they can only narrow the
     * result, and the model is never told they exist.
     *
     * <p>Calling this more than once merges, so a base query's scope survives.
     */
    public PendingQuery scope(Map<String, ?> extra) {
        if (extra == null || extra.isEmpty()) {
            return this;
        }
        Map<String, Object> merged = new LinkedHashMap<>(scope);
        merged.putAll(extra);
        return new PendingQuery(forge, text, Collections.unmodifiableMap(merged), options, requestId);
    }

    /** Adds a single scope filter. Convenience for the common one-tenant case. */
    public PendingQuery scope(String field, Object value) {
        return scope(Collections.singletonMap(field, value));
    }

    /** Bounds the whole operation, model call included. */
    public PendingQuery timeout(long millis) {
        if (millis <= 0) {
            throw new InvalidRequestException(
                    "timeout must be positive, got " + millis, "INVALID_REQUEST", Collections.emptyList());
        }
        return withOption("timeoutMs", (int) millis);
    }

    /** Bounds the validation-repair retries. {@code 0} means one attempt with no repairs. */
    public PendingQuery maxRepairs(int n) {
        if (n < 0) {
            throw new InvalidRequestException(
                    "maxRepairs cannot be negative, got " + n, "INVALID_REQUEST", Collections.emptyList());
        }
        return withOption("maxRepairs", n);
    }

    /**
     * Returns the model's verbatim reply on {@link QueryForgeResult#getRaw()}.
     *
     * <p>Off by default: the reply is derived from the user's question, and results tend to end up
     * in logs.
     */
    public PendingQuery includeRaw() {
        return withOption("includeRaw", Boolean.TRUE);
    }

    /**
     * Reports the effective AST — scope predicates included — as the result's AST.
     *
     * <p>Off by default, in which case the AST is exactly what the model produced and can be fed
     * back to {@link QueryForge#generate} unchanged. Turn it on for an audit log that must store
     * one object proving what ran.
     */
    public PendingQuery scopeInAst() {
        return withOption("scopeInAst", Boolean.TRUE);
    }

    /**
     * Correlates this query's log lines with your own request.
     *
     * <p>The id is stamped on every log record the SDK writes for this call and is handed to the
     * engine, which stamps it on its records too — so one {@code request_id=…} search returns both
     * halves of a query. Pass the trace id your framework already has.
     *
     * <p>Without this, the SDK generates a fresh id per call, which still correlates the SDK and
     * engine lines but cannot be tied to anything upstream.
     */
    public PendingQuery requestId(String id) {
        if (id == null || id.trim().isEmpty()) {
            throw new InvalidRequestException(
                    "requestId must be a non-empty string", "INVALID_REQUEST", Collections.emptyList());
        }
        return new PendingQuery(forge, text, scope, options, id.trim());
    }

    private PendingQuery withOption(String key, Object value) {
        Map<String, Object> next = new LinkedHashMap<>(options);
        next.put(key, value);
        return new PendingQuery(forge, text, scope, Collections.unmodifiableMap(next), requestId);
    }

    // ------------------------------------------------------------ terminals

    /** Compiles the query and returns everything the engine reported. */
    public QueryForgeResult result() {
        if (result == null) {
            result = forge.translate(text, scope, options, requestId);
        }
        return result;
    }

    /**
     * Returns the parameterized SQL statement.
     *
     * <p>Values are <strong>not</strong> inlined — that is the injection guarantee this library
     * rests on. Pass {@link #toArgs()} alongside it to your driver:
     *
     * <pre>{@code
     * PreparedStatement stmt = conn.prepareStatement(pending.toSql());
     * List<Object> args = pending.toArgs();
     * for (int i = 0; i < args.size(); i++) {
     *     stmt.setObject(i + 1, args.get(i));
     * }
     * }</pre>
     */
    public String toSql() {
        return result().getSql();
    }

    /** Returns the bound placeholder values, in the order the SQL expects them. */
    public List<Object> toArgs() {
        return result().getArgs();
    }

    /** Returns the compiled query document for a document backend. */
    public Map<String, Object> toMongo() {
        return result().getDoc();
    }

    /** Returns the validated intermediate representation. */
    public Map<String, Object> toAst() {
        return result().getAst();
    }

    /** Returns a deterministic prose rendering of what the query does. */
    public String explain() {
        return result().getExplain();
    }

    /** Returns non-fatal advisories, e.g. a filter on a non-indexed field. */
    public List<String> warnings() {
        return result().getWarnings();
    }

    @Override
    public String toString() {
        return "PendingQuery{" + text + ", " + (result == null ? "pending" : "compiled") + "}";
    }
}
