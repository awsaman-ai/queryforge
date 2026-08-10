package io.queryforge;

import java.util.List;

/**
 * The request was malformed, or a required argument was missing.
 *
 * <p>Always a bug in the calling code — retrying will not help.
 */
public class InvalidRequestException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    InvalidRequestException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
