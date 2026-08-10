package io.queryforge;

import java.util.List;

/**
 * The model answered, but never with usable JSON.
 *
 * <p>Usually transient. Retrying is reasonable; so is switching models.
 */
public class ModelOutputException extends QueryForgeException {

    private static final long serialVersionUID = 1L;

    ModelOutputException(String message, String code, List<Detail> details) {
        super(message, code, details);
    }
}
