package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * Tests against the real engine binary.
 *
 * <p>These are the ones that matter most: they prove the SDK and the Go engine agree on the wire
 * format. They use only the deterministic ops, so they need no API key and make no network call.
 */
class QueryForgeIntegrationTest {

    private static Path binary;

    @BeforeAll
    static void locateEngine() {
        binary = TestSupport.engineBinary();
        assumeTrue(binary != null, "no engine binary available and no Go toolchain to build one");
        TestSupport.useBinary(binary);
    }

    @AfterAll
    static void clearOverride() {
        TestSupport.clearBinary();
    }

    // ------------------------------------------------------------ handshake

    @Test
    @DisplayName("the version op reports the protocol and the registered backends")
    void engineVersionReportsProtocolAndBackends() {
        Map<String, Object> info = QueryForge.engineVersion();
        assertEquals(Boolean.TRUE, info.get("success"));
        assertTrue(((String) info.get("protocol")).startsWith("1."), "protocol: " + info.get("protocol"));
        assertTrue(
                Values.stringList(info.get("backends")).containsAll(TestSupport.BACKENDS),
                "backends: " + info.get("backends"));
    }

    // --------------------------------------------------- deterministic path

    @Test
    @DisplayName("a valid AST compiles to parameterized PostgreSQL")
    void generatesPostgres() {
        QueryForgeResult result = QueryForge.postgres(TestSupport.config()).generate(TestSupport.validAst());

        assertEquals("sql", result.getBackend());
        assertTrue(result.getSql().contains("FROM orders"), result.getSql());
        // The value must be bound, never inlined — the injection guarantee has to survive the
        // round trip through JSON and into a Java List.
        assertEquals(Collections.singletonList("DELIVERED"), result.getArgs());
        assertFalse(result.getSql().contains("DELIVERED"), "value was inlined: " + result.getSql());
        assertFalse(result.getExplain().isEmpty());
    }

    @Test
    @DisplayName("the same AST produces each dialect's own placeholder style")
    void dialectsDifferOnlyInSyntax() {
        QueryForgeResult postgres = QueryForge.postgres(TestSupport.config()).generate(TestSupport.validAst());
        QueryForgeResult mysql = QueryForge.mysql(TestSupport.config()).generate(TestSupport.validAst());

        assertTrue(postgres.getSql().contains("$1"), postgres.getSql());
        assertTrue(mysql.getSql().contains("?"), mysql.getSql());
        // Same AST, same bound values — only the dialect differs. This is the promise the
        // single-source-of-truth design rests on.
        assertEquals(postgres.getArgs(), mysql.getArgs());
    }

    @Test
    @DisplayName("a document backend returns a query document rather than SQL")
    void generatesMongo() {
        QueryForgeResult result = QueryForge.mongo(TestSupport.config()).generate(TestSupport.validAst());

        assertFalse(result.isSql());
        Map<String, Object> doc = result.getDoc();
        assertEquals("orders", doc.get("collection"));
        assertEquals("DELIVERED", Values.map(doc.get("filter")).get("status"));
    }

    @Test
    @DisplayName("a non-returnable field never reaches the projection")
    void hiddenFieldsStayHidden() {
        // returnable:false must hold on the default "all fields" path, not just when an explicit
        // select list happens to be requested.
        String sql = QueryForge.postgres(TestSupport.config()).generate(TestSupport.validAst()).getSql();
        assertFalse(sql.contains("internalNote"), sql);
        assertFalse(sql.contains("internal_note"), sql);
    }

    @Test
    @DisplayName("scope is AND-ed in, bound as a parameter, and reported back")
    void scopeIsAppliedAndReported() {
        QueryForgeResult result = QueryForge.postgres(TestSupport.config())
                .generate(TestSupport.validAst(), Collections.singletonMap("customerName", "ACME"));

        assertTrue(result.getSql().contains("customer_name"), result.getSql());
        assertTrue(result.getArgs().contains("ACME"));
        assertEquals(2, result.getArgs().size());

        assertEquals(1, result.getScope().size());
        ScopeFilter applied = result.getScope().get(0);
        assertEquals("customerName", applied.getField());
        assertEquals("equals", applied.getOperator());
        // The tagged-union wrapper is unwrapped: an audit log wants "ACME", not
        // {"kind":"string","v":"ACME"}.
        assertEquals("ACME", applied.getValue());
        assertTrue(applied.isDeclared());
    }

    @Test
    @DisplayName("withScope applies to every query the instance produces")
    void instanceScopeApplies() {
        QueryForgeResult result = QueryForge.postgres(TestSupport.config())
                .withScope(Collections.singletonMap("customerName", "ACME"))
                .generate(TestSupport.validAst());
        assertEquals(1, result.getScope().size());
    }

    @Test
    @DisplayName("the reported AST round-trips back through generate")
    void reportedAstRoundTrips() {
        QueryForge forge = QueryForge.postgres(TestSupport.config());
        Map<String, Object> scope = Collections.singletonMap("customerName", "ACME");

        QueryForgeResult first = forge.generate(TestSupport.validAst(), scope);
        QueryForgeResult second = forge.generate(first.getAst(), scope);

        assertEquals(first.getSql(), second.getSql());
        assertEquals(first.getArgs(), second.getArgs());
    }

    @Test
    @DisplayName("warnings surface filters on non-indexed fields")
    void warningsSurface() {
        QueryForgeResult result = QueryForge.postgres(TestSupport.config())
                .generate(TestSupport.validAst(), Collections.singletonMap("customerName", "ACME"));
        assertTrue(
                result.getWarnings().stream().anyMatch(w -> w.contains("customerName")),
                "warnings: " + result.getWarnings());
    }

    // ------------------------------------------------------------ validation

    @Test
    @DisplayName("validate accepts a legal AST and compiles nothing")
    void validateAcceptsLegalAst() {
        QueryForgeResult result = QueryForge.postgres(TestSupport.config()).validate(TestSupport.validAst());
        assertFalse(result.getExplain().isEmpty());
        assertFalse(result.isSql(), "validate must not compile anything");
    }

    @Test
    @DisplayName("an unknown field fails closed with structured, actionable detail")
    void unknownFieldCarriesDetails() {
        ValidationException e = assertThrows(
                ValidationException.class,
                () -> QueryForge.postgres(TestSupport.config()).generate(TestSupport.astWithUnknownField()));

        assertEquals("VALIDATION_FAILED", e.getCode());
        assertFalse(e.getDetails().isEmpty(), "an SDK must be able to name the offending field");

        Detail detail = e.getDetails().get(0);
        assertEquals("unknown_field", detail.getCode());
        assertEquals("amont", detail.getField());
        assertFalse(detail.getPath().isEmpty());
        // The suggestion list is the difference between "unknown field" and "did you mean
        // amount?", and must arrive as data rather than only as prose.
        assertTrue(detail.getSuggestions().contains("amount"), "suggestions: " + detail.getSuggestions());
    }

    @Test
    @DisplayName("an out-of-domain enum value is rejected")
    void outOfDomainEnumIsRejected() {
        Map<String, Object> bad = QueryForge.parseConfig(
                "{\"version\":\"1.0\",\"entity\":\"Order\",\"filter\":{\"type\":\"comparison\","
                        + "\"field\":\"status\",\"operator\":\"equals\","
                        + "\"value\":{\"kind\":\"enum\",\"v\":\"TELEPORTED\"}}}");

        ValidationException e = assertThrows(
                ValidationException.class, () -> QueryForge.postgres(TestSupport.config()).generate(bad));
        assertEquals("value_out_of_domain", e.getDetails().get(0).getCode());
    }

    // ---------------------------------------------------------------- misuse

    @Test
    @DisplayName("asking a Mongo result for SQL explains itself")
    void wrongBackendAccessorExplainsItself() {
        // Returning "" would let the caller execute an empty statement and get an opaque driver
        // error several frames from the mistake.
        QueryForgeResult mongo = QueryForge.mongo(TestSupport.config()).generate(TestSupport.validAst());
        QueryForgeException e = assertThrows(QueryForgeException.class, mongo::getSql);
        assertTrue(e.getMessage().contains("getDoc"), e.getMessage());

        QueryForgeResult sql = QueryForge.postgres(TestSupport.config()).generate(TestSupport.validAst());
        QueryForgeException e2 = assertThrows(QueryForgeException.class, sql::getDoc);
        assertTrue(e2.getMessage().contains("getSql"), e2.getMessage());
    }

    @Test
    @DisplayName("an unknown backend is reported with the registered list")
    void unknownBackendIsReported() {
        UnknownBackendException e = assertThrows(
                UnknownBackendException.class,
                () -> QueryForge.forBackend(TestSupport.config(), "oracle").generate(TestSupport.validAst()));
        assertTrue(e.getMessage().contains("sql"), e.getMessage());
    }

    @Test
    @DisplayName("a bad scope is told apart from a bad question")
    void badScopeIsItsOwnError() {
        // Scope comes from the application session, never the end user, so it must map to an
        // error an application developer will recognise as their own bug.
        assertThrows(
                InvalidScopeException.class,
                () -> QueryForge.postgres(TestSupport.config())
                        .generate(TestSupport.validAst(), Collections.singletonMap("", "nothing")));
    }

    @Test
    @DisplayName("a structurally invalid config is rejected by the engine")
    void invalidConfigIsRejected() {
        Map<String, Object> broken =
                QueryForge.parseConfig("{\"entity\":\"Order\",\"fields\":[{\"name\":\"s\",\"type\":\"enum\"}]}");
        assertThrows(
                InvalidConfigException.class,
                () -> QueryForge.postgres(broken).generate(TestSupport.validAst()));
    }

    @Test
    @DisplayName("a config error never echoes a pasted secret")
    void configErrorDoesNotEchoSecrets() {
        Map<String, Object> leaky = QueryForge.parseConfig(
                "{\"entity\":\"Order\",\"model\":{\"apiKeyEnv\":\"sk-ant-supersecret\"},"
                        + "\"fields\":[{\"name\":\"status\",\"type\":\"string\"}]}");

        InvalidConfigException e = assertThrows(
                InvalidConfigException.class,
                () -> QueryForge.postgres(leaky).generate(TestSupport.validAst()));
        assertFalse(e.getMessage().contains("supersecret"), e.getMessage());
    }

    // --------------------------------------------------------- config loading

    @Test
    @DisplayName("a config reads identically from a Map, a file and a JSON string")
    void configSourcesAgree(@TempDir Path tmp) throws Exception {
        Path file = tmp.resolve("orders.config.json");
        Files.write(file, Json.write(TestSupport.config()).getBytes("UTF-8"));

        String fromMap = QueryForge.postgres(TestSupport.config()).generate(TestSupport.validAst()).getSql();
        String fromFile = QueryForge.postgres(file).generate(TestSupport.validAst()).getSql();
        String fromText = QueryForge.postgres(QueryForge.parseConfig(Json.write(TestSupport.config())))
                .generate(TestSupport.validAst())
                .getSql();

        assertEquals(fromMap, fromFile);
        assertEquals(fromMap, fromText);
    }

    @Test
    @DisplayName("mutating the caller's config map does not change a built instance")
    void configIsCopiedNotAliased() {
        Map<String, Object> config = new LinkedHashMap<>(TestSupport.config());
        QueryForge forge = QueryForge.postgres(config);
        config.put("entity", "Mutated");

        // Would fail validation with an entity mismatch if the map were aliased.
        assertNotNull(forge.generate(TestSupport.validAst()).getSql());
    }

    @Test
    @DisplayName("a missing config file names the path")
    void missingConfigFileNamesThePath(@TempDir Path tmp) {
        Path absent = tmp.resolve("absent.json");
        InvalidConfigException e =
                assertThrows(InvalidConfigException.class, () -> QueryForge.postgres(absent));
        assertTrue(e.getMessage().contains("absent.json"), e.getMessage());
    }

    @Test
    @DisplayName("a malformed config file is reported before anything is spawned")
    void malformedConfigFileIsReported(@TempDir Path tmp) throws Exception {
        Path broken = tmp.resolve("broken.json");
        Files.write(broken, "{\"entity\": \"Order\",".getBytes("UTF-8"));

        InvalidConfigException e =
                assertThrows(InvalidConfigException.class, () -> QueryForge.postgres(broken));
        assertTrue(e.getMessage().contains("not valid JSON"), e.getMessage());
    }

    // ----------------------------------------------------- binary extraction

    @Test
    @DisplayName("binaryPath resolves to a real executable")
    void binaryPathResolves() {
        Path resolved = Path.of(QueryForge.binaryPath());
        assertTrue(Files.isRegularFile(resolved), "not a file: " + resolved);
        assertTrue(Files.isExecutable(resolved), "not executable: " + resolved);
    }

    @Test
    @DisplayName("the platform tag is one of the release targets")
    void platformTagIsARealTarget() {
        List<String> targets = List.of(
                "linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64",
                "windows-amd64", "windows-arm64");
        assertTrue(targets.contains(QueryForge.platformTag()), QueryForge.platformTag());
    }
}
