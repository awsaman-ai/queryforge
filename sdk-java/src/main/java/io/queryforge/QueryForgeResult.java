package io.queryforge;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;

/**
 * A compiled query, plus everything the engine can say about it.
 *
 * <p>Which fields are populated depends on the backend: SQL backends fill {@link #getSql()} and
 * {@link #getArgs()}, document backends fill {@link #getDoc()}. The rest — {@link #getAst()},
 * {@link #getExplain()}, {@link #getWarnings()}, {@link #getScope()} — are filled for every
 * backend.
 *
 * <p>Immutable: it describes something that already happened, and mutating it could only ever
 * make an audit record disagree with what ran.
 */
public final class QueryForgeResult {

    private final String backend;
    private final String sql;
    private final List<Object> args;
    private final Map<String, Object> doc;
    private final Map<String, Object> ast;
    private final String explain;
    private final List<String> warnings;
    private final List<ScopeFilter> scope;
    private final String providerUsed;
    private final int repairAttempts;
    private final String raw;

    private QueryForgeResult(
            String backend,
            String sql,
            List<Object> args,
            Map<String, Object> doc,
            Map<String, Object> ast,
            String explain,
            List<String> warnings,
            List<ScopeFilter> scope,
            String providerUsed,
            int repairAttempts,
            String raw) {
        this.backend = backend;
        this.sql = sql;
        this.args = args;
        this.doc = doc;
        this.ast = ast;
        this.explain = explain;
        this.warnings = warnings;
        this.scope = scope;
        this.providerUsed = providerUsed;
        this.repairAttempts = repairAttempts;
        this.raw = raw;
    }

    static QueryForgeResult fromJson(Map<String, Object> obj) {
        List<ScopeFilter> filters = new ArrayList<>();
        for (Object entry : Values.list(obj.get("scope"))) {
            if (entry instanceof Map) {
                filters.add(ScopeFilter.fromJson(Values.map(entry)));
            }
        }
        return new QueryForgeResult(
                Values.string(obj.get("backend")),
                Values.string(obj.get("sql")),
                Values.list(obj.get("args")),
                Values.nullableMap(obj.get("doc")),
                Values.nullableMap(obj.get("ast")),
                Values.string(obj.get("explain")),
                Values.stringList(obj.get("warnings")),
                Collections.unmodifiableList(filters),
                Values.string(obj.get("providerUsed")),
                Values.integer(obj.get("repairAttempts")),
                Values.string(obj.get("raw")));
    }

    /** Which generator produced this query, e.g. {@code mysql}. */
    public String getBackend() {
        return backend;
    }

    /**
     * The parameterized SQL statement.
     *
     * @throws QueryForgeException if this query compiled to a document backend, which produces no
     *     SQL. Returning {@code ""} instead would let a caller execute an empty statement and get
     *     an opaque driver error several frames from the mistake.
     */
    public String getSql() {
        if (sql.isEmpty()) {
            throw new QueryForgeException(
                    "This query compiled to the '" + backend + "' backend, which produces a query "
                            + "document rather than SQL. Use getDoc() instead.",
                    "WRONG_BACKEND");
        }
        return sql;
    }

    /**
     * The bound placeholder values, in the order the SQL expects them.
     *
     * <p>Values are never inlined into the statement — that is the injection guarantee this
     * library rests on. Pass these to your driver alongside {@link #getSql()}.
     */
    public List<Object> getArgs() {
        return args;
    }

    /**
     * The compiled query document for a document backend.
     *
     * @throws QueryForgeException if this query compiled to a SQL backend.
     */
    public Map<String, Object> getDoc() {
        if (doc == null) {
            throw new QueryForgeException(
                    "This query compiled to the '" + backend + "' backend, which produces SQL rather "
                            + "than a query document. Use getSql() and getArgs() instead.",
                    "WRONG_BACKEND");
        }
        return doc;
    }

    /** The validated intermediate representation, or an empty map if none was reported. */
    public Map<String, Object> getAst() {
        return ast == null ? Collections.emptyMap() : ast;
    }

    /** A deterministic prose rendering of what the query does. */
    public String getExplain() {
        return explain;
    }

    /** Non-fatal advisories, e.g. a filter on a non-indexed field. Never null. */
    public List<String> getWarnings() {
        return warnings;
    }

    /** The caller-imposed filters that were AND-ed into the query. Never null. */
    public List<ScopeFilter> getScope() {
        return scope;
    }

    /** Which model answered, when a fallback chain is configured. May be empty. */
    public String getProviderUsed() {
        return providerUsed;
    }

    /** How many validation repairs were needed before a conforming AST was produced. */
    public int getRepairAttempts() {
        return repairAttempts;
    }

    /** The model's verbatim reply; populated only when {@code includeRaw} was requested. */
    public String getRaw() {
        return raw;
    }

    /** True when this result carries SQL rather than a query document. */
    public boolean isSql() {
        return !sql.isEmpty();
    }

    @Override
    public String toString() {
        return "QueryForgeResult{backend=" + backend + ", " + (isSql() ? "sql=" + sql : "doc=" + doc) + "}";
    }
}
