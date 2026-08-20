package io.queryforge;

import java.io.PrintStream;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.logging.ConsoleHandler;
import java.util.logging.Formatter;
import java.util.logging.Handler;
import java.util.logging.Level;
import java.util.logging.LogRecord;
import java.util.logging.Logger;
import java.util.logging.StreamHandler;

/**
 * Structured logging for the QueryForge Java SDK.
 *
 * <h2>Why {@code java.util.logging} and not SLF4J</h2>
 *
 * <p>This SDK has <strong>no runtime dependencies</strong>, deliberately and as documented in its
 * pom: a thin wrapper that drags in a logging facade forces that facade's version on every
 * application using it, and a version conflict inside someone else's Spring app is a worse
 * experience than the small amount of plumbing below. {@code java.util.logging} is in the JDK, so
 * it costs nothing to depend on. It is the same choice the JDK's own {@code HttpClient} makes.
 *
 * <p>That is not a worse outcome for a host on Logback or Log4j, because JUL bridges cleanly. Add
 * the bridge you already have and QueryForge's records arrive in your pipeline with their fields
 * intact:
 *
 * <pre>{@code
 * // SLF4J / Logback
 * <dependency>org.slf4j:jul-to-slf4j</dependency>
 * SLF4JBridgeHandler.removeHandlersForRootLogger();
 * SLF4JBridgeHandler.install();
 *
 * // Log4j 2
 * -Djava.util.logging.manager=org.apache.logging.log4j.jul.LogManager
 * }</pre>
 *
 * <h2>What the SDK does and does not do</h2>
 *
 * <p>It obtains one logger, {@code io.queryforge}, and emits to it. It installs no handler, never
 * touches the root logger, and never reads a logging config file. Out of the box the SDK is silent
 * beyond whatever your JVM's default handler already does with INFO records.
 *
 * <p>Turn it on the way you turn on any other library:
 *
 * <pre>{@code
 * Logger.getLogger(QueryForgeLogging.LOGGER_NAME).setLevel(Level.FINE);
 *
 * // or, for JSON matching the engine's own field names:
 * QueryForgeLogging.configure(Level.INFO);
 * }</pre>
 *
 * <h2>Levels</h2>
 *
 * <p>JUL's level names differ from everyone else's, so the mapping is spelled out. The semantics
 * are identical across the Go, Python and Java surfaces.
 *
 * <table border="1">
 *   <caption>Level mapping</caption>
 *   <tr><th>Meaning</th><th>JUL</th><th>What it covers</th></tr>
 *   <tr><td>DEBUG</td><td>{@code FINE}</td><td>Binary resolution, a request being sent.</td></tr>
 *   <tr><td>INFO</td><td>{@code INFO}</td>
 *       <td>An operation completed, or was refused by the model. A refusal is INFO on purpose — it
 *           means the guard rails worked.</td></tr>
 *   <tr><td>WARN</td><td>{@code WARNING}</td><td>A step failed but the operation continues.</td></tr>
 *   <tr><td>ERROR</td><td>{@code SEVERE}</td>
 *       <td>The caller is receiving an exception. Exactly one per failed call.</td></tr>
 * </table>
 *
 * <h2>Privacy</h2>
 *
 * <p>The natural-language question, the scope <em>values</em> (tenant, user and subscription ids)
 * and the config contents are never logged, at any level. What is logged is shape: the entity, the
 * backend, the scope <em>keys</em>. Text of unknown provenance — chiefly the engine's stderr — is
 * scrubbed by {@link #redact(String)} before it reaches a record or an exception message.
 */
public final class QueryForgeLogging {

    /** The logger namespace. Configure this name to reach everything the SDK emits. */
    public static final String LOGGER_NAME = "io.queryforge";

    /**
     * Environment variable that turns SDK logging on without a code change.
     *
     * <p>Same spelling the engine binary honours, so one variable lights up both halves of a call.
     * Accepts {@code off | error | warn | info | debug}; an unrecognised value is reported on
     * stderr and ignored rather than thrown, because failing here would mean a typo in an
     * environment variable stops an application from loading a class.
     */
    public static final String LOG_LEVEL_ENV_VAR = "QUERYFORGE_LOG_LEVEL";

    /** System property equivalent of {@link #LOG_LEVEL_ENV_VAR}, and the one that wins. */
    public static final String LOG_LEVEL_PROPERTY = "queryforge.logLevel";

    // ------------------------------------------------------------ field names
    //
    // A cross-language contract, not an implementation detail: the Go engine and the Python SDK
    // emit the same names, so one saved search in a log aggregator works whichever surface
    // produced the line. Renaming one breaks somebody's dashboard.

    static final String FIELD_LIBRARY = "library";
    static final String FIELD_LANGUAGE = "language";
    static final String FIELD_VERSION = "version";
    static final String FIELD_COMPONENT = "component";
    static final String FIELD_OPERATION = "operation";
    static final String FIELD_OUTCOME = "outcome";
    static final String FIELD_BACKEND = "backend";
    static final String FIELD_ENTITY = "entity";
    static final String FIELD_REQUEST_ID = "request_id";
    static final String FIELD_DURATION_MS = "duration_ms";
    static final String FIELD_ERROR_CODE = "error_code";
    static final String FIELD_ERROR_TYPE = "error_type";
    static final String FIELD_SCOPE_KEYS = "scope_keys";

    static final String LIBRARY_NAME = "queryforge";
    static final String LANGUAGE_NAME = "java";

    /** The SDK's version, reported on every record. Kept in step with the pom by a test. */
    static final String SDK_VERSION = "1.2.0";

    private static final Logger ROOT = Logger.getLogger(LOGGER_NAME);

    /**
     * The level the SDK installs on its own logger when the host has not chosen one.
     *
     * <p>JUL's default threshold is INFO, and the JVM's default console handler prints it. Left
     * alone, that would put one line on every application's console for every query — a behaviour
     * change for every existing user of this SDK, imposed by a release they only wanted a bug fix
     * from. WARNING keeps the default quiet while leaving genuine problems visible.
     *
     * <p>This is the one level the SDK sets without being asked, and it only LOWERS verbosity. Any
     * explicit configuration — a level on {@code io.queryforge}, a handler, a {@code logging
     * .properties} entry, {@link #configure} — overrides it.
     */
    private static final Level DEFAULT_LEVEL = Level.WARNING;

    static {
        applyEnvironmentLevel();
        if (ROOT.getLevel() == null) {
            // Only when nothing has set one. A logging.properties entry is applied by the
            // LogManager before this class loads, and must win.
            ROOT.setLevel(DEFAULT_LEVEL);
        }
    }

    private QueryForgeLogging() {}

    // ----------------------------------------------------------------- levels

    /**
     * Maps a level name onto its JUL level.
     *
     * @throws IllegalArgumentException for anything unrecognised. That is deliberate and matches
     *     the engine: both plausible defaults are harmful. Falling back to "off" hides diagnostics
     *     from someone who explicitly asked for them and will conclude the feature is broken;
     *     falling back to "debug" starts writing detail a typo did not authorise.
     */
    public static Level parseLevel(String name) {
        String key = name == null ? "" : name.trim().toLowerCase(java.util.Locale.ROOT);
        switch (key) {
            case "off":
            case "none":
            case "silent":
                return Level.OFF;
            case "error":
            case "severe":
                return Level.SEVERE;
            case "warn":
            case "warning":
                return Level.WARNING;
            case "info":
                return Level.INFO;
            case "debug":
            case "fine":
                return Level.FINE;
            default:
                throw new IllegalArgumentException(
                        "unknown log level '" + name + "' (expected one of: off, error, warn, info, debug)");
        }
    }

    /**
     * Renders a JUL level as the name the engine binary understands.
     *
     * <p>Rounds towards MORE detail, because a caller who set FINER did not ask for less than FINE
     * and silently dropping their records would be the surprising answer.
     */
    static String engineLevelName(Level level) {
        if (level == null || level.intValue() == Level.OFF.intValue()) {
            return "off";
        }
        int value = level.intValue();
        if (value <= Level.FINE.intValue()) {
            return "debug";
        }
        if (value <= Level.INFO.intValue()) {
            return "info";
        }
        if (value <= Level.WARNING.intValue()) {
            return "warn";
        }
        return "error";
    }

    /**
     * Reports whether the host has deliberately asked for QueryForge diagnostics.
     *
     * <p>Two things count, and both name the {@code io.queryforge} logger specifically:
     *
     * <ul>
     *   <li>a handler attached to it — which is what {@link #configure} does; or
     *   <li>a level that admits INFO or finer. The SDK's own {@link #DEFAULT_LEVEL} is WARNING, so
     *       an untouched logger answers no, and a host raising it to INFO or FINE answers yes.
     * </ul>
     *
     * <p>Inheriting a level from the root logger deliberately does not count on its own: a host
     * turning FINE on globally has made a statement about their own code, not asked a subprocess to
     * start producing diagnostics — and acting on it would spawn every engine in debug mode for
     * anyone who ever enabled tracing anywhere. (The SDK's WARNING floor also stops that from
     * happening by accident.)
     *
     * <p>A host who wants engine logs at WARNING only — an unusual but legitimate ask — gets them
     * by calling {@link #configure(Level)}, which attaches a handler and so satisfies the first
     * condition regardless of level.
     */
    static boolean isConfigured() {
        return ROOT.getHandlers().length > 0 || ROOT.isLoggable(Level.INFO);
    }

    /**
     * Returns the level to ask the engine for, or {@code null} to leave it alone.
     *
     * <p>{@code null} — the default — means the SDK sends no {@code logLevel}, keeping the request
     * BYTE-IDENTICAL to a protocol-1.0 one. That matters more than it looks: the engine rejects
     * unknown request fields, so an SDK that always sent the field would turn every call into
     * INVALID_REQUEST for anyone pointing {@code QUERYFORGE_BINARY} at an older build.
     */
    static String engineLevel() {
        if (!isConfigured()) {
            return null;
        }
        String name = engineLevelName(effectiveLevel());
        return "off".equals(name) ? null : name;
    }

    /** Walks up to the first ancestor with an explicit level, as JUL itself does. */
    private static Level effectiveLevel() {
        for (Logger l = ROOT; l != null; l = l.getParent()) {
            if (l.getLevel() != null) {
                return l.getLevel();
            }
        }
        return Level.INFO;
    }

    // ------------------------------------------------------------- emission

    /** Returns the logger for one component, e.g. {@code io.queryforge.transport}. */
    static Logger logger(String component) {
        return component == null ? ROOT : Logger.getLogger(LOGGER_NAME + "." + component);
    }

    /**
     * Emits one structured record.
     *
     * <p>{@code message} is a fixed string with nothing interpolated into it — that is what lets an
     * aggregator count occurrences of "engine request failed" without the count fragmenting across
     * every distinct value that ever appeared in the text. Everything variable goes in
     * {@code fields}.
     *
     * <p>JUL has no key/value channel, so the fields travel two ways at once. They are appended to
     * the rendered message as {@code key=value} pairs, which is what a plain
     * {@code SimpleFormatter} will show; and they are attached verbatim as the record's
     * <em>parameters</em>, so {@link JsonFormatter}, a JUL-to-SLF4J bridge, or any custom handler
     * can read the map rather than parse a sentence.
     *
     * <p>Entries with a null value are dropped rather than rendered as "null", so a record's key
     * set says which facts were actually known.
     */
    static void log(Logger logger, Level level, String message, Map<String, Object> fields) {
        log(logger, level, message, fields, null);
    }

    /** As {@link #log(Logger, Level, String, Map)}, attaching a throwable. */
    static void log(
            Logger logger, Level level, String message, Map<String, Object> fields, Throwable thrown) {
        if (!logger.isLoggable(level)) {
            // Checked explicitly so the map below is not rendered for a record that will be
            // discarded. The default configuration discards most of them.
            return;
        }

        Map<String, Object> payload = new LinkedHashMap<>();
        payload.put(FIELD_LIBRARY, LIBRARY_NAME);
        payload.put(FIELD_LANGUAGE, LANGUAGE_NAME);
        payload.put(FIELD_VERSION, SDK_VERSION);
        if (fields != null) {
            for (Map.Entry<String, Object> entry : fields.entrySet()) {
                if (entry.getValue() != null) {
                    payload.put(entry.getKey(), entry.getValue());
                }
            }
        }

        LogRecord record = new LogRecord(level, message + " " + render(payload));
        record.setLoggerName(logger.getName());
        record.setParameters(new Object[] {payload});
        if (thrown != null) {
            record.setThrown(thrown);
        }
        logger.log(record);
    }

    /** Renders the field map as {@code key=value} pairs for the message text. */
    private static String render(Map<String, Object> fields) {
        StringBuilder sb = new StringBuilder();
        for (Map.Entry<String, Object> entry : fields.entrySet()) {
            if (sb.length() > 0) {
                sb.append(' ');
            }
            sb.append(entry.getKey()).append('=').append(entry.getValue());
        }
        return sb.toString();
    }

    /** Reads the structured fields back off a record, or an empty map if there are none. */
    @SuppressWarnings("unchecked")
    static Map<String, Object> fieldsOf(LogRecord record) {
        Object[] params = record.getParameters();
        if (params != null && params.length == 1 && params[0] instanceof Map) {
            return (Map<String, Object>) params[0];
        }
        return java.util.Collections.emptyMap();
    }

    // ----------------------------------------------------------- redaction

    /** Bound on any excerpt of text the SDK did not author. */
    static final int MAX_EXCERPT = 500;

    /**
     * Patterns for secrets that can turn up inside text the SDK did not author — chiefly the
     * engine's stderr, which is quoted verbatim into a {@link ProtocolException} when the
     * subprocess dies before answering.
     *
     * <p>This is a second line of defence, not the first. The first is that nothing here ever puts
     * a key into a message deliberately; QueryForge reads API keys from the environment and passes
     * them in an HTTP header. But a crashing engine can print anything, and "anything" heading into
     * a log aggregator is worth scrubbing.
     */
    private static final java.util.regex.Pattern[] SECRET_PATTERNS = {
        java.util.regex.Pattern.compile("(?i)\\b(bearer)\\s+[A-Za-z0-9._\\-+/=]{8,}"),
        java.util.regex.Pattern.compile(
                "(?i)\\b([a-z_\\-]*(?:api[_\\-]?key|secret|password|passwd|token|credential)[a-z_\\-]*)"
                        + "(\\s*[:=]\\s*)(\"[^\"]*\"|'[^']*'|[^\\s,;&)}\\]]+)"),
        java.util.regex.Pattern.compile("(?i)\\b([a-z][a-z0-9+.\\-]*://)([^\\s/:@]+):([^\\s/@]+)@"),
        java.util.regex.Pattern.compile("\\bsk-[A-Za-z0-9_\\-]{16,}"),
        java.util.regex.Pattern.compile("\\bAIza[A-Za-z0-9_\\-]{20,}"),
    };

    private static final String[] SECRET_REPLACEMENTS = {
        "$1 [REDACTED]", "$1$2[REDACTED]", "$1$2:[REDACTED]@", "[REDACTED]", "[REDACTED]",
    };

    /**
     * Scrubs credentials from text of unknown provenance and bounds its length.
     *
     * <p>Truncation is announced rather than silent: a reader has to be able to tell "the engine
     * said this" from "the engine said this and 900 KB more", or they will go hunting for a bug in
     * text that was never the whole story.
     */
    public static String redact(String text) {
        return redact(text, MAX_EXCERPT);
    }

    /** As {@link #redact(String)} with an explicit bound; a negative limit disables truncation. */
    public static String redact(String text, int limit) {
        if (text == null || text.isEmpty()) {
            return text;
        }
        String out = text;
        for (int i = 0; i < SECRET_PATTERNS.length; i++) {
            out = SECRET_PATTERNS[i].matcher(out).replaceAll(SECRET_REPLACEMENTS[i]);
        }
        if (limit >= 0 && out.length() > limit) {
            int dropped = out.length() - limit;
            out = out.substring(0, limit) + "… (" + dropped + " bytes truncated)";
        }
        return out;
    }

    /**
     * Returns a scope's field NAMES, sorted.
     *
     * <p>Names only, never values. Scope values are tenant, subscription, user and enterprise ids —
     * the most sensitive thing this SDK handles — while the names are exactly what an audit trail
     * needs in order to say which filters were forced onto a query.
     */
    static java.util.List<String> scopeKeys(Map<String, ?> scope) {
        if (scope == null || scope.isEmpty()) {
            return java.util.Collections.emptyList();
        }
        java.util.List<String> keys = new java.util.ArrayList<>(scope.keySet());
        java.util.Collections.sort(keys);
        return keys;
    }

    // -------------------------------------------------- correlation ids

    /**
     * Returns a short correlation id for one SDK call.
     *
     * <p>Twelve hex characters rather than a full UUID: this id exists to tie a handful of log
     * lines together, not to be globally unique forever, and a shorter string is markedly easier to
     * eyeball in a terminal. A caller who already has a trace id should pass their own — see
     * {@link PendingQuery#requestId(String)}.
     */
    static String newRequestId() {
        return java.util.UUID.randomUUID().toString().replace("-", "").substring(0, 12);
    }

    // ------------------------------------------------ optional host helpers

    /**
     * A JUL formatter rendering one JSON object per record.
     *
     * <p>Offered because a structured logger whose fields you have to write a formatter to see is
     * not much use, and because the JSON it emits matches the engine's field-for-field. Entirely
     * optional: attach it yourself, or ignore it and use your own.
     */
    public static final class JsonFormatter extends Formatter {
        @Override
        public String format(LogRecord record) {
            StringBuilder sb = new StringBuilder(256);
            sb.append('{');
            appendString(sb, "time", java.time.Instant.ofEpochMilli(record.getMillis()).toString());
            sb.append(',');
            appendString(sb, "level", levelName(record.getLevel()));
            sb.append(',');
            appendString(sb, "msg", baseMessage(record));
            sb.append(',');
            appendString(sb, "logger", String.valueOf(record.getLoggerName()));
            for (Map.Entry<String, Object> entry : fieldsOf(record).entrySet()) {
                sb.append(',');
                appendValue(sb, entry.getKey(), entry.getValue());
            }
            if (record.getThrown() != null) {
                sb.append(',');
                appendString(sb, "exception", stackTrace(record.getThrown()));
            }
            sb.append("}\n");
            return sb.toString();
        }

        /**
         * Strips the {@code key=value} tail that {@link #log} appends for text formatters, so the
         * JSON carries the stable message and the fields once each rather than twice.
         */
        private static String baseMessage(LogRecord record) {
            String message = record.getMessage();
            Map<String, Object> fields = fieldsOf(record);
            if (message == null || fields.isEmpty()) {
                return String.valueOf(message);
            }
            int cut = message.indexOf(" " + FIELD_LIBRARY + "=");
            return cut < 0 ? message : message.substring(0, cut);
        }

        /** Renders JUL's names as the four everyone else uses. */
        private static String levelName(Level level) {
            int value = level.intValue();
            if (value >= Level.SEVERE.intValue()) {
                return "ERROR";
            }
            if (value >= Level.WARNING.intValue()) {
                return "WARN";
            }
            if (value >= Level.INFO.intValue()) {
                return "INFO";
            }
            return "DEBUG";
        }

        private static String stackTrace(Throwable t) {
            java.io.StringWriter w = new java.io.StringWriter();
            t.printStackTrace(new java.io.PrintWriter(w));
            return w.toString();
        }

        private static void appendValue(StringBuilder sb, String key, Object value) {
            if (value instanceof Number || value instanceof Boolean) {
                sb.append('"').append(escape(key)).append("\":").append(value);
            } else if (value instanceof Iterable) {
                sb.append('"').append(escape(key)).append("\":[");
                boolean first = true;
                for (Object item : (Iterable<?>) value) {
                    if (!first) {
                        sb.append(',');
                    }
                    first = false;
                    sb.append('"').append(escape(String.valueOf(item))).append('"');
                }
                sb.append(']');
            } else {
                appendString(sb, key, String.valueOf(value));
            }
        }

        private static void appendString(StringBuilder sb, String key, String value) {
            sb.append('"').append(escape(key)).append("\":\"").append(escape(value)).append('"');
        }

        private static String escape(String s) {
            StringBuilder out = new StringBuilder(s.length() + 8);
            for (int i = 0; i < s.length(); i++) {
                char c = s.charAt(i);
                switch (c) {
                    case '"': out.append("\\\""); break;
                    case '\\': out.append("\\\\"); break;
                    case '\n': out.append("\\n"); break;
                    case '\r': out.append("\\r"); break;
                    case '\t': out.append("\\t"); break;
                    default:
                        if (c < 0x20) {
                            out.append(String.format("\\u%04x", (int) c));
                        } else {
                            out.append(c);
                        }
                }
            }
            return out.toString();
        }
    }

    /**
     * Attaches a JSON handler to the QueryForge logger. <strong>Opt-in only.</strong>
     *
     * <p>A library must not configure logging on its own initiative, and this method never runs
     * unless you call it. When you do, it touches nothing but the {@code io.queryforge} logger: the
     * root logger, other libraries' loggers and any handler you already installed are all left
     * exactly as they were.
     *
     * <p>Parent handlers are switched off, so calling this alongside the JVM's default console
     * handler gives you one JSON line per event rather than a JSON line and a plain-text duplicate.
     *
     * @return the handler, so you can remove it again
     */
    public static Handler configure(Level level) {
        return configure(level, null);
    }

    /** As {@link #configure(Level)}, writing to a specific stream. */
    public static Handler configure(Level level, PrintStream stream) {
        Handler handler = stream == null ? new ConsoleHandler() : new StreamHandler(stream, new JsonFormatter()) {
            @Override
            public synchronized void publish(LogRecord record) {
                super.publish(record);
                flush(); // a StreamHandler buffers; a test reading the stream needs it out now
            }
        };
        handler.setFormatter(new JsonFormatter());
        handler.setLevel(level);
        ROOT.addHandler(handler);
        ROOT.setLevel(level);
        ROOT.setUseParentHandlers(false);
        return handler;
    }

    /** Removes a handler installed by {@link #configure}. */
    public static void removeHandler(Handler handler) {
        ROOT.removeHandler(handler);
    }

    /**
     * Honours {@link #LOG_LEVEL_PROPERTY} / {@link #LOG_LEVEL_ENV_VAR} at class load, if set.
     *
     * <p>This is the one place the SDK configures anything without an explicit call, and it is
     * gated on a variable that only exists because someone set it deliberately. The case it serves
     * is real and not otherwise reachable: an operator debugging a running deployment cannot edit
     * the application's code to add a {@code configure} call, and QueryForge is usually several
     * layers down inside somebody else's service.
     *
     * <p>An unrecognised value is reported on stderr and then IGNORED. Throwing here would surface
     * as an {@link ExceptionInInitializerError} on first use — a typo in an environment variable
     * would stop the application from loading a class, which is a far worse outcome than logging
     * staying off.
     */
    private static void applyEnvironmentLevel() {
        String raw = System.getProperty(LOG_LEVEL_PROPERTY);
        if (raw == null || raw.trim().isEmpty()) {
            raw = System.getenv(LOG_LEVEL_ENV_VAR);
        }
        if (raw == null || raw.trim().isEmpty()) {
            return;
        }
        Level level;
        try {
            level = parseLevel(raw);
        } catch (IllegalArgumentException e) {
            System.err.println("queryforge: ignoring " + LOG_LEVEL_ENV_VAR + ": " + e.getMessage());
            return;
        }
        if (level.intValue() == Level.OFF.intValue()) {
            return;
        }
        configure(level);
    }
}
