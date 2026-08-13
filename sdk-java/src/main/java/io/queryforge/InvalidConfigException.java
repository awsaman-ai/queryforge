package io.queryforge;

import java.util.List;

/**
 * The config did not parse, or broke one of the engine's structural rules.
 */
public class InvalidConfigException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    InvalidConfigException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }

    /**
     * Preserves the underlying failure — an {@code IOException} from reading the file, or the JSON
     * parser's own complaint. Without it, "could not read the config" arrives with no indication of
     * whether the path was wrong, the permissions were wrong, or the disk was full.
     */
    InvalidConfigException(String message, String code, Throwable cause) {
        super(message, code, java.util.Collections.emptyList(), cause);
    }
}
