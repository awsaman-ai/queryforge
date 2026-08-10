package io.queryforge;

import java.util.List;

/**
 * A caller-supplied scope filter was rejected.
 *
 * <p>Scope comes from the application session (tenant, subscription, user), never from the end
 * user's question, so this is an application bug rather than something to show a user.
 */
public class InvalidScopeException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    InvalidScopeException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
