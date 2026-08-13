package io.queryforge;

import java.util.Collections;
import java.util.List;

/**
 * Base class for every failure this SDK reports.
 *
 * <p>It is <strong>unchecked</strong>. Checked exceptions would force a {@code try}/{@code catch}
 * around a fluent chain — {@code QueryForge.mysql(cfg).query(text).toSql()} — which is precisely
 * the ergonomics the SDK exists to provide. The failures here are also overwhelmingly of the kind
 * a caller handles once at a request boundary (a bad question, an unreachable model) rather than
 * at each call site.
 *
 * <p>Branch on the exception type for the common cases; {@link #getCode()} stays available for
 * anything finer, and is what keeps an error class introduced by a newer engine from disappearing
 * — it surfaces as this base class with the code intact.
 */
public class QueryForgeException extends RuntimeException {

    private static final long serialVersionUID = 1L;

    private final String code;

    // Declared as List rather than a concrete serializable type, which javac's [serial] lint
    // flags. The values actually stored are always JDK unmodifiable wrappers, which are
    // serializable, and Detail carries only Strings — so a serialized exception round-trips
    // fine. Narrowing the declared type to ArrayList to silence the warning would leak an
    // implementation detail into a field's type for no behavioural gain.
    @SuppressWarnings("serial")
    private final List<Detail> details;

    QueryForgeException(String message, String code, List<Detail> details) {
        this(message, code, details, null);
    }

    QueryForgeException(String message, String code) {
        this(message, code, Collections.emptyList(), null);
    }

    QueryForgeException(String message, String code, Throwable cause) {
        this(message, code, Collections.emptyList(), cause);
    }

    /**
     * The canonical constructor. Every other one delegates here.
     *
     * <p>It takes a cause as well as details because losing the cause is the specific defect this
     * signature was added to fix: {@code readConfig} used to catch an {@code IOException} and throw
     * an {@link InvalidConfigException} carrying only the message, so "permission denied" survived
     * as text while the stack trace that said <em>where</em> did not. A wrapped exception with no
     * cause turns a five-second diagnosis into a bisect.
     */
    QueryForgeException(String message, String code, List<Detail> details, Throwable cause) {
        super(message, cause);
        this.code = code == null ? "" : code;
        this.details = details == null ? Collections.emptyList() : Collections.unmodifiableList(details);
    }

    /** The stable protocol classification, e.g. {@code VALIDATION_FAILED}. Never null. */
    public String getCode() {
        return code;
    }

    /**
     * Structured findings, populated for a validation failure and empty otherwise.
     *
     * <p>This is what lets an application say "column 'agee' does not exist, did you mean 'age'?"
     * rather than reprinting one flattened sentence.
     */
    public List<Detail> getDetails() {
        return details;
    }

    @Override
    public String getMessage() {
        // The code is prefixed because these messages are most often read in a stack trace,
        // where the class alone does not say which of several codes mapped to it.
        String message = super.getMessage();
        return code.isEmpty() ? message : "[" + code + "] " + message;
    }

    /**
     * Builds the right subclass for a protocol error code.
     *
     * <p>An unrecognised code deliberately falls back to this base class rather than throwing:
     * an SDK talking to a newer engine must degrade, not crash.
     */
    static QueryForgeException fromCode(String code, String message, List<Detail> details) {
        String c = code == null ? "" : code;
        switch (c) {
            case "INVALID_REQUEST":
            case "UNKNOWN_OP":
                return new InvalidRequestException(message, c, details);
            case "INVALID_CONFIG":
                return new InvalidConfigException(message, c, details);
            case "UNKNOWN_BACKEND":
                return new UnknownBackendException(message, c, details);
            case "INVALID_SCOPE":
                return new InvalidScopeException(message, c, details);
            case "VALIDATION_FAILED":
                return new ValidationException(message, c, details);
            case "UNSUPPORTED_REQUEST":
                return new UnsupportedRequestException(message, c, details);
            case "MODEL_OUTPUT":
                return new ModelOutputException(message, c, details);
            case "MODEL_TRANSPORT":
                return new ModelTransportException(message, c, details);
            case "GENERATE_FAILED":
                return new GenerateException(message, c, details);
            case "TIMEOUT":
                return new TimeoutException(message, c, details);
            default:
                return new QueryForgeException(message, c, details);
        }
    }
}
