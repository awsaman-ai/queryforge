package io.queryforge;

import java.util.List;

/**
 * An AST broke a rule the config declares.
 *
 * <p>On {@link QueryForge#generate} that is the AST you supplied. On a translation it means the
 * model could not produce a conforming AST within the repair budget — usually because the config
 * does not register a field the question needs.
 *
 * <p>{@link #getDetails()} lists every finding, each with the offending field and, for a
 * misspelling, the nearest matches.
 */
public class ValidationException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    ValidationException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
