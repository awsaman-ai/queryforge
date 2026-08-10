package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Tests for the hand-rolled JSON codec.
 *
 * <p>This gets its own thorough suite because it is the one piece of the SDK that is not a thin
 * pass-through: everything else forwards bytes, but a bug here silently corrupts a query. The
 * cases below are the ones that actually bite — escaping, Unicode, integer/double distinction,
 * empty containers, and malformed input.
 */
class JsonTest {

    // ------------------------------------------------------------- writing

    @Test
    @DisplayName("scalars serialize to their JSON forms")
    void writesScalars() {
        assertEquals("null", Json.write(null));
        assertEquals("true", Json.write(Boolean.TRUE));
        assertEquals("false", Json.write(Boolean.FALSE));
        assertEquals("42", Json.write(42));
        assertEquals("42", Json.write(42L));
        assertEquals("\"hello\"", Json.write("hello"));
    }

    @Test
    @DisplayName("whole doubles lose Java's trailing .0")
    void writesWholeDoublesWithoutDecimalPoint() {
        // A bound argument of 100.0 arriving as "100.0" is not wrong JSON, but it reads badly in
        // a logged request and differs pointlessly from what the other SDKs send.
        assertEquals("100", Json.write(100.0d));
        assertEquals("10.5", Json.write(10.5d));
        assertEquals("-3", Json.write(-3.0d));
    }

    @Test
    @DisplayName("NaN and Infinity are refused rather than emitted")
    void refusesNonFiniteDoubles() {
        // JSON has no way to spell these; emitting the Java literal would produce a document the
        // engine cannot parse, several layers from the cause.
        assertThrows(IllegalArgumentException.class, () -> Json.write(Double.NaN));
        assertThrows(IllegalArgumentException.class, () -> Json.write(Double.POSITIVE_INFINITY));
        assertThrows(IllegalArgumentException.class, () -> Json.write(Double.NEGATIVE_INFINITY));
    }

    @Test
    @DisplayName("strings escape the characters JSON requires")
    void escapesStrings() {
        assertEquals("\"a\\\"b\"", Json.write("a\"b"));
        assertEquals("\"a\\\\b\"", Json.write("a\\b"));
        assertEquals("\"a\\nb\"", Json.write("a\nb"));
        assertEquals("\"a\\tb\"", Json.write("a\tb"));
        assertEquals("\"a\\rb\"", Json.write("a\rb"));
        assertEquals("\"\\u0000\"", Json.write("\u0000"));
        assertEquals("\"\\u001f\"", Json.write("\u001f"));
    }

    @Test
    @DisplayName("non-ASCII travels as UTF-8 rather than \\u escapes")
    void doesNotEscapeNonAscii() {
        // The engine reads UTF-8. Escaping would only make payloads larger and logs unreadable,
        // and a user's question is very often not ASCII.
        assertEquals("\"café\"", Json.write("café"));
        assertEquals("\"日本語\"", Json.write("日本語"));
        assertEquals("\"🚀\"", Json.write("🚀"));
    }

    @Test
    @DisplayName("maps and lists nest correctly")
    void writesContainers() {
        Map<String, Object> map = new LinkedHashMap<>();
        map.put("a", 1);
        map.put("b", Arrays.asList(1, "two", true, null));
        assertEquals("{\"a\":1,\"b\":[1,\"two\",true,null]}", Json.write(map));
    }

    @Test
    @DisplayName("empty containers survive the round trip")
    void writesEmptyContainers() {
        assertEquals("{}", Json.write(new LinkedHashMap<String, Object>()));
        assertEquals("[]", Json.write(new ArrayList<>()));
    }

    @Test
    @DisplayName("object key order is preserved")
    void preservesKeyOrder() {
        Map<String, Object> map = new LinkedHashMap<>();
        map.put("z", 1);
        map.put("a", 2);
        map.put("m", 3);
        // Stable output is what makes a logged request diffable and a golden test possible.
        assertEquals("{\"z\":1,\"a\":2,\"m\":3}", Json.write(map));
    }

    @Test
    @DisplayName("an unserializable value names its own type")
    void refusesUnknownTypes() {
        // A silent toString() would send the engine an object nobody meant to send, and it would
        // arrive looking like a legitimate string value.
        class Session {}
        IllegalArgumentException e =
                assertThrows(IllegalArgumentException.class, () -> Json.write(new Session()));
        assertTrue(e.getMessage().contains("Session"), e.getMessage());
    }

    // ------------------------------------------------------------- reading

    @Test
    @DisplayName("scalars parse to the right Java types")
    void readsScalars() {
        assertNull(Json.read("null"));
        assertEquals(Boolean.TRUE, Json.read("true"));
        assertEquals(Boolean.FALSE, Json.read("false"));
        assertEquals("hi", Json.read("\"hi\""));
    }

    @Test
    @DisplayName("integers stay integral and only real decimals become doubles")
    void distinguishesIntegersFromDoubles() {
        // A bound SQL argument that arrives as 100 must not reach a JDBC driver as 100.0; some
        // drivers bind that as a different column type.
        assertInstanceOf(Long.class, Json.read("100"));
        assertEquals(100L, Json.read("100"));
        assertInstanceOf(Double.class, Json.read("100.5"));
        assertEquals(100.5d, Json.read("100.5"));
        assertInstanceOf(Double.class, Json.read("1e3"));
        assertEquals(-7L, Json.read("-7"));
    }

    @Test
    @DisplayName("an integer too large for long degrades to double rather than failing")
    void handlesOversizedIntegers() {
        Object value = Json.read("99999999999999999999999");
        assertInstanceOf(Double.class, value);
    }

    @Test
    @DisplayName("escapes are decoded")
    void readsEscapes() {
        assertEquals("a\"b", Json.read("\"a\\\"b\""));
        assertEquals("a\\b", Json.read("\"a\\\\b\""));
        assertEquals("a\nb", Json.read("\"a\\nb\""));
        assertEquals("a/b", Json.read("\"a\\/b\""));
        assertEquals("\u00e9", Json.read("\"\\u00e9\""));
    }

    @Test
    @DisplayName("nested containers parse")
    void readsContainers() {
        Map<String, Object> map = Json.readObject("{\"a\":[1,{\"b\":true}],\"c\":null}");
        assertEquals(2, map.size());
        List<?> list = (List<?>) map.get("a");
        assertEquals(1L, list.get(0));
        assertEquals(Boolean.TRUE, ((Map<?, ?>) list.get(1)).get("b"));
        assertTrue(map.containsKey("c"));
        assertNull(map.get("c"));
    }

    @Test
    @DisplayName("whitespace between tokens is ignored")
    void toleratesWhitespace() {
        Map<String, Object> map = Json.readObject("  {\n \"a\" :\t1 ,\r\n \"b\" : [ ]\n}  ");
        assertEquals(1L, map.get("a"));
        assertEquals(0, ((List<?>) map.get("b")).size());
    }

    @Test
    @DisplayName("a value round-trips through write and read")
    void roundTrips() {
        Map<String, Object> original = new LinkedHashMap<>();
        original.put("text", "a \"quoted\" line\nwith a tab\there and café");
        original.put("count", 42L);
        original.put("ratio", 0.5d);
        original.put("flag", Boolean.TRUE);
        original.put("list", Arrays.asList("a", 1L, null));
        original.put("nested", new LinkedHashMap<>(original));

        assertEquals(original, Json.readObject(Json.write(original)));
    }

    @Test
    @DisplayName("malformed input is refused, not half-parsed")
    void rejectsMalformedInput() {
        String[] bad = {
            "",
            "{",
            "}",
            "{\"a\"}",
            "{\"a\":}",
            "{\"a\":1,}",
            "[1,2",
            "\"unterminated",
            "{'a':1}", // single quotes are not JSON
            "{\"a\":1}trailing",
            "tru",
            "\"bad\\qescape\"",
            "\"\\u00\"",
        };
        for (String input : bad) {
            assertThrows(
                    Json.JsonException.class,
                    () -> Json.read(input),
                    "expected '" + input + "' to be rejected");
        }
    }

    @Test
    @DisplayName("readObject refuses a non-object document")
    void readObjectRequiresAnObject() {
        assertThrows(Json.JsonException.class, () -> Json.readObject("[1,2,3]"));
        assertThrows(Json.JsonException.class, () -> Json.readObject("\"a string\""));
        assertThrows(Json.JsonException.class, () -> Json.readObject("null"));
    }

    @Test
    @DisplayName("deep nesting is bounded rather than overflowing the stack")
    void boundsNestingDepth() {
        StringBuilder deep = new StringBuilder();
        for (int i = 0; i < 5000; i++) {
            deep.append('[');
        }
        // A StackOverflowError is an Error, not an Exception, and would escape any reasonable
        // catch a caller writes around a query.
        assertThrows(Json.JsonException.class, () -> Json.read(deep.toString()));
    }
}
