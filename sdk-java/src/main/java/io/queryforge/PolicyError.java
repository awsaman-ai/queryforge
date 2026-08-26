package io.queryforge;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;

/**
 * The specifics of a {@code POLICY_VIOLATION}: which field triggered a {@code policy.requires}
 * rule, and which companion field(s) would have satisfied it. Mirrors the engine's
 * {@code PolicyErrorDetail} on the wire.
 */
public final class PolicyError {

    private final String field;
    private final List<String> requireAlsoOneOf;
    private final String message;

    PolicyError(String field, List<String> requireAlsoOneOf, String message) {
        this.field = field == null ? "" : field;
        this.requireAlsoOneOf = requireAlsoOneOf == null
                ? Collections.emptyList()
                : Collections.unmodifiableList(new ArrayList<>(requireAlsoOneOf));
        this.message = message == null ? "" : message;
    }

    static PolicyError fromJson(Map<String, Object> obj) {
        return new PolicyError(
                Values.string(obj.get("field")),
                Values.stringList(obj.get("requireAlsoOneOf")),
                Values.string(obj.get("message")));
    }

    /** The field that was filtered and triggered the rule. */
    public String getField() {
        return field;
    }

    /** Filtering one of these fields would have satisfied the rule; never null. */
    public List<String> getRequireAlsoOneOf() {
        return requireAlsoOneOf;
    }

    /** Human-readable explanation — the same text as the exception's own message. */
    public String getMessage() {
        return message;
    }

    @Override
    public String toString() {
        return "PolicyError(field=" + field + ", requireAlsoOneOf=" + requireAlsoOneOf + ")";
    }
}
