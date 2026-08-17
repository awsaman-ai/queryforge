package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import java.io.IOException;
import java.io.UncheckedIOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * Supplying an API key from somewhere other than the JVM's own environment.
 *
 * <p>The rules under test:
 *
 * <ul>
 *   <li>A key handed to the SDK reaches the engine subprocess, and nowhere else — not the request
 *       body, not the logs, not this JVM's environment.
 *   <li>The child still inherits everything else, so credentials ADD to the environment rather
 *       than replacing it.
 *   <li>Only the op that talks to a model receives the key.
 *   <li>A malformed name fails where the mistake is, and never by echoing the secret.
 * </ul>
 */
class QueryForgeCredentialsTest {

    private static final String SECRET = "sk-test-not-a-real-key-9f8e7d6c5b4a";

    @TempDir Path tmp;

    private Path envLog;

    @BeforeEach
    void setUp() {
        assumeTrue(TestSupport.supportsShellFake(), "needs a POSIX shell");
    }

    @AfterEach
    void tearDown() {
        TestSupport.clearBinary();
    }

    /**
     * A fake engine that records its own environment as well as the request it was sent. The
     * shared fake logs only stdin, which cannot answer the question these tests ask: did the
     * credential arrive out of band?
     */
    private Path installEnvRecordingEngine() {
        try {
            envLog = tmp.resolve("env.txt");
            Path reqLog = tmp.resolve("requests.jsonl");
            Path script = tmp.resolve("fake_engine.sh");

            // Both logs append, so a test that reinstalls the fake to observe a
            // second call must start from a clean slate or read the first one back.
            Files.deleteIfExists(envLog);
            Files.deleteIfExists(reqLog);

            String response =
                    "{\"success\":true,\"protocol\":\"1.0\",\"op\":\"translate\","
                            + "\"backend\":\"sql\",\"sql\":\"SELECT 1\",\"args\":[]}";

            String body =
                    "#!/bin/sh\n"
                            + "cat >> '" + reqLog + "'\n"
                            + "printf '\\n' >> '" + reqLog + "'\n"
                            + "env >> '" + envLog + "'\n"
                            + "printf '%s' '" + response + "'\n";
            Files.write(script, body.getBytes(StandardCharsets.UTF_8));
            if (!script.toFile().setExecutable(true, true)) {
                throw new IOException("could not mark the fake engine executable");
            }
            TestSupport.useBinary(script);
            return reqLog;
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }

    /** Reads one variable out of the environment the child actually saw. */
    private String childEnv(String name) {
        try {
            if (!Files.isRegularFile(envLog)) {
                return null;
            }
            for (String line : Files.readAllLines(envLog, StandardCharsets.UTF_8)) {
                int eq = line.indexOf('=');
                if (eq > 0 && line.substring(0, eq).equals(name)) {
                    return line.substring(eq + 1);
                }
            }
            return null;
        } catch (IOException e) {
            throw new UncheckedIOException(e);
        }
    }

    private static Map<String, Object> credentialConfig() {
        return QueryForge.parseConfig(
                "{"
                        + "\"entity\":\"Order\","
                        + "\"model\":{\"provider\":\"openai\",\"model\":\"gpt-5\","
                        + "\"apiKeyEnv\":\"QF_TEST_KEY\"},"
                        + "\"backends\":{\"sql\":{\"table\":\"orders\"}},"
                        + "\"fields\":[{\"name\":\"status\",\"type\":\"string\","
                        + "\"operators\":[\"equals\"]}]"
                        + "}");
    }

    // ----------------------------------------------------------- the happy path

    @Test
    @DisplayName("a credential handed to the SDK reaches the engine process")
    void credentialReachesTheEngine() {
        installEnvRecordingEngine();

        QueryForge.sql(credentialConfig())
                .withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET))
                .query("delivered orders")
                .toSql();

        assertEquals(SECRET, childEnv("QF_TEST_KEY"));
    }

    @Test
    @DisplayName("the credential never enters the request body")
    void credentialStaysOutOfTheRequest() throws IOException {
        Path reqLog = installEnvRecordingEngine();

        QueryForge.sql(credentialConfig())
                .withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET))
                .query("delivered orders")
                .toSql();

        String sent = new String(Files.readAllBytes(reqLog), StandardCharsets.UTF_8);
        assertFalse(
                sent.contains(SECRET),
                "the request is JSON-encoded, dumped on protocol errors and pasted into bug "
                        + "reports; the secret must not be in it");
    }

    @Test
    @DisplayName("the credential does not touch this JVM's own environment")
    void credentialDoesNotLeakIntoThisProcess() {
        installEnvRecordingEngine();

        QueryForge.sql(credentialConfig())
                .withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET))
                .query("q")
                .toSql();

        assertNotEquals(
                SECRET,
                System.getenv("QF_TEST_KEY"),
                "mutating the JVM environment would leak the key to every later subprocess");
    }

    @Test
    @DisplayName("the child still inherits the rest of the environment")
    void childKeepsTheInheritedEnvironment() {
        installEnvRecordingEngine();

        QueryForge.sql(credentialConfig())
                .withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET))
                .query("q")
                .toSql();

        assertNotNull(
                childEnv("PATH"),
                "a bare credentials map would strip PATH and break the engine in ways that look "
                        + "nothing like a credentials problem");
    }

    @Test
    @DisplayName("two instances with different keys do not interfere")
    void instancesAreIndependent() {
        installEnvRecordingEngine();

        QueryForge base = QueryForge.sql(credentialConfig());
        QueryForge tenantA = base.withCredentials(Collections.singletonMap("QF_TEST_KEY", "key-a"));
        QueryForge tenantB = base.withCredentials(Collections.singletonMap("QF_TEST_KEY", "key-b"));

        tenantA.query("q").toSql();
        assertEquals("key-a", childEnv("QF_TEST_KEY"));

        // The env log appends, and childEnv returns the FIRST match, so read the
        // second call from a clean log.
        installEnvRecordingEngine();
        tenantB.query("q").toSql();
        assertEquals("key-b", childEnv("QF_TEST_KEY"));
    }

    @Test
    @DisplayName("several withCredentials calls compose")
    void credentialsMerge() {
        installEnvRecordingEngine();

        QueryForge.sql(credentialConfig())
                .withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET))
                .withCredentials(Collections.singletonMap("QF_OTHER_KEY", "second"))
                .query("q")
                .toSql();

        assertEquals(SECRET, childEnv("QF_TEST_KEY"));
        assertEquals("second", childEnv("QF_OTHER_KEY"));
    }

    @Test
    @DisplayName("withCredentials returns a copy and leaves the original alone")
    void withCredentialsIsImmutable() {
        installEnvRecordingEngine();

        QueryForge base = QueryForge.sql(credentialConfig());
        QueryForge derived = base.withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET));

        base.query("q").toSql();
        assertNotEquals(
                SECRET, childEnv("QF_TEST_KEY"), "the original instance must not gain the key");

        assertNotNull(derived);
    }

    @Test
    @DisplayName("an empty or null credentials map is a no-op")
    void emptyCredentialsChangeNothing() {
        QueryForge base = QueryForge.sql(credentialConfig());

        assertSame(base, base.withCredentials(null));
        assertSame(base, base.withCredentials(Collections.<String, String>emptyMap()));
    }

    // -------------------------------------------------------- least privilege

    @Test
    @DisplayName("offline ops never receive the key")
    void generateDoesNotGetTheCredential() {
        installEnvRecordingEngine();

        Map<String, Object> ast = new LinkedHashMap<>();
        ast.put("version", "1.0");
        ast.put("entity", "Order");

        QueryForge.sql(credentialConfig())
                .withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET))
                .generate(ast);

        assertNotEquals(
                SECRET,
                childEnv("QF_TEST_KEY"),
                "generate makes no model call, so the engine it spawns has no use for a key");
    }

    // -------------------------------------------------------------- bad input

    @Test
    @DisplayName("a pasted key where a name belongs is rejected")
    void pastedKeyIsRejected() {
        QueryForge qf = QueryForge.sql(credentialConfig());

        InvalidRequestException e =
                assertThrows(
                        InvalidRequestException.class,
                        () -> qf.withCredentials(Collections.singletonMap(SECRET, "value")));

        assertTrue(
                e.getMessage().contains("environment variable name"),
                "the message should explain that the key is a NAME: " + e.getMessage());
        assertTrue(
                e.getMessage().contains("model.apiKeyEnv"),
                "the message should point at the config key it corresponds to");
    }

    @Test
    @DisplayName("an invalid variable name fails at configuration time, not query time")
    void invalidNamesRejectedEarly() {
        QueryForge qf = QueryForge.sql(credentialConfig());

        for (String bad : new String[] {"1LEADING", "has-hyphen", "has space", "", "has.dot"}) {
            assertThrows(
                    InvalidRequestException.class,
                    () -> qf.withCredentials(Collections.singletonMap(bad, "v")),
                    "name \"" + bad + "\" should have been rejected");
        }
    }

    @Test
    @DisplayName("a null value is rejected without printing it")
    void nullValueRejected() {
        QueryForge qf = QueryForge.sql(credentialConfig());
        Map<String, String> creds = new LinkedHashMap<>();
        creds.put("QF_TEST_KEY", null);

        InvalidRequestException e =
                assertThrows(InvalidRequestException.class, () -> qf.withCredentials(creds));
        assertTrue(e.getMessage().contains("QF_TEST_KEY"));
    }

    @Test
    @DisplayName("the env-name rule matches the engine's own")
    void envNameRuleMatchesTheEngine() {
        // Legal POSIX names.
        assertTrue(Transport.isEnvName("QF_API_KEY"));
        assertTrue(Transport.isEnvName("_underscore"));
        assertTrue(Transport.isEnvName("A1"));
        assertTrue(Transport.isEnvName("lowercase_ok"));

        // Illegal, and each is a mistake someone will actually make.
        assertFalse(Transport.isEnvName("1starts_with_digit"));
        assertFalse(Transport.isEnvName("sk-ant-akeynotaname"));
        assertFalse(Transport.isEnvName("has space"));
        assertFalse(Transport.isEnvName(""));
        assertFalse(Transport.isEnvName(null));
    }

    // ----------------------------------------------------------------- logging

    @Test
    @DisplayName("the credential never appears in the SDK's logs")
    void credentialIsNeverLogged() throws IOException {
        installEnvRecordingEngine();

        Path logFile = tmp.resolve("sdk.log");
        java.util.logging.Logger logger = java.util.logging.Logger.getLogger("io.queryforge");
        java.util.logging.FileHandler handler = new java.util.logging.FileHandler(logFile.toString());
        handler.setLevel(java.util.logging.Level.ALL);
        logger.addHandler(handler);
        logger.setLevel(java.util.logging.Level.ALL);

        try {
            QueryForge.sql(credentialConfig())
                    .withCredentials(Collections.singletonMap("QF_TEST_KEY", SECRET))
                    .query("delivered orders")
                    .toSql();
        } finally {
            handler.close();
            logger.removeHandler(handler);
            logger.setLevel(null);
        }

        List<String> lines = Files.readAllLines(logFile, StandardCharsets.UTF_8);
        for (String line : lines) {
            assertFalse(line.contains(SECRET), "a log line carried the secret: " + line);
        }
    }
}
