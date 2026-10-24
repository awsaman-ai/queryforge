package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * {@code model.apiKey}: the key written into the config itself.
 *
 * <p>The engine owns the behaviour; these tests prove the SDK carries it faithfully and never
 * becomes a second place the secret can leak from. They run the REAL engine against a local fake
 * model endpoint, so the whole path is covered: config map, SDK, engine subprocess, HTTP request.
 */
class QueryForgeInlineKeyTest {

    private static final String KEY = "AIzaSyLEAKCANARY-0123456789abcdef";

    private static final String AST =
            "{\"version\":\"1.0\",\"entity\":\"Order\",\"filter\":{\"type\":\"comparison\","
                    + "\"field\":\"status\",\"operator\":\"equals\",\"value\":{\"kind\":\"enum\",\"v\":\"DELIVERED\"}}}";

    private static Path binary;
    private HttpServer server;
    private final List<String> auth = new CopyOnWriteArrayList<>();

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

    @AfterEach
    void stop() {
        if (server != null) {
            server.stop(0);
        }
    }

    /** A throwaway OpenAI-compatible endpoint that records the Authorization header. */
    private String endpoint(int status) throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", ex -> {
            ex.getRequestBody().readAllBytes();
            String a = ex.getRequestHeaders().getFirst("Authorization");
            auth.add(a);
            String body;
            if (status == 200) {
                body = "{\"choices\":[{\"message\":{\"content\":" + Json.write(AST) + "}}]}";
            } else {
                // A badly behaved gateway: echo the credential back in the error.
                body = "invalid credential " + a;
            }
            byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().add("Content-Type", "application/json");
            ex.sendResponseHeaders(status, bytes.length);
            try (OutputStream os = ex.getResponseBody()) {
                os.write(bytes);
            }
        });
        server.start();
        return "http://127.0.0.1:" + server.getAddress().getPort();
    }

    private static Map<String, Object> cfg(String url, Map<String, Object> modelExtras) {
        Map<String, Object> cfg = new LinkedHashMap<>(TestSupport.config());
        Map<String, Object> model = new LinkedHashMap<>();
        model.put("provider", "custom");
        model.put("baseURL", url);
        model.put("model", "m");
        model.putAll(modelExtras);
        cfg.put("model", model);
        return cfg;
    }

    private static Map<String, Object> m(Object... kv) {
        Map<String, Object> out = new LinkedHashMap<>();
        for (int i = 0; i < kv.length; i += 2) {
            out.put((String) kv[i], kv[i + 1]);
        }
        return out;
    }

    @Test
    @DisplayName("an inline key reaches the model endpoint")
    void inlineKeyReachesTheEndpoint() throws Exception {
        String url = endpoint(200);
        String sql = QueryForge.postgres(cfg(url, m("apiKey", KEY))).query("delivered orders").toSql();
        assertTrue(sql.contains("status"), sql);
        assertEquals(List.of("Bearer " + KEY), auth);
    }

    @Test
    @DisplayName("whitespace pasted around the key is trimmed")
    void pastedWhitespaceIsTrimmed() throws Exception {
        String url = endpoint(200);
        QueryForge.postgres(cfg(url, m("apiKey", "  " + KEY + "\n"))).query("delivered orders").toSql();
        assertEquals(List.of("Bearer " + KEY), auth);
    }

    @Test
    @DisplayName("bad inline keys are rejected and the key is never echoed")
    void badKeysRejectedWithoutEcho() {
        List<Map<String, Object>> bad = new ArrayList<>();
        bad.add(m("apiKey", KEY, "apiKeyEnv", "QF_API_KEY")); // both
        bad.add(m("apiKey", "AIza has a space"));
        bad.add(m("apiKey", "AIzaabc\ndef"));
        bad.add(m("apiKey", "   "));
        bad.add(m("apiKey", "x".repeat(5000)));
        for (Map<String, Object> model : bad) {
            Map<String, Object> c = cfg("http://127.0.0.1:1", model);
            InvalidConfigException e = assertThrows(
                    InvalidConfigException.class, () -> QueryForge.postgres(c).generate(TestSupport.validAst()));
            String text = String.valueOf(e.getMessage()) + e;
            for (String fragment : new String[] {KEY, "AIza has", "AIzaabc", "LEAKCANARY"}) {
                assertFalse(text.contains(fragment), "leaked " + fragment + " in: " + text);
            }
        }
    }

    @Test
    @DisplayName("a key the provider rejects (and echoes) does not leak through the exception")
    void rejectedKeyNotLeaked() throws Exception {
        String url = endpoint(401);
        Exception e = assertThrows(
                Exception.class,
                () -> QueryForge.postgres(cfg(url, m("apiKey", KEY))).query("delivered orders").toSql());
        String text = e.getMessage() + e;
        for (Throwable t = e.getCause(); t != null; t = t.getCause()) {
            text += t.getMessage();
        }
        assertNotNull(text);
        assertFalse(text.contains(KEY) || text.contains("LEAKCANARY"), "leaked: " + text);
    }

    @Test
    @DisplayName("an inline key in a fallback entry is used when the primary is unreachable")
    void inlineKeyInFallbackEntry() throws Exception {
        String url = endpoint(200);
        Map<String, Object> c = cfg("http://127.0.0.1:1", m("apiKeyEnv", "NOT_SET_ANYWHERE", "timeoutSeconds", 2, "maxRetries", 0));
        List<Object> models = new ArrayList<>();
        models.add(m("provider", "custom", "baseURL", url, "model", "m", "apiKey", KEY));
        c.put("models", models);
        QueryForge.postgres(c).query("delivered orders").toSql();
        assertEquals(List.of("Bearer " + KEY), auth);
    }

    @Test
    @DisplayName("the key is not in the forge's toString")
    void keyNotInToString() throws Exception {
        String url = endpoint(200);
        QueryForge forge = QueryForge.postgres(cfg(url, m("apiKey", KEY)));
        assertFalse(forge.toString().contains(KEY));
    }
}
