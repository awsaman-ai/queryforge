package io.queryforge;

import java.util.List;

/**
 * The model declined: the question cannot be expressed in this config's vocabulary.
 *
 * <p>This is a well-formed answer rather than a failure of the pipeline. The message is written
 * to be shown to the person who asked.
 */
public class UnsupportedRequestException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    UnsupportedRequestException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
