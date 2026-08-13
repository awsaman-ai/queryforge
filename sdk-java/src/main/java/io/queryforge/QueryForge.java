package io.queryforge;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Natural language to database queries, as a native Java library.
 *
 * <pre>{@code
 * QueryForge forge = QueryForge.mysql(Paths.get("orders.config.json"));
 * String sql = forge.query("delivered orders over $100 last month").toSql();
 * }</pre>
 *
 * <p>There is no server to run and no Go toolchain to install. The engine ships as a platform
 * binary inside this jar; the SDK extracts it, spawns it, hands it one JSON request, and turns the
 * reply into Java objects. Everything that decides what a query <em>means</em> — parsing, the AST,
 * validation, dialects, SQL and Mongo generation — lives in that engine, so this SDK and the
 * Python, Node and .NET ones all produce byte-identical output for the same input.
 *
 * <h2>The two halves of the API</h2>
 *
 * <dl>
 *   <dt>{@link #query(String)}</dt>
 *   <dd>The full pipeline. Costs one model call, so it needs a {@code model} block in the config
 *       and the API key exported under the name that block's {@code apiKeyEnv} gives.</dd>
 *   <dt>{@link #generate(Map)} / {@link #validate(Map)}</dt>
 *   <dd>Deterministic. No model call, no network, no API key. Use these to re-compile a stored AST
 *       for a second backend, and in tests.</dd>
 * </dl>
 *
 * <p>Instances are immutable and thread-safe. Building one costs nothing — the executable is not
 * started until a query is actually compiled.
 */
public final class QueryForge {

    /** Backend ids the shipped engine registers. */
    private static final List<String> BACKENDS = Collections.unmodifiableList(
            Arrays.asList("sql", "mysql", "mongo"));

    private final Map<String, Object> config;
    private final String backend;
    private final Map<String, Object> scope;
    private final Map<String, Object> options;

    private QueryForge(
            Map<String, Object> config,
            String backend,
            Map<String, Object> scope,
            Map<String, Object> options) {
        this.config = config;
        this.backend = backend;
        this.scope = scope;
        this.options = options;
    }

    // ------------------------------------------------------------ factories

    /** Binds to the MySQL dialect ({@code ?} placeholders, backtick quoting). */
    public static QueryForge mysql(Map<String, Object> config) {
        return create(config, "mysql");
    }

    /** Binds to the MySQL dialect, reading the config from a JSON file. */
    public static QueryForge mysql(Path configPath) {
        return create(readConfig(configPath), "mysql");
    }

    /** Binds to the PostgreSQL dialect ({@code $1} placeholders). */
    public static QueryForge postgres(Map<String, Object> config) {
        return create(config, "sql");
    }

    /** Binds to the PostgreSQL dialect, reading the config from a JSON file. */
    public static QueryForge postgres(Path configPath) {
        return create(readConfig(configPath), "sql");
    }

    /**
     * Binds to the PostgreSQL dialect.
     *
     * <p>{@code sql} is the engine's own id for that dialect; both names are offered because the
     * config files say {@code sql} while callers think in terms of the database they are pointed
     * at.
     */
    public static QueryForge sql(Map<String, Object> config) {
        return postgres(config);
    }

    /** Binds to the PostgreSQL dialect, reading the config from a JSON file. */
    public static QueryForge sql(Path configPath) {
        return postgres(configPath);
    }

    /** Binds to MongoDB, producing a query document rather than SQL. */
    public static QueryForge mongo(Map<String, Object> config) {
        return create(config, "mongo");
    }

    /** Binds to MongoDB, reading the config from a JSON file. */
    public static QueryForge mongo(Path configPath) {
        return create(readConfig(configPath), "mongo");
    }

    /**
     * Binds to an explicitly named backend.
     *
     * <p>For a custom generator registered in a build of the engine that this SDK does not know
     * about. Prefer the named factories.
     */
    public static QueryForge forBackend(Map<String, Object> config, String backend) {
        return create(config, backend);
    }

    private static QueryForge create(Map<String, Object> config, String backend) {
        if (config == null) {
            throw new InvalidConfigException(
                    "config must not be null", "INVALID_CONFIG", Collections.emptyList());
        }
        if (backend == null || backend.trim().isEmpty()) {
            throw new UnknownBackendException(
                    "backend must not be empty. Registered: " + String.join(", ", BACKENDS),
                    "UNKNOWN_BACKEND",
                    Collections.emptyList());
        }
        // Copied, not aliased: mutating the caller's map after construction must not change what
        // a later query compiles against.
        return new QueryForge(
                Collections.unmodifiableMap(new LinkedHashMap<>(config)),
                backend,
                Collections.emptyMap(),
                Collections.emptyMap());
    }

    // ------------------------------------------------------ instance config

    /**
     * Returns a copy with filters applied to every query it produces.
     *
     * <p>Use this for scope that is fixed for the lifetime of a request — the tenant a web request
     * belongs to, say — rather than repeating it on each query.
     */
    public QueryForge withScope(Map<String, ?> scope) {
        if (scope == null || scope.isEmpty()) {
            return this;
        }
        Map<String, Object> merged = new LinkedHashMap<>(this.scope);
        merged.putAll(scope);
        return new QueryForge(config, backend, Collections.unmodifiableMap(merged), options);
    }

    /** Returns a copy whose queries are bounded by this deadline unless overridden per query. */
    public QueryForge withTimeout(long millis) {
        if (millis <= 0) {
            throw new InvalidRequestException(
                    "timeout must be positive, got " + millis, "INVALID_REQUEST", Collections.emptyList());
        }
        Map<String, Object> next = new LinkedHashMap<>(options);
        next.put("timeoutMs", (int) millis);
        return new QueryForge(config, backend, scope, Collections.unmodifiableMap(next));
    }

    /** The backend id this instance compiles to. */
    public String getBackend() {
        return backend;
    }

    // ------------------------------------------------------- the model path

    /**
     * Describes a question. Nothing runs until a terminal method is called on the result.
     *
     * <p>Costs one model call (more if a repair is needed) when it does run.
     */
    public PendingQuery query(String text) {
        if (text == null || text.trim().isEmpty()) {
            throw new InvalidRequestException(
                    "query text must be a non-empty string", "INVALID_REQUEST", Collections.emptyList());
        }
        return new PendingQuery(this, text, scope, options);
    }

    // ----------------------------------------------- the deterministic path

    /**
     * Compiles a pre-built AST. No model call, no network, no API key.
     *
     * <p>The AST is validated against the config first, so an AST from an untrusted source cannot
     * compile to something the config forbids.
     */
    public QueryForgeResult generate(Map<String, Object> ast) {
        return generate(ast, scope);
    }

    /** Compiles a pre-built AST with an explicit scope, replacing this instance's. */
    public QueryForgeResult generate(Map<String, Object> ast, Map<String, ?> scope) {
        if (ast == null) {
            throw new InvalidRequestException(
                    "ast must not be null", "INVALID_REQUEST", Collections.emptyList());
        }
        Map<String, Object> request = baseRequest("generate", scope, options);
        request.put("ast", ast);
        return QueryForgeResult.fromJson(Transport.run(request, timeoutOf(options)));
    }

    /**
     * Checks an AST against the config without compiling it.
     *
     * @throws ValidationException with a populated {@link QueryForgeException#getDetails()} when
     *     the AST is not legal
     */
    public QueryForgeResult validate(Map<String, Object> ast) {
        if (ast == null) {
            throw new InvalidRequestException(
                    "ast must not be null", "INVALID_REQUEST", Collections.emptyList());
        }
        Map<String, Object> request = baseRequest("validate", Collections.emptyMap(), options);
        request.put("ast", ast);
        return QueryForgeResult.fromJson(Transport.run(request, timeoutOf(options)));
    }

    // ------------------------------------------------------------ internals

    /** Runs a translation. Called by {@link PendingQuery} on its first terminal call. */
    QueryForgeResult translate(String text, Map<String, Object> scope, Map<String, Object> options) {
        return translate(text, scope, options, null);
    }

    /** As above, with the caller's correlation id. See {@link PendingQuery#requestId(String)}. */
    QueryForgeResult translate(
            String text, Map<String, Object> scope, Map<String, Object> options, String requestId) {
        Map<String, Object> request = baseRequest("translate", scope, options);
        request.put("query", text);
        return QueryForgeResult.fromJson(Transport.run(request, timeoutOf(options), requestId));
    }

    private Map<String, Object> baseRequest(
            String op, Map<String, ?> scope, Map<String, Object> options) {
        Map<String, Object> request = new LinkedHashMap<>();
        request.put("op", op);
        request.put("backend", backend);
        request.put("config", config);
        Map<String, ?> effective = (scope == null || scope.isEmpty()) ? this.scope : scope;
        if (effective != null && !effective.isEmpty()) {
            request.put("scope", effective);
        }
        if (options != null && !options.isEmpty()) {
            request.put("options", options);
        }
        return request;
    }

    private static Long timeoutOf(Map<String, Object> options) {
        Object ms = options.get("timeoutMs");
        return ms instanceof Number ? ((Number) ms).longValue() : null;
    }

    /** Reads and parses a JSON config file. */
    private static Map<String, Object> readConfig(Path path) {
        if (path == null) {
            throw new InvalidConfigException(
                    "config path must not be null", "INVALID_CONFIG", Collections.emptyList());
        }
        String text;
        try {
            text = new String(Files.readAllBytes(path), StandardCharsets.UTF_8);
        } catch (IOException e) {
            // The cause is chained, not dropped. "Could not read the config" on its own does not
            // say whether the path was wrong, the permissions were wrong, or the disk was full,
            // and those have three different fixes.
            throw new InvalidConfigException(
                    "Could not read the config file '" + path + "': " + e.getMessage(),
                    "INVALID_CONFIG",
                    e);
        }
        try {
            return Json.readObject(text);
        } catch (Json.JsonException e) {
            throw new InvalidConfigException(
                    "The config file '" + path + "' is not valid JSON: " + e.getMessage(),
                    "INVALID_CONFIG",
                    e);
        }
    }

    /**
     * Parses a JSON config from a string.
     *
     * <p>Offered as an explicit method rather than another overload of the factories, because a
     * {@code String} overload alongside a {@code Path} one would make {@code QueryForge.mysql(s)}
     * ambiguous to read at the call site: is {@code s} a path or the config itself?
     */
    public static Map<String, Object> parseConfig(String json) {
        if (json == null) {
            throw new InvalidConfigException(
                    "config JSON must not be null", "INVALID_CONFIG", Collections.emptyList());
        }
        try {
            return Json.readObject(json);
        } catch (Json.JsonException e) {
            throw new InvalidConfigException(
                    "The config string is not valid JSON: " + e.getMessage(), "INVALID_CONFIG", e);
        }
    }

    /**
     * Returns the bundled engine's version, protocol version and registered backends.
     *
     * <p>Useful as an installation check: it exercises binary extraction, process spawning and
     * JSON decoding without needing a config or an API key.
     */
    public static Map<String, Object> engineVersion() {
        return Transport.run(Collections.singletonMap("op", "version"), null);
    }

    /** Returns the path of the executable this SDK will run, extracting it if necessary. */
    public static String binaryPath() {
        return BinaryResolver.resolve().toString();
    }

    /** Returns the {@code <os>-<arch>} tag this JVM resolves to. */
    public static String platformTag() {
        return BinaryResolver.platformTag();
    }

    @Override
    public String toString() {
        return "QueryForge{entity=" + config.get("entity") + ", backend=" + backend + "}";
    }
}
