package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;

/**
 * Tests of the SDK's own logic, against a scripted fake engine.
 *
 * <p>Everything here covers behaviour the real binary cannot be asked to produce offline: each
 * error code, a model call with no API key, a crash, corrupted output, a protocol mismatch, and
 * the laziness and caching of the fluent builder.
 */
class QueryForgeSdkTest {

    @TempDir
    Path tmp;

    @BeforeEach
    void requireShell() {
        assumeTrue(TestSupport.supportsShellFake(), "the scripted fake needs a POSIX shell");
    }

    @AfterEach
    void clearOverride() {
        TestSupport.clearBinary();
    }

    /** Installs a fake that answers with the given response, and returns it. */
    private TestSupport.FakeBinary install(Map<String, Object> response) {
        TestSupport.FakeBinary fake = TestSupport.fakeBinary(tmp, response);
        TestSupport.useBinary(fake.path());
        return fake;
    }

    /** Installs a fake that answers with raw text, an exit code and stderr. */
    private TestSupport.FakeBinary install(String body, int exitCode, String stderr, long sleepMillis) {
        TestSupport.FakeBinary fake = TestSupport.fakeBinary(tmp, body, exitCode, stderr, sleepMillis);
        TestSupport.useBinary(fake.path());
        return fake;
    }

    private QueryForge forge() {
        return QueryForge.postgres(TestSupport.config());
    }

    // ------------------------------------------------------- fluent surface

    @Test
    @DisplayName("describing a query spawns nothing until a terminal call")
    void queryIsLazy() {
        TestSupport.FakeBinary fake = install(TestSupport.okResponse("sql", "SELECT 1"));

        PendingQuery pending = forge().query("delivered orders");
        assertEquals(0, fake.callCount(), "describing a query must not spawn anything");

        pending.toSql();
        assertEquals(1, fake.callCount());
    }

    @Test
    @DisplayName("reading several facts off one query costs one model call")
    void resultIsCached() {
        TestSupport.FakeBinary fake = install(
                TestSupport.okResponse("sql", "SELECT 1", "explain", "prose", "args", Arrays.asList(7L)));

        PendingQuery pending = forge().query("delivered orders");
        pending.toSql();
        pending.explain();
        pending.toArgs();
        pending.warnings();

        assertEquals(1, fake.callCount());
    }

    @Test
    @DisplayName("builders return new objects so a base query is reusable")
    void buildersDoNotMutate() {
        TestSupport.FakeBinary fake = install(TestSupport.okResponse("sql", "SELECT 1"));

        PendingQuery base = forge().query("open orders");
        PendingQuery scoped = base.scope("tenantId", "t1");
        assertNotSame(base, scoped);

        base.toSql();
        scoped.toSql();

        List<Map<String, Object>> sent = fake.invocations();
        assertFalse(sent.get(0).containsKey("scope"), "the base query was mutated by scope()");
        assertEquals("t1", Values.map(sent.get(1).get("scope")).get("tenantId"));
    }

    @Test
    @DisplayName("scope merges rather than replaces")
    void scopeMerges() {
        TestSupport.FakeBinary fake = install(TestSupport.okResponse("sql", "SELECT 1"));

        forge().query("orders").scope("tenantId", "t1").scope("ownerId", "u9").toSql();

        Map<String, Object> scope = Values.map(fake.invocations().get(0).get("scope"));
        assertEquals("t1", scope.get("tenantId"));
        assertEquals("u9", scope.get("ownerId"));
    }

    @Test
    @DisplayName("per-query options reach the wire")
    void optionsReachTheWire() {
        TestSupport.FakeBinary fake = install(TestSupport.okResponse("sql", "SELECT 1"));

        forge().query("orders").timeout(2500).maxRepairs(0).includeRaw().scopeInAst().toSql();

        Map<String, Object> options = Values.map(fake.invocations().get(0).get("options"));
        assertEquals(2500L, ((Number) options.get("timeoutMs")).longValue());
        // 0 must survive as a value rather than vanishing as a default: it means "one attempt,
        // no repairs", which is a meaningfully different request.
        assertEquals(0L, ((Number) options.get("maxRepairs")).longValue());
        assertEquals(Boolean.TRUE, options.get("includeRaw"));
        assertEquals(Boolean.TRUE, options.get("scopeInAst"));
    }

    @Test
    @DisplayName("the request carries the op, backend and config")
    void requestShapeIsCorrect() {
        TestSupport.FakeBinary fake = install(TestSupport.okResponse("sql", "SELECT 1"));

        QueryForge.mysql(TestSupport.config()).query("orders").toSql();

        Map<String, Object> sent = fake.invocations().get(0);
        assertEquals("translate", sent.get("op"));
        assertEquals("mysql", sent.get("backend"));
        assertEquals("Order", Values.map(sent.get("config")).get("entity"));
    }

    @Test
    @DisplayName("a blank question is refused before anything is spawned")
    void blankQueryIsRefusedLocally() {
        TestSupport.FakeBinary fake = install(TestSupport.okResponse());
        for (String text : new String[] {"", "   ", "\n\t", null}) {
            assertThrows(InvalidRequestException.class, () -> forge().query(text));
        }
        assertEquals(0, fake.callCount(), "a blank question must not cost a process spawn");
    }

    @Test
    @DisplayName("non-positive timeouts and negative repair budgets are refused")
    void invalidOptionsAreRefused() {
        assertThrows(InvalidRequestException.class, () -> forge().query("orders").timeout(0));
        assertThrows(InvalidRequestException.class, () -> forge().query("orders").timeout(-1));
        assertThrows(InvalidRequestException.class, () -> forge().query("orders").maxRepairs(-1));
        assertThrows(InvalidRequestException.class, () -> forge().withTimeout(0));
    }

    // -------------------------------------------------------- error mapping

    @ParameterizedTest(name = "{0} maps to {1}")
    @CsvSource({
        "INVALID_REQUEST,     io.queryforge.InvalidRequestException",
        "UNKNOWN_OP,          io.queryforge.InvalidRequestException",
        "INVALID_CONFIG,      io.queryforge.InvalidConfigException",
        "UNKNOWN_BACKEND,     io.queryforge.UnknownBackendException",
        "INVALID_SCOPE,       io.queryforge.InvalidScopeException",
        "VALIDATION_FAILED,   io.queryforge.ValidationException",
        "UNSUPPORTED_REQUEST, io.queryforge.UnsupportedRequestException",
        "MODEL_OUTPUT,        io.queryforge.ModelOutputException",
        "MODEL_TRANSPORT,     io.queryforge.ModelTransportException",
        "GENERATE_FAILED,     io.queryforge.GenerateException",
        "TIMEOUT,             io.queryforge.TimeoutException",
        "INTERNAL,            io.queryforge.QueryForgeException",
    })
    void everyProtocolCodeMapsToAnException(String code, String expectedClass) throws Exception {
        install(TestSupport.errResponse(code, "something went wrong"));

        QueryForgeException e =
                assertThrows(QueryForgeException.class, () -> forge().query("orders").toSql());
        assertEquals(Class.forName(expectedClass), e.getClass());
        assertEquals(code, e.getCode());
        assertTrue(e.getMessage().contains("something went wrong"), e.getMessage());
    }

    @Test
    @DisplayName("an unrecognised code still raises the base class with the code intact")
    void unknownCodeDegradesGracefully() {
        // An SDK talking to a newer engine must degrade, not crash: a code this version predates
        // has to surface rather than disappear.
        install(TestSupport.errResponse("SOME_FUTURE_CODE", "from a newer engine"));

        QueryForgeException e =
                assertThrows(QueryForgeException.class, () -> forge().query("orders").toSql());
        assertEquals(QueryForgeException.class, e.getClass());
        assertEquals("SOME_FUTURE_CODE", e.getCode());
    }

    @Test
    @DisplayName("validation details survive the trip")
    void validationDetailsArePreserved() {
        Map<String, Object> detail = new LinkedHashMap<>();
        detail.put("code", "unknown_field");
        detail.put("path", "filter");
        detail.put("field", "agee");
        detail.put("message", "filter: unknown field \"agee\"");
        detail.put("suggestions", Arrays.asList("age", "amount"));

        install(TestSupport.errResponse(
                "VALIDATION_FAILED", "filter: unknown field", "details", Arrays.asList(detail)));

        ValidationException e =
                assertThrows(ValidationException.class, () -> forge().query("orders").toSql());
        Detail first = e.getDetails().get(0);
        assertEquals("unknown_field", first.getCode());
        assertEquals("agee", first.getField());
        assertEquals(Arrays.asList("age", "amount"), first.getSuggestions());
    }

    @Test
    @DisplayName("an error with no message still reads")
    void errorWithoutMessageStillReads() {
        Map<String, Object> response = new LinkedHashMap<>();
        response.put("success", Boolean.FALSE);
        response.put("protocol", "1.0");
        response.put("code", "INTERNAL");
        install(response);

        QueryForgeException e =
                assertThrows(QueryForgeException.class, () -> forge().query("orders").toSql());
        assertFalse(e.getMessage().trim().isEmpty());
    }

    // ------------------------------------------------- a misbehaving engine

    @Test
    @DisplayName("a crash with no output carries stderr through")
    void crashCarriesStderr() {
        install(null, 3, "segmentation fault", 0);

        ProtocolException e =
                assertThrows(ProtocolException.class, () -> forge().query("orders").toSql());
        // stderr is the only evidence of what happened; replacing it with a bare exit code would
        // leave the user nothing to act on.
        assertTrue(e.getMessage().contains("segmentation fault"), e.getMessage());
        assertTrue(e.getMessage().contains("3"), e.getMessage());
    }

    @Test
    @DisplayName("non-JSON output is a protocol error")
    void nonJsonOutputIsAProtocolError() {
        install("panic: runtime error: index out of range", 2, "", 0);

        ProtocolException e =
                assertThrows(ProtocolException.class, () -> forge().query("orders").toSql());
        assertTrue(e.getMessage().contains("not JSON"), e.getMessage());
    }

    @Test
    @DisplayName("a huge garbage stream is truncated in the message")
    void garbageIsTruncated() {
        StringBuilder garbage = new StringBuilder();
        for (int i = 0; i < 10_000; i++) {
            garbage.append("xxxxxxxxxx");
        }
        install(garbage.toString(), 0, "", 0);

        ProtocolException e =
                assertThrows(ProtocolException.class, () -> forge().query("orders").toSql());
        // The message is going into someone's log.
        assertTrue(e.getMessage().length() < 2000, "message length " + e.getMessage().length());
    }

    @Test
    @DisplayName("a major protocol mismatch is refused")
    void majorProtocolMismatchIsRefused() {
        Map<String, Object> response = new LinkedHashMap<>();
        response.put("success", Boolean.TRUE);
        response.put("protocol", "2.0");
        response.put("sql", "SELECT 1");
        install(response);

        ProtocolException e =
                assertThrows(ProtocolException.class, () -> forge().query("orders").toSql());
        assertTrue(e.getMessage().contains("2.0"), e.getMessage());
    }

    @Test
    @DisplayName("a minor protocol bump is accepted")
    void minorProtocolBumpIsAccepted() {
        // Additive changes must not break an older SDK — that is the whole point of versioning
        // the protocol rather than the binary alone.
        Map<String, Object> response = new LinkedHashMap<>();
        response.put("success", Boolean.TRUE);
        response.put("protocol", "1.7");
        response.put("backend", "sql");
        response.put("sql", "SELECT 1");
        install(response);

        assertEquals("SELECT 1", forge().query("orders").toSql());
    }

    @Test
    @DisplayName("a response with no protocol version is refused")
    void missingProtocolIsRefused() {
        Map<String, Object> response = new LinkedHashMap<>();
        response.put("success", Boolean.TRUE);
        response.put("sql", "SELECT 1");
        install(response);

        ProtocolException e =
                assertThrows(ProtocolException.class, () -> forge().query("orders").toSql());
        assertTrue(e.getMessage().contains("protocol"), e.getMessage());
    }

    @Test
    @DisplayName("unknown response fields are ignored")
    void unknownResponseFieldsAreIgnored() {
        // The mirror of the mismatch test: a newer engine adding an optional field must not break
        // this SDK.
        install(TestSupport.okResponse(
                "sql", "SELECT 1", "somethingNew", Collections.singletonMap("added", "later")));
        assertEquals("SELECT 1", forge().query("orders").toSql());
    }

    @Test
    @DisplayName("a hung engine is killed and reported")
    void hungEngineIsKilled() {
        // The engine enforces its own deadline; this outer bound only catches an engine that has
        // hung badly enough not to honour it.
        install(Json.write(TestSupport.okResponse("sql", "SELECT 1")), 0, "", 30_000);

        ProtocolException e = assertThrows(
                ProtocolException.class, () -> forge().query("orders").timeout(50).toSql());
        assertTrue(e.getMessage().contains("killed"), e.getMessage());
    }

    @Test
    @DisplayName("a large request does not deadlock on a full pipe")
    void largeRequestDoesNotDeadlock() {
        // A subprocess that fills its stdout pipe blocks until someone reads it. If the SDK were
        // still writing the request at that moment, both sides would wait forever — which is why
        // the streams are drained on their own threads.
        install(TestSupport.okResponse("sql", "SELECT 1"));

        StringBuilder bigValue = new StringBuilder();
        for (int i = 0; i < 200_000; i++) {
            bigValue.append("padding");
        }
        assertEquals(
                "SELECT 1",
                forge().query("orders").scope("customerName", bigValue.toString()).toSql());
    }

    // ---------------------------------------------------- binary resolution

    @Test
    @DisplayName("a missing override is reported, never silently ignored")
    void missingOverrideIsReported() {
        // Falling back to the bundled binary would run a *different* engine than the one the user
        // named, which is precisely what the override exists to prevent.
        System.setProperty(BinaryResolver.BINARY_PROPERTY, tmp.resolve("nope").toString());
        BinaryResolver.resetCache();

        BinaryNotFoundException e = assertThrows(
                BinaryNotFoundException.class, () -> forge().query("orders").toSql());
        assertTrue(e.getMessage().contains("nope"), e.getMessage());
    }

    @Test
    @DisplayName("a non-executable override says how to fix it")
    void nonExecutableOverrideExplainsTheFix() throws Exception {
        Path path = tmp.resolve("not-executable");
        Files.write(path, "#!/bin/sh\ntrue\n".getBytes("UTF-8"));
        assertTrue(path.toFile().setExecutable(false, false));

        System.setProperty(BinaryResolver.BINARY_PROPERTY, path.toString());
        BinaryResolver.resetCache();

        BinaryNotFoundException e = assertThrows(
                BinaryNotFoundException.class, () -> forge().query("orders").toSql());
        assertTrue(e.getMessage().contains("chmod"), e.getMessage());
    }

    // -------------------------------------------------------- result decoding

    @Test
    @DisplayName("a result exposes every reported field")
    void resultExposesEveryField() {
        Map<String, Object> scopeEntry = new LinkedHashMap<>();
        scopeEntry.put("field", "tenantId");
        scopeEntry.put("operator", "equals");
        scopeEntry.put("value", Collections.singletonMap("v", "t1"));
        scopeEntry.put("declared", Boolean.FALSE);

        install(TestSupport.okResponse(
                "sql", "SELECT 1",
                "args", Arrays.asList(1L, "two", Boolean.TRUE, null),
                "explain", "prose",
                "warnings", Arrays.asList("slow"),
                "ast", Collections.singletonMap("entity", "Order"),
                "providerUsed", "groq/llama",
                "repairAttempts", 2,
                "raw", "{...}",
                "scope", Arrays.asList(scopeEntry)));

        QueryForgeResult result = forge().query("orders").includeRaw().result();

        assertEquals("SELECT 1", result.getSql());
        assertEquals(Arrays.asList(1L, "two", Boolean.TRUE, null), result.getArgs());
        assertEquals("prose", result.getExplain());
        assertEquals(Arrays.asList("slow"), result.getWarnings());
        assertEquals("Order", result.getAst().get("entity"));
        assertEquals("groq/llama", result.getProviderUsed());
        assertEquals(2, result.getRepairAttempts());
        assertEquals("{...}", result.getRaw());
        assertEquals("t1", result.getScope().get(0).getValue());
        assertFalse(result.getScope().get(0).isDeclared());
    }

    @Test
    @DisplayName("a sparse response decodes to neutral defaults")
    void sparseResponseDecodes() {
        // Every field but success and protocol is omitted when empty, so the decoder must not
        // assume any of them are present.
        Map<String, Object> response = new LinkedHashMap<>();
        response.put("success", Boolean.TRUE);
        response.put("protocol", "1.0");
        install(response);

        QueryForgeResult result = forge().query("orders").result();
        assertTrue(result.getArgs().isEmpty());
        assertTrue(result.getWarnings().isEmpty());
        assertTrue(result.getScope().isEmpty());
        assertTrue(result.getAst().isEmpty());
        assertEquals(0, result.getRepairAttempts());
        assertFalse(result.isSql());
    }

    @Test
    @DisplayName("a response field of the wrong type degrades instead of throwing")
    void wronglyTypedFieldsDegrade() {
        // An engine that reshapes an optional field must not turn into a ClassCastException
        // several frames from the cause.
        install(TestSupport.okResponse(
                "sql", "SELECT 1", "warnings", "not a list", "repairAttempts", "not a number"));

        QueryForgeResult result = forge().query("orders").result();
        assertTrue(result.getWarnings().isEmpty());
        assertEquals(0, result.getRepairAttempts());
    }

    @Test
    @DisplayName("result collections are unmodifiable")
    void resultCollectionsAreImmutable() {
        // A result describes something that already happened; mutating it could only make an
        // audit record disagree with what ran.
        install(TestSupport.okResponse("sql", "SELECT 1", "args", Arrays.asList(1L)));
        QueryForgeResult result = forge().query("orders").result();
        assertThrows(UnsupportedOperationException.class, () -> result.getArgs().add("x"));
        assertThrows(UnsupportedOperationException.class, () -> result.getWarnings().add("x"));
    }
}
