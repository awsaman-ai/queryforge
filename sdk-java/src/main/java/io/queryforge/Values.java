package io.queryforge;

import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Defensive readers for values pulled out of a decoded JSON response.
 *
 * <p>Every field on the wire but {@code success} and {@code protocol} is omitted when empty, so a
 * response legitimately arrives with most keys missing. Each reader therefore takes "absent" and
 * "present but the wrong shape" to the same neutral default rather than throwing: an engine that
 * adds or reshapes an optional field must not turn into a {@link ClassCastException} several
 * frames from the cause.
 */
final class Values {

    private Values() {}

    /** Returns a string value, or {@code ""} when absent or not a string. */
    static String string(Object value) {
        return value instanceof String ? (String) value : "";
    }

    /** Returns an int value, or {@code 0} when absent or not a number. */
    static int integer(Object value) {
        return value instanceof Number ? ((Number) value).intValue() : 0;
    }

    /** Returns a boolean value, or {@code false} when absent or not a boolean. */
    static boolean bool(Object value) {
        return value instanceof Boolean && (Boolean) value;
    }

    /** Returns an immutable list, or an empty one when absent or not a list. */
    @SuppressWarnings("unchecked")
    static List<Object> list(Object value) {
        if (!(value instanceof List)) {
            return Collections.emptyList();
        }
        return Collections.unmodifiableList(new ArrayList<>((List<Object>) value));
    }

    /** Returns an immutable list of strings, skipping any element that is not one. */
    static List<String> stringList(Object value) {
        if (!(value instanceof List)) {
            return Collections.emptyList();
        }
        List<String> out = new ArrayList<>();
        for (Object element : (List<?>) value) {
            if (element instanceof String) {
                out.add((String) element);
            }
        }
        return Collections.unmodifiableList(out);
    }

    /** Returns an immutable map, or an empty one when absent or not a map. */
    @SuppressWarnings("unchecked")
    static Map<String, Object> map(Object value) {
        if (!(value instanceof Map)) {
            return Collections.emptyMap();
        }
        return Collections.unmodifiableMap(new LinkedHashMap<>((Map<String, Object>) value));
    }

    /** Returns a map, or {@code null} when absent — used where absence is meaningful. */
    @SuppressWarnings("unchecked")
    static Map<String, Object> nullableMap(Object value) {
        if (!(value instanceof Map)) {
            return null;
        }
        return Collections.unmodifiableMap(new LinkedHashMap<>((Map<String, Object>) value));
    }
}
