package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.io.TempDir;

/**
 * Amazon Bedrock through the real engine binary.
 *
 * <p>The SDK holds no Bedrock logic: it forwards {@code model.aws} as opaque JSON and carries
 * {@code AWS_*} variables to the engine through {@code withCredentials}. These tests pin that
 * contract end to end against a fake Bedrock endpoint, using a binary built from THIS checkout
 * (a cached one may predate the feature).
 */
class QueryForgeBedrockTest {

    private static final String INLINE_SECRET = "ABSKinline-bearer-secret-value-123";
    private static final String ENV_SECRET = "bearer-from-the-credentials-map-456";

    private static final String AST_REPLY =
            "{\"version\":\"1.0\",\"entity\":\"Order\",\"filter\":{\"type\":\"comparison\","
                    + "\"field\":\"status\",\"operator\":\"equals\","
                    + "\"value\":{\"kind\":\"enum\",\"v\":\"DELIVERED\"}}}";

    @TempDir static Path binDir;
    private static Path binary;

    private HttpServer server;
    private final List<String[]> seen = new CopyOnWriteArrayList<>(); // {path, authorization, body}

    @BeforeAll
    static void buildFreshEngine() throws Exception {
        binary = binDir.resolve("queryforge");
        boolean built;
        try {
            built = new ProcessBuilder("go", "build", "-o", binary.toString(), "./cmd/queryforge")
                            .directory(TestSupport.repoRoot().toFile())
                            .redirectErrorStream(true)
                            .start()
                            .waitFor()
                    == 0;
        } catch (IOException e) { // no Go toolchain on PATH
            built = false;
        }
        assumeTrue(built && Files.isExecutable(binary), "needs a Go toolchain to build the engine from this checkout");
        TestSupport.useBinary(binary);
    }

    @AfterAll
    static void clearOverride() {
        TestSupport.clearBinary();
    }

    @BeforeEach
    void startFakeBedrock() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            String body = new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
            seen.add(new String[] {
                exchange.getRequestURI().getRawPath(), exchange.getRequestHeaders().getFirst("Authorization"), body
            });
            String reply = "{\"output\":{\"message\":{\"content\":[{\"text\":" + Json.write(AST_REPLY) + "}]}},"
                    + "\"stopReason\":\"end_turn\",\"usage\":{\"inputTokens\":3,\"outputTokens\":4}}";
            byte[] out = reply.getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().add("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, out.length);
            exchange.getResponseBody().write(out);
            exchange.close();
        });
        server.start();
    }

    @AfterEach
    void stopFakeBedrock() {
        server.stop(0);
    }

    private Map<String, Object> config(String awsFields) {
        return QueryForge.parseConfig(
                "{\"entity\":\"Order\","
                        + "\"model\":{\"provider\":\"bedrock\",\"model\":\"us.anthropic.claude-sonnet-4-6\","
                        + "\"baseURL\":\"http://127.0.0.1:" + server.getAddress().getPort() + "\","
                        + "\"aws\":{\"region\":\"us-east-1\"" + (awsFields.isEmpty() ? "" : "," + awsFields) + "}},"
                        + "\"backends\":{\"sql\":{\"table\":\"orders\"}},"
                        + "\"fields\":[{\"name\":\"status\",\"type\":\"enum\","
                        + "\"values\":[\"NEW\",\"DELIVERED\"],\"operators\":[\"equals\"]}]}");
    }

    @Test
    @DisplayName("a credential supplied by reference arrives through withCredentials")
    void credentialByReference() {
        Map<String, String> creds = new HashMap<>();
        creds.put("AWS_BEARER_TOKEN_BEDROCK", ENV_SECRET);

        PendingQuery q = QueryForge.sql(config("\"bearerTokenEnv\":\"AWS_BEARER_TOKEN_BEDROCK\""))
                .withCredentials(creds)
                .query("delivered orders");

        assertTrue(q.toSql().contains("status"));
        assertEquals("Bearer " + ENV_SECRET, seen.get(0)[1]);
        assertEquals("/model/us.anthropic.claude-sonnet-4-6/converse", seen.get(0)[0]);
        // By reference means the config holds no secret, hence no inline warning.
        assertFalse(String.join(" ", q.warnings()).contains("inline"));
    }

    @Test
    @DisplayName("the standard variable works with no credential in the aws block at all")
    void zeroConfigPath() {
        Map<String, String> creds = new HashMap<>();
        creds.put("AWS_BEARER_TOKEN_BEDROCK", ENV_SECRET);

        QueryForge.sql(config("")).withCredentials(creds).query("delivered orders").toSql();

        assertEquals("Bearer " + ENV_SECRET, seen.get(0)[1]);
    }

    @Test
    @DisplayName("an inline credential works, is warned about by name, and stays out of the body")
    void inlineCredentialWarns() {
        PendingQuery q = QueryForge.sql(config("\"bearerToken\":\"" + INLINE_SECRET + "\"")).query("delivered orders");
        q.toSql();

        assertEquals("Bearer " + INLINE_SECRET, seen.get(0)[1]);
        String warnings = String.join(" ", q.warnings());
        assertTrue(warnings.contains("aws.bearerToken"), warnings);
        assertFalse(warnings.contains(INLINE_SECRET), "the warning must name the field, never the value");
        assertFalse(seen.get(0)[2].contains(INLINE_SECRET), "the credential must not be sent in the request body");
    }

    @Test
    @DisplayName("IAM keys with a session token sign the request")
    void sigV4() {
        Map<String, String> creds = new HashMap<>();
        creds.put("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE");
        creds.put("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY");
        creds.put("AWS_SESSION_TOKEN", "temporary-session-token");

        QueryForge.sql(config(
                        "\"accessKeyIdEnv\":\"AWS_ACCESS_KEY_ID\",\"secretAccessKeyEnv\":\"AWS_SECRET_ACCESS_KEY\","
                                + "\"sessionTokenEnv\":\"AWS_SESSION_TOKEN\""))
                .withCredentials(creds)
                .query("delivered orders")
                .toSql();

        String auth = seen.get(0)[1];
        assertTrue(auth.startsWith("AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"), auth);
        assertTrue(auth.contains("/us-east-1/bedrock/aws4_request"), auth);
        assertTrue(auth.contains("x-amz-security-token"), auth);
        assertFalse(auth.contains("wJalrXUtnFEMI"), "the secret key must never appear in a header");
    }
}
