package io.queryforge;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * A minimal JSON reader and writer, sufficient for this SDK's wire format.
 *
 * <p>This exists so the SDK has <strong>zero runtime dependencies</strong>. That is not
 * frugality for its own sake: a thin wrapper that drags in Jackson forces its version on every
 * application that uses it, and a Jackson version conflict inside someone else's Spring app is a
 * far worse experience than the few hundred lines here. It also means the published jar needs no
 * dependency resolution at all, which is one fewer thing for an enterprise review to sign off.
 *
 * <p>The scope is deliberately small. It parses and emits exactly the JSON subset RFC 8259
 * defines, mapping objects to {@link LinkedHashMap} (insertion-ordered, so a re-serialized
 * request is stable), arrays to {@link List}, numbers to {@link Long} or {@link Double}, and the
 * rest to {@link String}, {@link Boolean} and {@code null}. It is not a general-purpose binding
 * library and does not try to be: there is no reflection, no annotations, and no streaming.
 *
 * <p>Package-private on purpose — it is an implementation detail, not API. Callers who want a
 * JSON object work with {@code Map<String, Object>}.
 */
final class Json {

    private Json() {}

    // ---------------------------------------------------------------- writing

    /** Serializes a value to compact JSON text. */
    static String write(Object value) {
        StringBuilder out = new StringBuilder();
        writeValue(out, value);
        return out.toString();
    }

    private static void writeValue(StringBuilder out, Object value) {
        if (value == null) {
            out.append("null");
        } else if (value instanceof String) {
            writeString(out, (String) value);
        } else if (value instanceof Boolean) {
            out.append(value.toString());
        } else if (value instanceof Double || value instanceof Float) {
            double d = ((Number) value).doubleValue();
            // JSON has no way to spell these, and emitting the Java literal would produce a
            // document the engine cannot parse. Failing here names the offending value while
            // the caller can still see where it came from.
            if (Double.isNaN(d) || Double.isInfinite(d)) {
                throw new IllegalArgumentException(
                        "Cannot serialize " + d + " to JSON: NaN and Infinity have no JSON representation");
            }
            out.append(trimDouble(d));
        } else if (value instanceof Number) {
            out.append(value.toString());
        } else if (value instanceof Map) {
            writeObject(out, (Map<?, ?>) value);
        } else if (value instanceof Iterable) {
            writeArray(out, (Iterable<?>) value);
        } else if (value instanceof Object[]) {
            writeArray(out, java.util.Arrays.asList((Object[]) value));
        } else {
            // A silent toString() here would send the engine an object nobody meant to send,
            // and it would arrive looking like a legitimate string value.
            throw new IllegalArgumentException(
                    "Cannot serialize " + value.getClass().getName()
                            + " to JSON; scope values must be strings, numbers, booleans, or lists of those");
        }
    }

    /** Renders a double without Java's trailing {@code .0} on whole numbers. */
    private static String trimDouble(double d) {
        if (d == Math.rint(d) && !Double.isInfinite(d) && Math.abs(d) < 1e15) {
            return Long.toString((long) d);
        }
        return Double.toString(d);
    }

    private static void writeObject(StringBuilder out, Map<?, ?> map) {
        out.append('{');
        boolean first = true;
        for (Map.Entry<?, ?> entry : map.entrySet()) {
            if (!first) {
                out.append(',');
            }
            first = false;
            if (entry.getKey() == null) {
                throw new IllegalArgumentException("JSON object keys cannot be null");
            }
            writeString(out, entry.getKey().toString());
            out.append(':');
            writeValue(out, entry.getValue());
        }
        out.append('}');
    }

    private static void writeArray(StringBuilder out, Iterable<?> values) {
        out.append('[');
        boolean first = true;
        for (Object value : values) {
            if (!first) {
                out.append(',');
            }
            first = false;
            writeValue(out, value);
        }
        out.append(']');
    }

    private static void writeString(StringBuilder out, String s) {
        out.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':
                    out.append("\\\"");
                    break;
                case '\\':
                    out.append("\\\\");
                    break;
                case '\n':
                    out.append("\\n");
                    break;
                case '\r':
                    out.append("\\r");
                    break;
                case '\t':
                    out.append("\\t");
                    break;
                case '\b':
                    out.append("\\b");
                    break;
                case '\f':
                    out.append("\\f");
                    break;
                default:
                    // Control characters must be escaped; everything else, including non-ASCII,
                    // travels as UTF-8. The engine reads UTF-8, so escaping it would only make
                    // the payload larger and the logs unreadable.
                    if (c < 0x20) {
                        out.append(String.format("\\u%04x", (int) c));
                    } else {
                        out.append(c);
                    }
            }
        }
        out.append('"');
    }

    // ---------------------------------------------------------------- reading

    /**
     * Parses JSON text into Java values.
     *
     * @throws JsonException if the text is not well-formed JSON
     */
    static Object read(String text) {
        Parser parser = new Parser(text);
        parser.skipWhitespace();
        Object value = parser.parseValue();
        parser.skipWhitespace();
        if (!parser.atEnd()) {
            throw new JsonException("unexpected trailing content at position " + parser.pos);
        }
        return value;
    }

    /** Parses JSON text expected to be an object. */
    @SuppressWarnings("unchecked")
    static Map<String, Object> readObject(String text) {
        Object value = read(text);
        if (!(value instanceof Map)) {
            throw new JsonException(
                    "expected a JSON object, got " + (value == null ? "null" : value.getClass().getSimpleName()));
        }
        return (Map<String, Object>) value;
    }

    /** Signals malformed JSON. Unchecked, because a caller cannot meaningfully recover. */
    static final class JsonException extends RuntimeException {
        private static final long serialVersionUID = 1L;

        JsonException(String message) {
            super(message);
        }
    }

    /** A recursive-descent parser over the JSON grammar. */
    private static final class Parser {
        /**
         * Bounds recursion so a deeply nested document cannot overflow the stack. The engine's
         * own output nests only a handful of levels; this is far above that and far below the
         * default JVM stack depth.
         */
        private static final int MAX_DEPTH = 200;

        private final String src;
        private int pos;
        private int depth;

        Parser(String src) {
            this.src = src;
        }

        boolean atEnd() {
            return pos >= src.length();
        }

        void skipWhitespace() {
            while (pos < src.length()) {
                char c = src.charAt(pos);
                if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
                    pos++;
                } else {
                    break;
                }
            }
        }

        Object parseValue() {
            if (atEnd()) {
                throw new JsonException("unexpected end of input");
            }
            char c = src.charAt(pos);
            switch (c) {
                case '{':
                    return parseObject();
                case '[':
                    return parseArray();
                case '"':
                    return parseString();
                case 't':
                    expect("true");
                    return Boolean.TRUE;
                case 'f':
                    expect("false");
                    return Boolean.FALSE;
                case 'n':
                    expect("null");
                    return null;
                default:
                    return parseNumber();
            }
        }

        private void enter() {
            if (++depth > MAX_DEPTH) {
                throw new JsonException("JSON nesting exceeds " + MAX_DEPTH + " levels");
            }
        }

        private Map<String, Object> parseObject() {
            enter();
            pos++; // consume '{'
            Map<String, Object> map = new LinkedHashMap<>();
            skipWhitespace();
            if (peek() == '}') {
                pos++;
                depth--;
                return map;
            }
            while (true) {
                skipWhitespace();
                if (peek() != '"') {
                    throw new JsonException("expected a string key at position " + pos);
                }
                String key = parseString();
                skipWhitespace();
                if (peek() != ':') {
                    throw new JsonException("expected ':' after key at position " + pos);
                }
                pos++;
                skipWhitespace();
                map.put(key, parseValue());
                skipWhitespace();
                char c = peek();
                if (c == ',') {
                    pos++;
                    continue;
                }
                if (c == '}') {
                    pos++;
                    depth--;
                    return map;
                }
                throw new JsonException("expected ',' or '}' at position " + pos);
            }
        }

        private List<Object> parseArray() {
            enter();
            pos++; // consume '['
            List<Object> list = new ArrayList<>();
            skipWhitespace();
            if (peek() == ']') {
                pos++;
                depth--;
                return list;
            }
            while (true) {
                skipWhitespace();
                list.add(parseValue());
                skipWhitespace();
                char c = peek();
                if (c == ',') {
                    pos++;
                    continue;
                }
                if (c == ']') {
                    pos++;
                    depth--;
                    return list;
                }
                throw new JsonException("expected ',' or ']' at position " + pos);
            }
        }

        private String parseString() {
            pos++; // consume opening quote
            StringBuilder out = new StringBuilder();
            while (true) {
                if (atEnd()) {
                    throw new JsonException("unterminated string");
                }
                char c = src.charAt(pos++);
                if (c == '"') {
                    return out.toString();
                }
                if (c != '\\') {
                    out.append(c);
                    continue;
                }
                if (atEnd()) {
                    throw new JsonException("unterminated escape sequence");
                }
                char esc = src.charAt(pos++);
                switch (esc) {
                    case '"':
                        out.append('"');
                        break;
                    case '\\':
                        out.append('\\');
                        break;
                    case '/':
                        out.append('/');
                        break;
                    case 'b':
                        out.append('\b');
                        break;
                    case 'f':
                        out.append('\f');
                        break;
                    case 'n':
                        out.append('\n');
                        break;
                    case 'r':
                        out.append('\r');
                        break;
                    case 't':
                        out.append('\t');
                        break;
                    case 'u':
                        if (pos + 4 > src.length()) {
                            throw new JsonException("truncated \\u escape at position " + pos);
                        }
                        String hex = src.substring(pos, pos + 4);
                        try {
                            out.append((char) Integer.parseInt(hex, 16));
                        } catch (NumberFormatException e) {
                            throw new JsonException("invalid \\u escape '" + hex + "' at position " + pos);
                        }
                        pos += 4;
                        break;
                    default:
                        throw new JsonException("invalid escape '\\" + esc + "' at position " + (pos - 1));
                }
            }
        }

        private Object parseNumber() {
            int start = pos;
            if (peek() == '-') {
                pos++;
            }
            boolean floating = false;
            while (!atEnd()) {
                char c = src.charAt(pos);
                if (c >= '0' && c <= '9') {
                    pos++;
                } else if (c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-') {
                    floating = true;
                    pos++;
                } else {
                    break;
                }
            }
            String text = src.substring(start, pos);
            if (text.isEmpty() || text.equals("-")) {
                throw new JsonException("expected a value at position " + start);
            }
            try {
                if (floating) {
                    return Double.valueOf(text);
                }
                // Integers stay integral where they fit. A bound SQL argument that arrives as
                // 100 must not reach the caller's JDBC driver as 100.0, which some drivers bind
                // as a different column type.
                return Long.valueOf(text);
            } catch (NumberFormatException e) {
                try {
                    return Double.valueOf(text);
                } catch (NumberFormatException e2) {
                    throw new JsonException("invalid number '" + text + "' at position " + start);
                }
            }
        }

        private void expect(String literal) {
            if (!src.startsWith(literal, pos)) {
                throw new JsonException("invalid literal at position " + pos);
            }
            pos += literal.length();
        }

        private char peek() {
            if (atEnd()) {
                throw new JsonException("unexpected end of input");
            }
            return src.charAt(pos);
        }
    }
}
