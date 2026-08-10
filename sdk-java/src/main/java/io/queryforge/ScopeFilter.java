package io.queryforge;

import java.util.Map;

/**
 * One caller-imposed filter that was AND-ed into the query.
 *
 * <p>Reported back so an audit log can record exactly what was forced onto a query rather than
 * having to re-derive it from the scope map. {@link #isDeclared()} is true when the config
 * registers the field, meaning its type and operator rules were enforced.
 */
public final class ScopeFilter {

    private final String field;
    private final String operator;
    private final Object value;
    private final boolean declared;

    ScopeFilter(String field, String operator, Object value, boolean declared) {
        this.field = field;
        this.operator = operator;
        this.value = value;
        this.declared = declared;
    }

    static ScopeFilter fromJson(Map<String, Object> obj) {
        Object raw = obj.get("value");
        // Unwrap the AST's tagged-union value into the plain payload. Someone auditing scope
        // wants "ACME", not {"kind":"string","v":"ACME"}.
        Object unwrapped = raw instanceof Map ? ((Map<?, ?>) raw).get("v") : raw;
        return new ScopeFilter(
                Values.string(obj.get("field")),
                Values.string(obj.get("operator")),
                unwrapped,
                Values.bool(obj.get("declared")));
    }

    /** The logical field name this filter applies to. */
    public String getField() {
        return field;
    }

    /** The operator used, e.g. {@code equals} or {@code in}. */
    public String getOperator() {
        return operator;
    }

    /** The value applied, unwrapped from the AST's tagged union. */
    public Object getValue() {
        return value;
    }

    /** True when the config registers this field, so its rules were enforced. */
    public boolean isDeclared() {
        return declared;
    }

    @Override
    public String toString() {
        return field + " " + operator + " " + value;
    }
}
