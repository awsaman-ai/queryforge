package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import java.io.ByteArrayOutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import java.util.stream.Collectors;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * Structured logging and fail-fast behaviour in the Java SDK.
 *
 * <p>Three properties are under test, and they are the three the whole change exists for:
 *
 * <ol>
 *   <li><strong>Nothing fails silently.</strong> Every failure path throws, carries a code, and
 *       preserves the original cause.
 *   <li><strong>Failures are logged once, at the boundary, with usable fields.</strong>
 *   <li><strong>Secrets and user data never reach a log</strong>, even at the most verbose level.
 * </ol>
 *
 * <p>Assertions are on the structured FIELDS, never on a rendered line and never on a timestamp. A
 * test that string-matches log output fails the moment someone reorders two attributes, which is
 * how a team learns to distrust logging tests.
 */
class QueryForgeLoggingTest {

    /** Captures records with their field maps intact. */
    static final class Recorder extends Handler {
        private final List<LogRecord> records = Collections.synchronizedList(new ArrayList<>());

        @Override
        public void publish(LogRecord record) {
            records.add(record);
        }

        @Override
        public void flush() {}

        @Override
        public void close() {}

        List<LogRecord> all() {
            return new ArrayList<>(records);
        }

        List<LogRecord> atLeast(Level level) {
            return all().stream()
                    .filter(r -> r.getLevel().intValue() >= level.intValue())
                    .collect(Collectors.toList());
        }

        Map<String, Object> fieldsOf(int index) {
            return QueryForgeLogging.fieldsOf(all().get(index));
        }

        /** Everything logged, rendered — for the privacy canaries, which must search it all. */
        String text() {
            StringBuilder sb = new StringBuilder();
            for (LogRecord r : all()) {
                sb.append(r.getMessage()).append('\n');
                sb.append(QueryForgeLogging.fieldsOf(r)).append('\n');
                if (r.getThrown() != null) {
                    java.io.StringWriter w = new java.io.StringWriter();
                    r.getThrown().printStackTrace(new java.io.PrintWriter(w));
                    sb.append(w).append('\n');
                }
            }
            return sb.toString();
        }
    }

    private Logger root;
    private Level originalLevel;
    private boolean originalUseParents;
    private Recorder recorder;

    @BeforeEach
    void attachRecorder() {
        root = Logger.getLogger(QueryForgeLogging.LOGGER_NAME);
        originalLevel = root.getLevel();
        originalUseParents = root.getUseParentHandlers();
        recorder = new Recorder();
        recorder.setLevel(Level.ALL);
        root.addHandler(recorder);
        // FINE deliberately: the privacy canaries have to prove a secret does not leak even at
        // the most verbose setting the SDK offers.
        root.setLevel(Level.FINE);
        root.setUseParentHandlers(false); // keep the surefire console clean
    }

    @AfterEach
    void detachRecorder() {
        root.removeHandler(recorder);
        root.setLevel(originalLevel);
        root.setUseParentHandlers(originalUseParents);
        TestSupport.clearBinary();
    }

    // ------------------------------------------------------------------ levels

    @Test
    @DisplayName("parseLevel accepts the documented names")
    void parseLevelAcceptsTheDocumentedNames() {
        assertEquals(Level.FINE, QueryForgeLogging.parseLevel("debug"));
        assertEquals(Level.INFO, QueryForgeLogging.parseLevel("INFO"));
        assertEquals(Level.WARNING, QueryForgeLogging.parseLevel("  warn "));
        assertEquals(Level.WARNING, QueryForgeLogging.parseLevel("warning"));
        assertEquals(Level.SEVERE, QueryForgeLogging.parseLevel("error"));
        assertEquals(Level.OFF, QueryForgeLogging.parseLevel("off"));
    }

    @Test
    @DisplayName("parseLevel rejects a typo rather than defaulting")
    void parseLevelRejectsATypo() {
        // Both plausible defaults are harmful: "off" hides diagnostics from someone who asked for
        // them, "debug" starts writing detail nobody authorised.
        for (String bad : Arrays.asList("trace", "verbose", "debgu", "1", "")) {
            assertThrows(IllegalArgumentException.class, () -> QueryForgeLogging.parseLevel(bad));
        }
    }

    @Test
    @DisplayName("engine level names round towards more detail, never less")
    void engineLevelNamesRoundTowardsDetail() {
        assertEquals("debug", QueryForgeLogging.engineLevelName(Level.FINEST));
        assertEquals("debug", QueryForgeLogging.engineLevelName(Level.FINE));
        assertEquals("info", QueryForgeLogging.engineLevelName(Level.CONFIG));
        assertEquals("info", QueryForgeLogging.engineLevelName(Level.INFO));
        assertEquals("warn", QueryForgeLogging.engineLevelName(Level.WARNING));
        assertEquals("error", QueryForgeLogging.engineLevelName(Level.SEVERE));
        assertEquals("off", QueryForgeLogging.engineLevelName(Level.OFF));
    }

    // ---------------------------------------------- the wire: what we ask for

    @Test
    @DisplayName("a default request carries no logging fields")
    void aDefaultRequestCarriesNoLoggingFields(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        // Undo this test class's own configuration: the point is what an UNCONFIGURED SDK sends.
        root.removeHandler(recorder);
        root.setLevel(Level.WARNING);

        TestSupport.FakeBinary fake =
                TestSupport.fakeBinary(dir, TestSupport.okResponse("sql", "SELECT 1"));
        TestSupport.useBinary(fake.path());

        QueryForge.postgres(TestSupport.config()).generate(TestSupport.validAst());

        Map<String, Object> options = Values.map(fake.invocations().get(0).get("options"));
        assertFalse(options.containsKey("logLevel"),
                "the default request must stay byte-identical to a protocol-1.0 one — the engine "
                        + "rejects unknown fields, so this would break an older binary");
        assertFalse(options.containsKey("requestId"));
    }

    @Test
    @DisplayName("configured logging asks the engine for the same level")
    void configuredLoggingAsksTheEngineForTheSameLevel(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        TestSupport.FakeBinary fake =
                TestSupport.fakeBinary(dir, TestSupport.okResponse("sql", "SELECT 1"));
        TestSupport.useBinary(fake.path());

        QueryForge.postgres(TestSupport.config()).generate(TestSupport.validAst());

        Map<String, Object> options = Values.map(fake.invocations().get(0).get("options"));
        assertEquals("debug", options.get("logLevel"));
        assertNotNull(options.get("requestId"), "an id is generated so the two halves correlate");
    }

    @Test
    @DisplayName("a caller-supplied request id reaches both the log and the engine")
    void aCallerSuppliedRequestIdReachesBothHalves(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        TestSupport.FakeBinary fake =
                TestSupport.fakeBinary(dir, TestSupport.okResponse("sql", "SELECT 1"));
        TestSupport.useBinary(fake.path());

        QueryForge.postgres(TestSupport.config())
                .query("delivered orders")
                .requestId("trace-abc-123")
                .toSql();

        Map<String, Object> options = Values.map(fake.invocations().get(0).get("options"));
        assertEquals("trace-abc-123", options.get("requestId"));

        List<LogRecord> perCall = recorder.all().stream()
                .filter(r -> (QueryForgeLogging.LOGGER_NAME + ".transport").equals(r.getLoggerName()))
                .collect(Collectors.toList());
        assertFalse(perCall.isEmpty(), "the transport should have logged something");
        for (LogRecord record : perCall) {
            assertEquals("trace-abc-123",
                    QueryForgeLogging.fieldsOf(record).get(QueryForgeLogging.FIELD_REQUEST_ID));
        }
    }

    @Test
    @DisplayName("requestId rejects an empty value")
    void requestIdRejectsAnEmptyValue() {
        assertThrows(
                InvalidRequestException.class,
                () -> QueryForge.postgres(TestSupport.config()).query("x").requestId("   "));
    }

    // --------------------------------------------------------- fields, levels

    @Test
    @DisplayName("a successful call logs one INFO record with the canonical fields")
    void aSuccessfulCallLogsOneInfoRecord(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        TestSupport.useBinary(TestSupport.fakeBinary(dir, TestSupport.okResponse(
                        "sql", "SELECT 1", "repairAttempts", 2, "providerUsed", "gemini"))
                .path());

        QueryForge.postgres(TestSupport.config()).query("delivered orders").toSql();

        List<LogRecord> completed = recorder.all().stream()
                .filter(r -> r.getMessage().startsWith("engine request completed"))
                .collect(Collectors.toList());
        assertEquals(1, completed.size(), "exactly one outcome line per call");
        assertEquals(Level.INFO, completed.get(0).getLevel());

        Map<String, Object> fields = QueryForgeLogging.fieldsOf(completed.get(0));
        assertEquals("queryforge", fields.get(QueryForgeLogging.FIELD_LIBRARY));
        assertEquals("java", fields.get(QueryForgeLogging.FIELD_LANGUAGE));
        assertEquals("translate", fields.get(QueryForgeLogging.FIELD_OPERATION));
        assertEquals("sql", fields.get(QueryForgeLogging.FIELD_BACKEND));
        assertEquals("Order", fields.get(QueryForgeLogging.FIELD_ENTITY));
        assertEquals("ok", fields.get(QueryForgeLogging.FIELD_OUTCOME));
        assertNotNull(fields.get(QueryForgeLogging.FIELD_DURATION_MS));
        assertNotNull(fields.get(QueryForgeLogging.FIELD_REQUEST_ID));
        assertNotNull(fields.get(QueryForgeLogging.FIELD_VERSION));
        // A translation that needed repairs cost extra model calls; that is worth surfacing
        // because it is almost always a closable config gap.
        assertEquals(2L, ((Number) fields.get("repair_attempts")).longValue());
        assertEquals("gemini", fields.get("provider"));
    }

    @Test
    @DisplayName("every engine failure is thrown and logged exactly once")
    void everyEngineFailureIsThrownAndLoggedOnce(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());

        Map<String, Class<? extends QueryForgeException>> expected = new LinkedHashMap<>();
        expected.put("MODEL_TRANSPORT", ModelTransportException.class);
        expected.put("VALIDATION_FAILED", ValidationException.class);
        expected.put("UNSUPPORTED_REQUEST", UnsupportedRequestException.class);
        expected.put("INVALID_SCOPE", InvalidScopeException.class);
        expected.put("GENERATE_FAILED", GenerateException.class);
        expected.put("TIMEOUT", TimeoutException.class);

        int index = 0;
        for (Map.Entry<String, Class<? extends QueryForgeException>> entry : expected.entrySet()) {
            Path caseDir = dir.resolve("case" + index++);
            TestSupport.useBinary(
                    TestSupport.fakeBinary(caseDir, TestSupport.errResponse(entry.getKey(), "no"))
                            .path());
            recorder.all().clear();
            Recorder scoped = new Recorder();
            scoped.setLevel(Level.ALL);
            root.addHandler(scoped);
            try {
                QueryForgeException thrown = assertThrows(
                        entry.getValue(),
                        () -> QueryForge.postgres(TestSupport.config()).query("x").toSql());
                assertEquals(entry.getKey(), thrown.getCode());

                List<LogRecord> severe = scoped.atLeast(Level.SEVERE);
                assertEquals(1, severe.size(),
                        "expected exactly 1 SEVERE record for " + entry.getKey()
                                + ", got " + severe.size());
                Map<String, Object> fields = QueryForgeLogging.fieldsOf(severe.get(0));
                assertEquals(entry.getKey(), fields.get(QueryForgeLogging.FIELD_ERROR_CODE));
                assertEquals(entry.getValue().getSimpleName(),
                        fields.get(QueryForgeLogging.FIELD_ERROR_TYPE));
                assertEquals("error", fields.get(QueryForgeLogging.FIELD_OUTCOME));
                // The stack trace belongs in the log exactly once, and this is the once.
                assertNotNull(severe.get(0).getThrown());
            } finally {
                root.removeHandler(scoped);
            }
        }
    }

    @Test
    @DisplayName("an unknown error code degrades to the base class and is still logged")
    void anUnknownCodeDegradesAndIsStillLogged(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        TestSupport.useBinary(
                TestSupport.fakeBinary(dir, TestSupport.errResponse("SOMETHING_NEW_IN_2027", "hi"))
                        .path());

        QueryForgeException thrown = assertThrows(
                QueryForgeException.class,
                () -> QueryForge.postgres(TestSupport.config()).query("x").toSql());
        assertEquals("SOMETHING_NEW_IN_2027", thrown.getCode());
        assertEquals(1, recorder.atLeast(Level.SEVERE).size());
    }

    // ------------------------------------------------------- cause preservation

    @Test
    @DisplayName("an unreadable config file preserves the IOException as the cause")
    void anUnreadableConfigFilePreservesTheCause(@TempDir Path dir) {
        // The defect this fixes: InvalidConfigException used to be built with the message only,
        // so "could not read the config" arrived with no indication of whether the path was
        // wrong, the permissions were wrong, or the disk was full.
        Path missing = dir.resolve("no-such-config.json");

        InvalidConfigException thrown =
                assertThrows(InvalidConfigException.class, () -> QueryForge.mysql(missing));

        assertEquals("INVALID_CONFIG", thrown.getCode());
        assertNotNull(thrown.getCause(), "the root cause must survive the wrapping");
        assertTrue(thrown.getCause() instanceof java.io.IOException,
                "expected an IOException, got " + thrown.getCause().getClass());
    }

    @Test
    @DisplayName("malformed config JSON preserves the parser's own complaint as the cause")
    void malformedConfigJsonPreservesTheCause(@TempDir Path dir) throws Exception {
        Path path = dir.resolve("broken.json");
        java.nio.file.Files.write(path, "{\"entity\": ".getBytes(StandardCharsets.UTF_8));

        InvalidConfigException thrown =
                assertThrows(InvalidConfigException.class, () -> QueryForge.mysql(path));

        assertNotNull(thrown.getCause());
        assertTrue(thrown.getCause() instanceof Json.JsonException);
    }

    @Test
    @DisplayName("a malformed config string preserves the parser's complaint as the cause")
    void aMalformedConfigStringPreservesTheCause() {
        InvalidConfigException thrown =
                assertThrows(InvalidConfigException.class, () -> QueryForge.parseConfig("{nope"));
        assertNotNull(thrown.getCause());
    }

    @Test
    @DisplayName("a crashed engine throws rather than returning nothing")
    void aCrashedEngineThrows(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        // The worst silent-failure risk in the whole SDK: stdout is empty, so a naive
        // implementation returns an empty result and the caller builds a query from nothing.
        TestSupport.useBinary(
                TestSupport.fakeBinary(dir, null, 3, "segmentation fault", 0).path());

        ProtocolException thrown = assertThrows(
                ProtocolException.class,
                () -> QueryForge.postgres(TestSupport.config()).query("x").toSql());

        assertTrue(thrown.getMessage().contains("produced no response"));
        assertTrue(thrown.getMessage().contains("segmentation fault"),
                "stderr is the only evidence there is; it must be carried through");
        assertEquals(1, recorder.atLeast(Level.SEVERE).size());
    }

    @Test
    @DisplayName("a non-JSON reply preserves the parse failure as the cause")
    void aNonJsonReplyPreservesTheCause(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        TestSupport.useBinary(TestSupport.fakeBinary(dir, "this is not JSON", 0, "", 0).path());

        ProtocolException thrown = assertThrows(
                ProtocolException.class,
                () -> QueryForge.postgres(TestSupport.config()).query("x").toSql());
        assertNotNull(thrown.getCause(), "losing the cause turns a specific fault into 'something went wrong'");
    }

    // -------------------------------------------------- redaction and canaries

    @Test
    @DisplayName("redact scrubs every credential shape the engine could print")
    void redactScrubsCredentials() {
        Map<String, String> cases = new LinkedHashMap<>();
        cases.put("Authorization: Bearer sk-abcdefghijklmnop", "sk-abcdefghijklmnop");
        cases.put("{\"api_key\": \"AIzaSyDsecretsecretsecret1234\"}", "AIzaSyDsecretsecretsecret1234");
        cases.put("apiKey=super-secret-value", "super-secret-value");
        cases.put("password: hunter2hunter2", "hunter2hunter2");
        cases.put("token = abc123def456ghi", "abc123def456ghi");
        cases.put("postgres://user:topsecret@db.internal:5432/x", "topsecret");
        cases.put("plain sk-0123456789abcdefXYZ text", "sk-0123456789abcdefXYZ");

        for (Map.Entry<String, String> entry : cases.entrySet()) {
            String cleaned = QueryForgeLogging.redact(entry.getKey());
            assertFalse(cleaned.contains(entry.getValue()),
                    "secret survived redaction: " + entry.getKey() + " -> " + cleaned);
            assertTrue(cleaned.contains("REDACTED"), "nothing was redacted in: " + cleaned);
        }
    }

    @Test
    @DisplayName("redact bounds text and announces the truncation")
    void redactBoundsAndAnnounces() {
        // Silent truncation would make a 500-byte prefix look like the whole reply and send
        // someone hunting for a bug in text that was never the full story.
        StringBuilder big = new StringBuilder();
        for (int i = 0; i < 5000; i++) {
            big.append('x');
        }
        String cleaned = QueryForgeLogging.redact(big.toString());
        assertTrue(cleaned.length() < 5000);
        assertTrue(cleaned.contains("truncated"));
    }

    @Test
    @DisplayName("redact leaves ordinary text alone")
    void redactLeavesOrdinaryTextAlone() {
        String text = "unknown field 'agee' (did you mean: age?)";
        assertEquals(text, QueryForgeLogging.redact(text));
    }

    @Test
    @DisplayName("engine stderr is scrubbed before it reaches an exception")
    void engineStderrIsScrubbed(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        TestSupport.useBinary(TestSupport.fakeBinary(
                        dir, null, 1, "fatal: Authorization: Bearer sk-LEAKED-TOKEN-9911", 0)
                .path());

        ProtocolException thrown = assertThrows(
                ProtocolException.class,
                () -> QueryForge.postgres(TestSupport.config()).query("x").toSql());

        assertFalse(thrown.getMessage().contains("sk-LEAKED-TOKEN-9911"));
        assertFalse(recorder.text().contains("sk-LEAKED-TOKEN-9911"));
    }

    @Test
    @DisplayName("the question never reaches a log, on success or on failure")
    void theQuestionNeverReachesALog(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        String canary = "CANARY-QUESTION-java-4c2e";

        TestSupport.useBinary(
                TestSupport.fakeBinary(dir.resolve("ok"), TestSupport.okResponse("sql", "SELECT 1"))
                        .path());
        QueryForge.postgres(TestSupport.config()).query("orders for " + canary).toSql();
        assertFalse(recorder.text().contains(canary), "the question leaked on the success path");

        TestSupport.useBinary(TestSupport.fakeBinary(
                        dir.resolve("fail"), TestSupport.errResponse("MODEL_TRANSPORT", "refused"))
                .path());
        assertThrows(
                ModelTransportException.class,
                () -> QueryForge.postgres(TestSupport.config()).query("orders for " + canary).toSql());
        assertFalse(recorder.text().contains(canary), "the question leaked on the failure path");
    }

    @Test
    @DisplayName("scope values never reach a log, but the keys do")
    void scopeValuesNeverReachALogButKeysDo(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        // Scope values are tenant, user and subscription ids — the most sensitive thing the SDK
        // handles. The keys are exactly what an audit trail needs.
        String canary = "CANARY-TENANT-java-9f1b";
        TestSupport.useBinary(
                TestSupport.fakeBinary(dir, TestSupport.okResponse("sql", "SELECT 1")).path());

        QueryForge.postgres(TestSupport.config())
                .query("delivered orders")
                .scope("customerName", canary)
                .toSql();

        assertFalse(recorder.text().contains(canary), "a scope VALUE reached the log");
        assertTrue(recorder.text().contains("customerName"),
                "the scope KEY should be logged for the audit trail");
    }

    @Test
    @DisplayName("the config never reaches a log")
    void theConfigNeverReachesALog(@TempDir Path dir) {
        assumeTrue(TestSupport.supportsShellFake());
        // A config carries physical table and column names, which plenty of organisations treat
        // as confidential. Only its shape is logged.
        TestSupport.useBinary(
                TestSupport.fakeBinary(dir, TestSupport.okResponse("sql", "SELECT 1")).path());

        QueryForge.postgres(TestSupport.config()).query("delivered orders").toSql();

        String text = recorder.text();
        for (String secret : Arrays.asList("total_amount", "customer_name", "internalNote")) {
            assertFalse(text.contains(secret), "config content '" + secret + "' reached a log");
        }
        assertTrue(text.contains("Order"), "the entity should be logged so two configs differ");
    }

    @Test
    @DisplayName("scopeKeys returns names only, sorted")
    void scopeKeysReturnsNamesOnly() {
        Map<String, Object> scope = new LinkedHashMap<>();
        scope.put("b", "secret");
        scope.put("a", "also-secret");
        assertEquals(Arrays.asList("a", "b"), QueryForgeLogging.scopeKeys(scope));
        assertEquals(Collections.emptyList(), QueryForgeLogging.scopeKeys(null));
        assertEquals(Collections.emptyList(), QueryForgeLogging.scopeKeys(Collections.emptyMap()));
    }

    // ------------------------------------------------------ the opt-in helpers

    @Test
    @DisplayName("the JSON formatter emits the canonical fields")
    void theJsonFormatterEmitsTheCanonicalFields() {
        ByteArrayOutputStream buffer = new ByteArrayOutputStream();
        Handler handler = QueryForgeLogging.configure(
                Level.INFO, new PrintStream(buffer, true, StandardCharsets.UTF_8));
        try {
            Map<String, Object> fields = new LinkedHashMap<>();
            fields.put(QueryForgeLogging.FIELD_OPERATION, "translate");
            fields.put(QueryForgeLogging.FIELD_ERROR_CODE, "MODEL_TRANSPORT");
            fields.put(QueryForgeLogging.FIELD_DURATION_MS, 42L);
            QueryForgeLogging.log(
                    QueryForgeLogging.logger(null), Level.INFO, "test event", fields);
        } finally {
            QueryForgeLogging.removeHandler(handler);
        }

        Map<String, Object> payload = Json.readObject(buffer.toString(StandardCharsets.UTF_8).trim());
        assertEquals("INFO", payload.get("level"));
        assertEquals("test event", payload.get("msg"));
        assertEquals("queryforge", payload.get("library"));
        assertEquals("java", payload.get("language"));
        assertEquals("translate", payload.get("operation"));
        assertEquals("MODEL_TRANSPORT", payload.get("error_code"));
        // A number stays a number rather than becoming a quoted string, or arithmetic in a
        // dashboard silently stops working.
        assertEquals(42L, ((Number) payload.get("duration_ms")).longValue());
        // The exact timestamp is never asserted anywhere in this file, only that it exists.
        assertNotNull(payload.get("time"));
    }

    @Test
    @DisplayName("configure touches only the QueryForge logger")
    void configureTouchesOnlyTheQueryForgeLogger() {
        Logger jvmRoot = Logger.getLogger("");
        Handler[] before = jvmRoot.getHandlers();
        Level beforeLevel = jvmRoot.getLevel();

        Handler handler = QueryForgeLogging.configure(Level.FINE);
        try {
            assertArrayEqualsByIdentity(before, jvmRoot.getHandlers());
            assertSame(beforeLevel, jvmRoot.getLevel());
            assertFalse(root.getUseParentHandlers(),
                    "parent handlers off, or every record is emitted twice");
        } finally {
            QueryForgeLogging.removeHandler(handler);
        }
    }

    @Test
    @DisplayName("the field map survives on the record for a structured handler")
    void theFieldMapSurvivesOnTheRecord() {
        // JUL has no key/value channel, so the fields travel as the record's parameters. A bridge
        // or a custom handler reads the map rather than parsing a sentence.
        QueryForgeLogging.log(
                QueryForgeLogging.logger(null),
                Level.INFO,
                "structured",
                Collections.singletonMap("operation", "validate"));

        Map<String, Object> fields = recorder.fieldsOf(recorder.all().size() - 1);
        assertEquals("validate", fields.get("operation"));
        assertEquals("queryforge", fields.get(QueryForgeLogging.FIELD_LIBRARY));
    }

    @Test
    @DisplayName("null field values are dropped rather than rendered as null")
    void nullFieldValuesAreDropped() {
        Map<String, Object> fields = new LinkedHashMap<>();
        fields.put("present", "yes");
        fields.put("absent", null);
        QueryForgeLogging.log(QueryForgeLogging.logger(null), Level.INFO, "nulls", fields);

        Map<String, Object> got = recorder.fieldsOf(recorder.all().size() - 1);
        assertEquals("yes", got.get("present"));
        assertFalse(got.containsKey("absent"),
                "a record's key set should say which facts were actually known");
    }

    @Test
    @DisplayName("a request id is short and unique")
    void aRequestIdIsShortAndUnique() {
        java.util.Set<String> ids = new java.util.HashSet<>();
        for (int i = 0; i < 500; i++) {
            String id = QueryForgeLogging.newRequestId();
            assertEquals(12, id.length());
            ids.add(id);
        }
        assertEquals(500, ids.size());
    }

    @Test
    @DisplayName("the SDK version reported in logs matches the pom")
    void theSdkVersionMatchesThePom() throws Exception {
        // A version that drifts makes every log line quietly wrong about which build produced it.
        String pom = new String(
                java.nio.file.Files.readAllBytes(Path.of("pom.xml")), StandardCharsets.UTF_8);
        String marker = "<version>" + QueryForgeLogging.SDK_VERSION + "</version>";
        assertTrue(pom.contains(marker),
                "QueryForgeLogging.SDK_VERSION (" + QueryForgeLogging.SDK_VERSION
                        + ") is not the pom's <version>");
    }

    @Test
    @DisplayName("an unconfigured SDK does not ask the engine for logs")
    void anUnconfiguredSdkDoesNotAskForLogs() {
        root.removeHandler(recorder);
        root.setLevel(Level.WARNING); // the SDK's own default floor
        assertFalse(QueryForgeLogging.isConfigured());
        assertNull(QueryForgeLogging.engineLevel());
    }

    private static void assertArrayEqualsByIdentity(Handler[] expected, Handler[] actual) {
        assertEquals(expected.length, actual.length, "handler count changed on the root logger");
        for (int i = 0; i < expected.length; i++) {
            assertSame(expected[i], actual[i]);
        }
    }
}
