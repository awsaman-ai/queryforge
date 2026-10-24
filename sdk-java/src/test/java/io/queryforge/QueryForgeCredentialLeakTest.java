package io.queryforge;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import com.sun.net.httpserver.HttpServer;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.OutputStream;
import java.io.PrintStream;
import java.io.PrintWriter;
import java.io.StringWriter;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.logging.Handler;
import java.util.logging.Level;
import org.junit.jupiter.api.AfterAll;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Nothing the SDK logs, throws or prints may carry a credential.
 *
 * <p>Each scenario plants a canary as the key, makes a misbehaving model endpoint reflect it back
 * every way a gateway might, then searches everything a caller could end up writing to a log: the
 * DEBUG log stream (including stack traces), every exception's message and full cause chain, and
 * the toString of the objects holding the config.
 */
class QueryForgeCredentialLeakTest {

    private static final String KEY = "AIzaSyLEAKCANARY-0123456789abcdef";
    private static final String ENV_KEY = "ENVCANARY-9876543210-abcdefghij";
    private static final String[] CANARIES = {"LEAKCANARY", "ENVCANARY", KEY, ENV_KEY};

    private static Path binary;
    private HttpServer server;

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

    private String reflector(String mode) throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", ex -> {
            ex.getRequestBody().readAllBytes();
            String echo = ex.getRequestHeaders().getFirst("Authorization") + " " + ex.getRequestHeaders().getFirst("x-api-key");
            int code = 200;
            String body;
            switch (mode) {
                case "401": code = 401; body = "unauthorized: " + echo; break;
                case "500": code = 500; body = "<html>" + echo + "</html>"; break;
                case "400-json": code = 400; body = "{\"error\":{\"message\":\"bad " + echo + "\"}}"; break;
                case "reply": body = "{\"choices\":[{\"message\":{\"content\":" + Json.write("no. " + echo) + "}}]}"; break;
                default: body = "garbage " + echo;
            }
            byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
            ex.getResponseHeaders().add("Content-Type", "application/json");
            ex.sendResponseHeaders(code, bytes.length);
            try (OutputStream os = ex.getResponseBody()) {
                os.write(bytes);
            }
        });
        server.start();
        return "http://127.0.0.1:" + server.getAddress().getPort();
    }

    private static String flatten(Throwable t) {
        StringWriter sw = new StringWriter();
        t.printStackTrace(new PrintWriter(sw));
        return t + " " + t.getMessage() + " " + sw;
    }

    private static void assertClean(String label, String blob) {
        for (String canary : CANARIES) {
            assertFalse(blob.contains(canary), label + ": " + canary + " leaked in:\n" + blob.substring(0, Math.min(blob.length(), 1500)));
        }
    }

    @Test
    @DisplayName("no credential reaches the DEBUG log, an exception, or a toString — any route, any failure")
    void noCredentialReachesLogsExceptionsOrToString() throws Exception {
        for (String mode : new String[] {"401", "500", "400-json", "reply", "garbage"}) {
            for (String route : new String[] {"inline", "env"}) {
                String url = reflector(mode);
                ByteArrayOutputStream buf = new ByteArrayOutputStream();
                Handler h = QueryForgeLogging.configure(Level.FINE, new PrintStream(buf, true, "UTF-8"));
                List<String> seen = new ArrayList<>();
                try {
                    Map<String, Object> cfg = new LinkedHashMap<>(TestSupport.config());
                    Map<String, Object> model = new LinkedHashMap<>();
                    model.put("provider", "custom");
                    model.put("baseURL", url);
                    model.put("model", "m");
                    model.put("maxRetries", 0);
                    QueryForge forge;
                    if (route.equals("inline")) {
                        model.put("apiKey", KEY);
                        cfg.put("model", model);
                        forge = QueryForge.postgres(cfg);
                    } else {
                        model.put("apiKeyEnv", "QF_LEAKTEST_KEY");
                        cfg.put("model", model);
                        forge = QueryForge.postgres(cfg).withCredentials(Map.of("QF_LEAKTEST_KEY", ENV_KEY));
                    }
                    seen.add(forge.toString());
                    PendingQuery pending = forge.query("delivered orders");
                    seen.add(pending.toString());
                    try {
                        seen.add(String.valueOf(pending.result()));
                    } catch (QueryForgeException e) {
                        seen.add(flatten(e));
                    }
                } finally {
                    QueryForgeLogging.removeHandler(h);
                    server.stop(0);
                    server = null;
                }
                String blob = String.join("\n", seen) + "\n" + buf.toString("UTF-8");
                assertClean(route + "/" + mode, blob);
                assertTrue(buf.size() > 0, "expected DEBUG log lines to have been captured");
            }
        }
    }

    @Test
    @DisplayName("rejected configs never echo the key")
    void rejectedConfigsNeverEchoTheKey() {
        List<Map<String, Object>> bad = new ArrayList<>();
        Map<String, Object> both = new LinkedHashMap<>();
        both.put("apiKey", KEY);
        both.put("apiKeyEnv", "QF_API_KEY");
        bad.add(both);
        Map<String, Object> spaced = new LinkedHashMap<>();
        spaced.put("apiKey", KEY + " two words");
        bad.add(spaced);
        Map<String, Object> inEnv = new LinkedHashMap<>();
        inEnv.put("apiKeyEnv", KEY);
        bad.add(inEnv);
        for (Map<String, Object> extra : bad) {
            Map<String, Object> cfg = new LinkedHashMap<>(TestSupport.config());
            Map<String, Object> model = new LinkedHashMap<>();
            model.put("provider", "custom");
            model.put("baseURL", "http://127.0.0.1:1");
            model.putAll(extra);
            cfg.put("model", model);
            QueryForgeException e = assertThrows(
                    QueryForgeException.class, () -> QueryForge.postgres(cfg).generate(TestSupport.validAst()));
            assertClean("rejected config", flatten(e));
        }
    }

    @Test
    @DisplayName("a malformed config string never echoes a key-shaped token back")
    void malformedConfigStringDoesNotEchoTokens() {
        String[] broken = {
            "{\"entity\":\"Order\",\"model\":{\"apiKey\": 8675309-" + "LEAKCANARY9999 }}",
            "{\"entity\":\"Order\",\"model\":{\"apiKey\":\"" + KEY + "\" oops}}",
            "{\"entity\":\"Order\",\"model\":{\"apiKey\":" + KEY + "}}",
        };
        for (String text : broken) {
            QueryForgeException e = assertThrows(QueryForgeException.class, () -> QueryForge.parseConfig(text));
            assertClean("malformed config", flatten(e));
        }
    }
}
